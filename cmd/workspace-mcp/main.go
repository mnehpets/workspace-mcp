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
	"runtime/debug"

	"github.com/mnehpets/http/endpoint"
	"github.com/mnehpets/http/jsonrpc"
	"golang.ngrok.com/ngrok"
	ngrokconfig "golang.ngrok.com/ngrok/config"

	"github.com/mnehpets/workspace-mcp/mcp"
)

// version is the build version string. Set via -X flag for tagged releases:
//
//	go build -ldflags="-X main.version=v1.2.3"
//
// Otherwise buildVersion() derives it from runtime/debug build info.
var version = "(devel)"

// buildVersion returns the effective version string: the ldflags override when
// set, else a VCS stamp (short hash + dirty flag) from the embedded build info.
func buildVersion() string {
	if version != "(devel)" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	// Module version from `go install …@vX.Y.Z`; skip the synthetic "(devel)" value.
	if mv := bi.Main.Version; mv != "" && mv != "(devel)" {
		if rev, dirty := vcsStamp(bi); rev != "" {
			if dirty {
				return mv + " (" + rev + ", dirty)"
			}
			return mv + " (" + rev + ")"
		}
		return mv
	}
	// No tagged module version — use the VCS stamp alone.
	if rev, dirty := vcsStamp(bi); rev != "" {
		if dirty {
			return rev + " (dirty)"
		}
		return rev
	}
	return version
}

// vcsStamp extracts the short commit hash and dirty flag from build settings.
func vcsStamp(bi *debug.BuildInfo) (rev string, dirty bool) {
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
			if len(rev) > 8 {
				rev = rev[:8]
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return
}

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
	workspace := flag.String("workspace", "", "in -stdio mode, the single workspace to serve (required when more than one is configured; HTTP mode serves all via per-route endpoints)")
	printVersion := flag.Bool("version", false, "print the build version and exit")
	flag.Parse()

	v := buildVersion()
	if *printVersion {
		fmt.Println(v)
		return nil
	}

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

	if *stdio {
		ws, err := selectStdioWorkspace(reg, *workspace)
		if err != nil {
			return err
		}
		log.Slog().Info("starting stdio", "workspace", ws.Name, "version", v)
		return serveStdio(mcp.NewServer(ws, log, v), log)
	}

	// Only the zrok frontend terminates the client connection and forwards over
	// the ziti overlay, so X-Forwarded-For is the sole source of the real client
	// address there; ngrok and direct TCP preserve it in RemoteAddr. Decide the
	// trust here, where the active tunnel is known, rather than sniffing per-request.
	trustForwardedFor := cfg.Server.Zrok.Enabled
	handler := mcp.BuildHandler(reg, log, bearerTokens, oauthServer, cfg.Server.AllowedOrigins, trustForwardedFor, v)

	if cfg.Server.Zrok.Enabled {
		return serveZrok(cfg, env, handler, log)
	}
	if cfg.Server.Ngrok.Enabled {
		return serveNgrok(context.Background(), cfg, env, handler, log, v)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	log.Slog().Info("starting", "addr", addr, "workspaces", len(cfg.Workspaces), "version", v)
	srv := &http.Server{Addr: addr, Handler: handler}
	return srv.ListenAndServe()
}

// serveNgrok dials ngrok and serves the handler over the resulting tunnel.
// The tunnel URL is logged at startup and requires no external ngrok process.
func serveNgrok(ctx context.Context, cfg *mcp.Config, env map[string]string, handler http.Handler, log *mcp.Logger, version string) error {
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
	log.Slog().Info("starting via ngrok", "url", listener.URL(), "workspaces", len(cfg.Workspaces), "version", version)
	logWorkspaceURLs(log, "ngrok", []string{listener.URL()}, cfg)
	return http.Serve(listener, handler)
}

// selectStdioWorkspace picks the single workspace to serve over stdio. Stdio has
// no URL to carry the workspace (§17), so the choice is explicit: -workspace by
// name, or — when exactly one workspace is configured — that one implicitly.
func selectStdioWorkspace(reg *mcp.Registry, name string) (*mcp.Workspace, error) {
	if name != "" {
		ws, err := reg.Get(name)
		if err != nil {
			return nil, fmt.Errorf("-workspace %q: %w", name, err)
		}
		return ws, nil
	}
	list := reg.List()
	if len(list) == 1 {
		return list[0], nil
	}
	return nil, fmt.Errorf("-stdio requires -workspace when multiple workspaces are configured (%d found)", len(list))
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
