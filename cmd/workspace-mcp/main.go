// Command shim is the standalone MCP server: it exposes one or more local
// directory trees (workspaces) to claude.ai over Streamable HTTP, read-only,
// behind a bearer token, sandboxed per-workspace via os.Root. It can also run as
// a local stdio MCP server (-stdio) for tools like the MCP Inspector.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/mnehpets/http/endpoint"
	"github.com/mnehpets/http/jsonrpc"
	"golang.ngrok.com/ngrok"
	ngrokconfig "golang.ngrok.com/ngrok/config"

	"github.com/mnehpets/workspace-mcp/mcp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "./config.yaml", "path to YAML config")
	envPath := flag.String("env", "./secrets.env", "path to dotenv secrets file")
	stdio := flag.Bool("stdio", false, "run as a local stdio MCP server (no HTTP, no bearer auth)")
	flag.Parse()

	cfg, err := mcp.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	env, err := mcp.LoadEnv(*envPath)
	if err != nil {
		return err
	}

	// In stdio mode the transport is a trusted local pipe, so bearer tokens are
	// neither resolved nor required.
	var bearerTokens []string
	var oauthServer *mcp.OAuthServer
	if !*stdio {
		bearerTokens, err = cfg.ResolveBearerTokens(env)
		if err != nil {
			return fmt.Errorf("resolve bearer token: %w", err)
		}
		if cfg.Auth.OAuth.Enabled() {
			clientID, clientSecret, err := cfg.ResolveOAuthCredentials(env)
			if err != nil {
				return fmt.Errorf("resolve oauth credentials: %w", err)
			}
			oauthServer = mcp.NewOAuthServer(clientID, clientSecret)
		}
	}
	if err := cfg.Validate(bearerTokens, !*stdio); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// All logging goes to stderr; in stdio mode stdout is reserved for the protocol.
	log := mcp.NewLogger(cfg.Log.Level, os.Stderr)

	reg, err := mcp.Build(cfg)
	if err != nil {
		return err
	}
	defer reg.Close()

	server := mcp.NewServer(reg, log)

	if *stdio {
		log.Slog().Info("starting stdio", "workspaces", len(cfg.Workspaces))
		return serveStdio(server, log)
	}

	handler := buildHandler(server, log, bearerTokens, oauthServer)

	if cfg.Server.Ngrok.Enabled {
		return serveNgrok(context.Background(), cfg, env, handler, log)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	log.Slog().Info("starting", "addr", addr, "workspaces", len(cfg.Workspaces))
	srv := &http.Server{Addr: addr, Handler: handler}
	return srv.ListenAndServe()
}

// serveNgrok dials ngrok and serves the handler over the resulting tunnel.
// The tunnel URL is logged at startup and requires no external ngrok process.
func serveNgrok(ctx context.Context, cfg *mcp.Config, env map[string]string, handler http.Handler, log *mcp.Logger) error {
	authtoken, err := cfg.ResolveNgrokAuthtoken(env)
	if err != nil {
		return fmt.Errorf("resolve ngrok authtoken: %w", err)
	}

	var epOpts []ngrokconfig.HTTPEndpointOption
	if cfg.Server.Ngrok.Domain != "" {
		epOpts = append(epOpts, ngrokconfig.WithDomain(cfg.Server.Ngrok.Domain))
	}

	listener, err := ngrok.Listen(ctx,
		ngrokconfig.HTTPEndpoint(epOpts...),
		ngrok.WithAuthtoken(authtoken),
	)
	if err != nil {
		return fmt.Errorf("ngrok listen: %w", err)
	}
	log.Slog().Info("starting via ngrok", "url", listener.URL(), "workspaces", len(cfg.Workspaces))
	return http.Serve(listener, handler)
}

// buildHandler wires the HTTP routes: an unauthenticated /healthz, plus
// bearer-protected POST /mcp (JSON-RPC) and GET /mcp (SSE keepalive stream).
// If oauthServer is non-nil, OAuth routes are registered and OAuth-issued
// access tokens are accepted alongside static bearer tokens.
func buildHandler(server *mcp.Server, log *mcp.Logger, bearerTokens []string, oauthServer *mcp.OAuthServer) http.Handler {
	rpc := jsonrpc.NewEndpoint()
	server.Register(rpc)

	bearer := mcp.NewBearer(bearerTokens, log)
	if oauthServer != nil {
		bearer = bearer.WithExtra(oauthServer.CheckToken)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", endpoint.HandleFunc(func(_ http.ResponseWriter, _ *http.Request, _ struct{}) (endpoint.Renderer, error) {
		return &endpoint.JSONRenderer{Value: "ok"}, nil
	}, log))
	if oauthServer != nil {
		mux.Handle("GET /.well-known/oauth-authorization-server", endpoint.HandleFunc(oauthServer.WellKnownAuthServer, log))
		mux.Handle("GET /.well-known/oauth-protected-resource", endpoint.HandleFunc(oauthServer.WellKnownProtectedResource, log))
		mux.Handle("/oauth/authorize", endpoint.HandleFunc(oauthServer.Authorize, log))
		mux.Handle("POST /oauth/token", endpoint.HandleFunc(oauthServer.Token, log))
	}
	mux.Handle("POST /mcp", endpoint.Handler(rpc.Endpoint, bearer, log))
	mux.Handle("GET /mcp", endpoint.Handler(sseStream, bearer, log))
	return mux
}

// serveStdio runs the MCP server over stdin/stdout using newline-delimited
// JSON-RPC (the MCP stdio transport). It reuses the exact same jsonrpc dispatch
// and tool gating as the HTTP path by driving the in-process handler with a
// synthetic request per message. There is no bearer auth: stdio is local and
// trusted by construction.
func serveStdio(server *mcp.Server, _ *mcp.Logger) error {
	rpc := jsonrpc.NewEndpoint()
	server.Register(rpc)
	handler := endpoint.Handler(rpc.Endpoint)
	return serveStdioRW(handler, os.Stdin, os.Stdout)
}

// serveStdioRW runs the stdio loop over arbitrary reader/writer (so it can be
// tested without real pipes).
func serveStdioRW(handler http.Handler, r io.Reader, w io.Writer) error {
	in := bufio.NewReaderSize(r, 1<<20)
	out := bufio.NewWriter(w)
	defer out.Flush()

	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			resp := dispatchStdio(handler, line)
			if len(resp) > 0 {
				out.Write(resp)
				out.WriteByte('\n')
				if ferr := out.Flush(); ferr != nil {
					return ferr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// dispatchStdio feeds one JSON-RPC message through the HTTP handler in-process
// and returns the response bytes (empty for notifications).
func dispatchStdio(handler http.Handler, message []byte) []byte {
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(message))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// Notifications yield 204 / no body.
	return bytes.TrimRight(rec.Body.Bytes(), "\n")
}

// sseStream is the Streamable-HTTP GET stream: the optional server→client
// channel for server-initiated messages (progress, log notifications,
// resources/updated, sampling/elicitation requests, etc.). We don't push any of
// those yet, so for now it just holds the connection open with periodic
// keepalives until the client disconnects. Kept in place so that when we do add
// server-pushed messages to the LLM, the channel already exists — emit them by
// yielding SSEvents here instead of empty keepalives.
func sseStream(w http.ResponseWriter, r *http.Request, _ struct{}) (endpoint.Renderer, error) {
	ctx := r.Context()
	events := func(yield func(endpoint.SSEvent) bool) {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !yield(endpoint.SSEvent{Data: ""}) {
					return
				}
			}
		}
	}
	return &endpoint.SSERenderer{Events: events}, nil
}
