// This file implements the built-in zrok tunnel: an alternative
// to ngrok for reaching the server from claude.ai and other public MCP
// clients.
//
// The non-negotiable constraint is that the zrok SDK is driven entirely from
// config.yaml + secret refs — never from zrok's ambient environment. The
// SDK's default path (environment.LoadRoot) reads an enabled environment
// from ~/.zrok (or ZROK_* env vars); we bypass that with zrokRoot, an
// in-memory env_core.Root built from our resolved enableToken + apiEndpoint.
// env_core.Root is an interface, and the SDK calls we use (EnableEnvironment,
// CreateShare, DeleteShare, DisableEnvironment) only exercise Client(),
// Environment(), and IsEnabled(); the disk-flavored methods are stubbed.
//
// Lifecycle: at startup we enable an *ephemeral* zrok environment from the
// account token, open a public proxy share on it, and http.Serve over a ziti
// listener bound to the share. On shutdown (SIGINT/SIGTERM or serve error)
// the share is deleted and the environment disabled, so nothing leaks on the
// zrok service. The ziti identity returned by the enable call stays in
// memory: sdk.NewListener insists on reading the identity from a file path,
// so we inline its few lines (json.Unmarshal + ziti.NewContext) instead of
// ever writing the private key to disk.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-openapi/runtime"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/zrok/v2/build"
	"github.com/openziti/zrok/v2/environment/env_core"
	"github.com/openziti/zrok/v2/rest_client_zrok"
	zrokmetadata "github.com/openziti/zrok/v2/rest_client_zrok/metadata"
	zrokshare "github.com/openziti/zrok/v2/rest_client_zrok/share"
	"github.com/openziti/zrok/v2/sdk/golang/sdk"

	"github.com/mnehpets/workspace-mcp/mcp"
)

const defaultZrokApiEndpoint = "https://api-v2.zrok.io"

// errZrokMemRoot marks env_core.Root methods that have no meaning for an
// in-memory, config-driven root (everything that would read or write ~/.zrok).
var errZrokMemRoot = errors.New("not supported by the in-memory zrok root")

// zrokRoot is an in-memory env_core.Root constructed from our config-resolved
// values. It deliberately has no on-disk state: no ~/.zrok, no ZROK_* env
// vars, no identity files.
type zrokRoot struct {
	apiEndpoint string
	frontend    string
	env         *env_core.Environment
}

var _ env_core.Root = (*zrokRoot)(nil)

func newZrokRoot(enableToken, apiEndpoint, frontend string) *zrokRoot {
	if apiEndpoint == "" {
		apiEndpoint = defaultZrokApiEndpoint
	}
	if frontend == "" {
		frontend = "public"
	}
	return &zrokRoot{
		apiEndpoint: apiEndpoint,
		frontend:    frontend,
		env: &env_core.Environment{
			AccountToken: enableToken,
			ApiEndpoint:  apiEndpoint,
		},
	}
}

// Client mirrors env_v0_4's Client(): REST transport against the configured
// API endpoint plus the controller's client-version handshake. build.String()
// from a library import reports "v2.0.x [developer build]", which matches the
// controller's accepted v2.0 patterns.
func (r *zrokRoot) Client() (*rest_client_zrok.Zrok, error) {
	apiUrl, err := url.Parse(r.apiEndpoint)
	if err != nil {
		return nil, fmt.Errorf("parse zrok api endpoint %q: %w", r.apiEndpoint, err)
	}
	transport := httptransport.New(apiUrl.Host, "/api/v2", []string{apiUrl.Scheme})
	transport.Producers["application/zrok.v1+json"] = runtime.JSONProducer()
	transport.Consumers["application/zrok.v1+json"] = runtime.JSONConsumer()

	client := rest_client_zrok.New(transport, strfmt.Default)
	if _, err := client.Metadata.ClientVersionCheck(&zrokmetadata.ClientVersionCheckParams{
		Body: zrokmetadata.ClientVersionCheckBody{ClientVersion: build.String()},
	}); err != nil {
		return nil, fmt.Errorf("zrok client version check against %q: %w", r.apiEndpoint, err)
	}
	return client, nil
}

func (r *zrokRoot) Metadata() *env_core.Metadata { return nil }
func (r *zrokRoot) Obliterate() error            { return errZrokMemRoot }

func (r *zrokRoot) HasConfig() (bool, error) { return true, nil }
func (r *zrokRoot) Config() *env_core.Config {
	return &env_core.Config{ApiEndpoint: r.apiEndpoint, DefaultNamespace: r.frontend, Headless: true}
}
func (r *zrokRoot) SetConfig(*env_core.Config) error { return errZrokMemRoot }

func (r *zrokRoot) ApiEndpoint() (string, string)      { return r.apiEndpoint, "config" }
func (r *zrokRoot) DefaultNamespace() (string, string) { return r.frontend, "config" }
func (r *zrokRoot) Headless() (bool, string)           { return true, "config" }
func (r *zrokRoot) SuperNetwork() (bool, string)       { return false, "config" }

// IsEnabled reports whether the ephemeral environment has been enabled (the
// controller has issued us an environment ziti identity).
func (r *zrokRoot) IsEnabled() bool                    { return r.env.ZitiIdentity != "" }
func (r *zrokRoot) Environment() *env_core.Environment { return r.env }
func (r *zrokRoot) SetEnvironment(env *env_core.Environment) error {
	r.env = env
	return nil
}
func (r *zrokRoot) DeleteEnvironment() error { return errZrokMemRoot }

func (r *zrokRoot) PublicIdentityName() string      { return "public" }
func (r *zrokRoot) EnvironmentIdentityName() string { return "environment" }

func (r *zrokRoot) ZitiIdentityNamed(string) (string, error)   { return "", errZrokMemRoot }
func (r *zrokRoot) SaveZitiIdentityNamed(string, string) error { return errZrokMemRoot }
func (r *zrokRoot) DeleteZitiIdentityNamed(string) error       { return errZrokMemRoot }

func (r *zrokRoot) AgentSocket() (string, error)     { return "", errZrokMemRoot }
func (r *zrokRoot) AgentRegistry() (string, error)   { return "", errZrokMemRoot }
func (r *zrokRoot) AgentEnrollment() (string, error) { return "", errZrokMemRoot }

// reserveZrokName reserves a share name in the configured frontend namespace so
// the public URL stays stable across restarts. zrok's NameSelection requires the
// name to already exist; passing an unreserved name to CreateShare fails with a
// 409 ("error finding name … in namespace …: sql: no rows in result set").
// Reservation is account-scoped and persistent, so we reserve idempotently on
// every startup: a 409 here means the name is already reserved (by us, on an
// earlier run), which is exactly the state we want. Any other error propagates.
func reserveZrokName(root *zrokRoot, namespace, name string) (bool, error) {
	client, err := root.Client()
	if err != nil {
		return false, err
	}
	auth := httptransport.APIKeyAuth("X-TOKEN", "header", root.env.AccountToken)
	req := zrokshare.NewCreateShareNameParams()
	req.Body = zrokshare.CreateShareNameBody{NamespaceToken: namespace, Name: name}
	if _, err := client.Share.CreateShareName(req, auth); err != nil {
		var conflict *zrokshare.CreateShareNameConflict
		if errors.As(err, &conflict) {
			return false, nil // already reserved — reuse it
		}
		return false, fmt.Errorf("reserve zrok name %q in namespace %q: %w", name, namespace, err)
	}
	return true, nil
}

// reapStaleZrokEnvs disables any of the account's environments left over from a
// previous run of this same uniqueName. Each start creates a fresh *ephemeral*
// environment; an unclean exit (SIGKILL during a dev rebuild, or a failed
// teardown) leaves it enabled on the account, and zrok's free tier caps
// concurrent environments — once the cap is hit, the next CreateShare fails with
// an opaque 500 from the controller's resource allocator. We make restarts
// self-healing by reaping prior environments that carry our exact description
// (env-<uniqueName>) before enabling a new one. This is safe because only one
// instance can hold a given uniqueName at a time (the reserved name maps to a
// single share), so a same-named environment is by definition stale — "last
// start wins". Best-effort: failures here only warn, never block startup.
func reapStaleZrokEnvs(root *zrokRoot, description string, log *mcp.Logger) {
	client, err := root.Client()
	if err != nil {
		log.Slog().Warn("zrok reap: client", "err", err)
		return
	}
	auth := httptransport.APIKeyAuth("X-TOKEN", "header", root.env.AccountToken)
	ov, err := client.Metadata.Overview(zrokmetadata.NewOverviewParams(), auth)
	if err != nil {
		log.Slog().Warn("zrok reap: overview", "err", err)
		return
	}
	for _, er := range ov.Payload.Environments {
		if er.Environment == nil || er.Environment.Description != description || er.Environment.ZID == "" {
			continue
		}
		if err := sdk.DisableEnvironment(&sdk.Environment{ZitiIdentity: er.Environment.ZID}, root); err != nil {
			log.Slog().Warn("zrok reap: disable stale environment", "zId", er.Environment.ZID, "err", err)
			continue
		}
		log.Slog().Info("zrok reaped stale environment", "zId", er.Environment.ZID, "description", description)
	}
}

// createZrokShareWithRetry opens the public proxy share, retrying on the
// controller's transient 500s. CreateShare's allocation step runs a sequence of
// live ziti-network operations (config, service, then bind/dial/edge-router
// policies); any one can momentarily flake and surface as an opaque
// shareInternalServerError. Those are safe to retry — the request is unchanged
// and idempotent from our side. A 409 (name conflict) or any other error is
// deterministic, so we return it immediately rather than retrying into the same wall.
func createZrokShareWithRetry(root *zrokRoot, req *sdk.ShareRequest, log *mcp.Logger) (*sdk.Share, error) {
	const attempts = 3
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		var shr *sdk.Share
		if shr, err = sdk.CreateShare(root, req); err == nil {
			return shr, nil
		}
		var ise *zrokshare.ShareInternalServerError
		if !errors.As(err, &ise) || attempt == attempts {
			return nil, err
		}
		backoff := time.Duration(attempt) * time.Second
		log.Slog().Warn("zrok create share failed (transient), retrying",
			"attempt", attempt, "attempts", attempts, "backoff", backoff.String(), "err", err)
		time.Sleep(backoff)
	}
	return nil, err
}

// serveZrok brings up the built-in zrok tunnel and serves the handler over
// it. It enables an ephemeral zrok environment from the configured account
// token, creates a public proxy share (named via frontend/uniqueName when
// configured, otherwise a random share token), logs the public URL(s), and
// serves HTTP over the share's ziti listener. The share and the ephemeral
// environment are released on SIGINT/SIGTERM or when serving stops, so clean
// shutdowns leave nothing behind on the zrok service.
func serveZrok(cfg *mcp.Config, env map[string]string, handler http.Handler, log *mcp.Logger) error {
	enableToken, err := cfg.ResolveZrokEnableToken(env)
	if err != nil {
		return fmt.Errorf("resolve zrok enableToken: %w", err)
	}

	root := newZrokRoot(enableToken, cfg.Server.Zrok.ApiEndpoint, cfg.Server.Zrok.Frontend)

	// The environment and share carry console-visible labels. Derive them from
	// the configured uniqueName (the stable public name) so they read as a set —
	// env-<name> / share-<name> — instead of an opaque "workspace-mcp". Fall back
	// to a generic label when no uniqueName is set (random-URL mode).
	label := cfg.Server.Zrok.UniqueName
	if label == "" {
		label = "workspace-mcp"
	}

	envDescription := "env-" + label

	// With a stable uniqueName there is exactly one live instance, so reap any
	// environment this same name leaked on a previous (unclean) exit before we
	// add another — otherwise leaked ephemeral envs accumulate against the
	// free-tier cap and CreateShare starts failing with an opaque 500.
	if cfg.Server.Zrok.UniqueName != "" {
		reapStaleZrokEnvs(root, envDescription, log)
	}

	host, _ := os.Hostname()
	zenv, err := sdk.EnableEnvironment(root, &sdk.EnableRequest{
		Host:        host,
		Description: envDescription,
	})
	if err != nil {
		return fmt.Errorf("zrok enable environment: %w", err)
	}
	root.env.ZitiIdentity = zenv.ZitiIdentity

	disableEnv := func() {
		if err := sdk.DisableEnvironment(zenv, root); err != nil {
			log.Slog().Error("zrok disable environment", "err", err)
		}
	}

	// A configured uniqueName gives a stable URL only if the name is reserved in
	// the frontend namespace first; reserve it (idempotently) before sharing, or
	// CreateShare 409s on the unreserved name.
	if cfg.Server.Zrok.UniqueName != "" {
		created, err := reserveZrokName(root, root.frontend, cfg.Server.Zrok.UniqueName)
		if err != nil {
			disableEnv()
			return err
		}
		if created {
			log.Slog().Info("reserved zrok name", "name", cfg.Server.Zrok.UniqueName, "namespace", root.frontend)
		}
	}

	shr, err := createZrokShareWithRetry(root, &sdk.ShareRequest{
		ShareMode:   sdk.PublicShareMode,
		BackendMode: sdk.ProxyBackendMode,
		Target:      "share-" + label,
		NameSelections: []sdk.NameSelection{
			{NamespaceToken: root.frontend, Name: cfg.Server.Zrok.UniqueName},
		},
	}, log)
	if err != nil {
		disableEnv()
		return fmt.Errorf("zrok create share: %w", err)
	}

	// release tears down the share and the ephemeral environment exactly
	// once, whether we get here via signal or via http.Serve returning.
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			if err := sdk.DeleteShare(root, shr); err != nil {
				log.Slog().Error("zrok delete share", "err", err)
			}
			disableEnv()
		})
	}

	// In-memory equivalent of sdk.NewListener: the SDK loads the ziti
	// identity from a file (ziti.NewConfigFromFile is ReadFile +
	// json.Unmarshal); ours never touches disk.
	zcfg := &ziti.Config{}
	if err := json.Unmarshal([]byte(zenv.ZitiConfig), zcfg); err != nil {
		release()
		return fmt.Errorf("parse zrok ziti identity: %w", err)
	}
	zctx, err := ziti.NewContext(zcfg)
	if err != nil {
		release()
		return fmt.Errorf("zrok ziti context: %w", err)
	}
	listener, err := zctx.ListenWithOptions(shr.Token, &ziti.ListenOptions{
		ConnectTimeout:               30 * time.Second,
		WaitForNEstablishedListeners: 1,
	})
	if err != nil {
		release()
		return fmt.Errorf("zrok listen: %w", err)
	}

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigC
		log.Slog().Info("shutting down zrok share")
		_ = listener.Close()
		release()
		os.Exit(0)
	}()

	endpoints := make([]string, len(shr.FrontendEndpoints))
	for i, ep := range shr.FrontendEndpoints {
		endpoints[i] = normalizePublicBase(ep)
	}
	log.Slog().Info("starting via zrok",
		"url", strings.Join(endpoints, " "),
		"workspaces", len(cfg.Workspaces))
	logWorkspaceURLs(log, "zrok", shr.FrontendEndpoints, cfg)
	err = http.Serve(listener, handler)
	release()
	return err
}

// logWorkspaceURLs prints the public connector URL for each configured
// workspace. The path segment selects the tree (<base>/mcp/<name>), so each
// workspace is its own claude.ai connector — this is the URL to paste when
// adding it. Logged for every tunnel (ngrok and zrok) over each public base
// (zrok can advertise more than one frontend endpoint).
func logWorkspaceURLs(log *mcp.Logger, via string, bases []string, cfg *mcp.Config) {
	for _, base := range bases {
		base = normalizePublicBase(base)
		for i := range cfg.Workspaces {
			name := cfg.Workspaces[i].Name
			log.Slog().Info("workspace url", "via", via, "workspace", name, "url", base+"/mcp/"+name)
		}
	}
}

// normalizePublicBase trims a trailing slash and ensures the base carries a
// scheme. ngrok's listener.URL() is already a full https:// URL, but zrok's
// FrontendEndpoints come back bare (e.g. "myname.share.zrok.io"), so a
// scheme-less base is assumed to be https — the only scheme a public connector
// URL uses.
func normalizePublicBase(base string) string {
	base = strings.TrimRight(base, "/")
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	return base
}
