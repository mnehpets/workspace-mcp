package mcp

import (
	"net/http"
	"time"

	"github.com/mnehpets/http/endpoint"
	"github.com/mnehpets/http/jsonrpc"
)

// BuildHandler wires the HTTP routes for the server. Workspace selection is by
// route (§17): each configured workspace gets its own MCP endpoint at
// POST/GET /mcp/<name>, so a claude.ai connector URL *is* a workspace. An unknown
// path segment is a plain 404 (ServeMux has no matching route), not a domain
// error. Auth is server-wide (one bearer/OAuth across every path); per-workspace
// policy remains the AuthZ layer.
//
// Routes:
//   - GET  /healthz                                       (unauthenticated)
//   - OAuth discovery + endpoints                         (when oauthServer != nil)
//   - POST /mcp/<name>  (JSON-RPC)  + GET /mcp/<name> (SSE keepalive), origin- and bearer-gated
//
// allowedOrigins is the Origin-header allowlist (MCP 2025-11-25 DNS-rebinding
// defense); empty means Origin-less requests pass and any present Origin is
// rejected (see NewOriginCheck).
func BuildHandler(reg *Registry, log *Logger, bearerTokens []string, oauthServer *OAuthServer, allowedOrigins []string) http.Handler {
	bearer := NewBearer(bearerTokens, log)
	if oauthServer != nil {
		// With OAuth on, an unauthenticated request gets a WWW-Authenticate header
		// pointing at this endpoint's protected-resource metadata (RFC 9728).
		bearer = bearer.WithExtra(oauthServer.CheckToken).WithResourceMetadata()
	}
	origin := NewOriginCheck(allowedOrigins, log)

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", endpoint.HandleFunc(func(_ http.ResponseWriter, _ *http.Request, _ struct{}) (endpoint.Renderer, error) {
		return &endpoint.JSONRenderer{Value: "ok"}, nil
	}, log))
	if oauthServer != nil {
		mux.Handle("GET /.well-known/oauth-authorization-server", endpoint.HandleFunc(oauthServer.WellKnownAuthServer, log))
		// Both the bare path and any per-resource suffix (.../mcp/<name>) resolve to
		// protected-resource metadata for the named resource (RFC 9728).
		mux.Handle("GET /.well-known/oauth-protected-resource", endpoint.HandleFunc(oauthServer.WellKnownProtectedResource, log))
		mux.Handle("GET /.well-known/oauth-protected-resource/", endpoint.HandleFunc(oauthServer.WellKnownProtectedResource, log))
		mux.Handle("/oauth/authorize", endpoint.HandleFunc(oauthServer.Authorize, log))
		mux.Handle("POST /oauth/token", endpoint.HandleFunc(oauthServer.Token, log))
	}

	// One MCP endpoint per workspace; the path segment selects the os.Root/policy.
	for _, ws := range reg.List() {
		srv := NewServer(ws, log)
		rpc := jsonrpc.NewEndpoint()
		srv.Register(rpc)
		mux.Handle("POST /mcp/"+ws.Name, endpoint.Handler(rpc.Endpoint, origin, bearer, log))
		mux.Handle("GET /mcp/"+ws.Name, endpoint.Handler(sseStream, origin, bearer, log))
	}
	return mux
}

// sseStream is the Streamable-HTTP GET stream: the optional server→client
// channel for server-initiated messages (progress, log notifications, etc.). We
// don't push any of those yet, so for now it just holds the connection open with
// periodic keepalives until the client disconnects. Kept in place so that when we
// do add server-pushed messages, the channel already exists — emit them by
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
