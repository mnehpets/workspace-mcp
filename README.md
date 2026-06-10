# workspace-mcp

A standalone Go MCP server that gives the **claude.ai web app** safe, **read-only**
access to one or more **local directory trees** ("workspaces") over a public
HTTPS tunnel. A workspace whose tree is a Git repository gains extra, git-aware
operations (status).

It depends on **no other program running locally** — no second process, no
external binary. The server sandboxes each workspace's directory with Go's
`os.Root` (symlink- and TOCTOU-safe), applies a per-workspace allow/deny policy
on top, requires a bearer token, and exposes only a small read-only tool surface.
Writing is intentionally **not** built yet.

The use case is **research and workflow**, not coding — a *web* assistant using a
local repo (notes, docs, papers, data) as extra context. See **[docs/design.md](docs/design.md)**
for the design: guiding principle, security model, package layout, and the full
MCP schema. This README is the operational guide.

```
claude.ai (web app)
  → custom connector / remote MCP over HTTPS
  → ngrok public URL
  → workspace-mcp   (bearer-authenticated, default-deny, read-only)
  → per-workspace os.Root sandbox (selected by the `workspace` param)
  → local directory tree(s) — git repos get extra operations
```

## Install

```sh
go install github.com/mnehpets/workspace-mcp/cmd/workspace-mcp@latest
```

The `main` package lives under `cmd/workspace-mcp`, so the install path must
include it — `go install github.com/mnehpets/workspace-mcp@latest` (without the
subpath) fails with "does not contain package". This drops a `workspace-mcp`
binary in `$(go env GOBIN)` (or `$GOPATH/bin`). Pin a release with `@v0.1.0`
instead of `@latest`.

## Build

`go install` above is all an end user needs. Build from source only when you're
working on the code:

```sh
go build ./cmd/workspace-mcp
```

Requires Go 1.26+ (the `go.mod` toolchain); the `os.Root` sandbox itself needs 1.24+.

## Configure

1. Copy the examples and edit:

   ```sh
   cp example/config.example.yaml config.yaml
   cp example/secrets.example.env secrets.env
   ```

2. Edit `config.yaml`: set each workspace's `root` (absolute path), and tune the
   per-workspace `policy.allowGlobs` / `policy.blockGlobs`, `read.maxBytes`, and
   `grep` settings. There must be a workspace named `default` (it is the fallback
   for the `workspace` tool parameter). See `example/config.example.yaml` for the full
   shape.

3. Put the bearer token in `secrets.env` (never in `config.yaml`). Generate a strong one:

   ```sh
   echo "MCP_BEARER_TOKEN=$(openssl rand -hex 32)" > secrets.env
   ```

   The config references it by name:

   ```yaml
   auth:
     bearerToken:
       env: MCP_BEARER_TOKEN
   ```

   Secrets resolve from the `secrets.env` file overlaid by the OS environment (the OS
   environment **wins**), so a deployment can inject `MCP_BEARER_TOKEN` without a
   file. A missing/empty referenced variable is a startup error.

   **Rotation:** to roll the token without a lockstep cutover, list multiple tokens
   instead of one (set `bearerTokens`, not `bearerToken`) — the server accepts any
   of them:

   ```yaml
   auth:
     bearerTokens:
       - env: MCP_BEARER_TOKEN        # current
       - env: MCP_BEARER_TOKEN_NEXT   # next — switch claude.ai over, then drop this
   ```

`config.yaml` and `secrets.env` are gitignored. Only the `*.example.*` files are committed.

## Run

```sh
./workspace-mcp -config config.yaml -env secrets.env
# health check (no auth):
curl http://127.0.0.1:3850/healthz   # -> {"ok":true}
```

The server binds `127.0.0.1` only; it is meant to sit behind a tunnel.

## Run as a local stdio server

For local clients that spawn the server as a subprocess (MCP Inspector, Claude
Desktop, Claude Code) use `-stdio`. There is no HTTP listener and no bearer token
in this mode — stdio is a trusted local pipe. The same sandbox, policy, and
read-only guarantees still apply; only the transport differs. All logs go to
stderr so stdout stays a clean JSON-RPC channel.

Stdio has no URL to carry the workspace, so it serves exactly one. With a single
workspace configured it is selected implicitly; with more than one, name it with
`-workspace`:

```sh
./workspace-mcp -stdio -config config.yaml                 # one workspace configured
./workspace-mcp -stdio -workspace default -config config.yaml   # pick one of several
```

Try it with the MCP Inspector:

```sh
npx @modelcontextprotocol/inspector ./workspace-mcp -stdio -config config.yaml
```

Or drive it by hand (newline-delimited JSON-RPC on stdin):

```sh
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
  | ./workspace-mcp -stdio -config config.yaml
```

Note: claude.ai itself only connects to *remote* servers, so use the HTTP +
ngrok path for it; stdio is for local clients and testing.

### With the Claude CLI (Claude Code)

A ready-to-use stdio registration lives in `.mcp.json` (gitignored — it holds
absolute host paths), pointing at a prebuilt `./workspace-mcp` and `config.local.yaml`:

```sh
go build ./cmd/workspace-mcp  # .mcp.json references the absolute binary path
claude mcp list               # shows: workspace-mcp ... ⏸ Pending approval
claude                        # run once, approve the project MCP server
```

`config.local.yaml` is a host-specific test config (also gitignored) with your
local workspace roots.

To use it from a *different* project directory, register it there instead of
relying on this repo's `.mcp.json`:

```sh
claude mcp add-json workspace-mcp '{
  "type": "stdio",
  "command": "/absolute/path/to/workspace-mcp",
  "args": ["-stdio", "-workspace", "default", "-config", "/absolute/path/to/config.local.yaml"]
}'
```

## Expose with ngrok

The server has **built-in ngrok** — no external process or `ngrok.yml` needed.
Enable it in `config.yaml`:

```yaml
server:
  ngrok:
    enabled: true
    authtoken:
      env: NGROK_AUTHTOKEN          # in secrets.env
    # domain: my-reserved-host.ngrok.app   # optional: pin a stable domain
```

Add `NGROK_AUTHTOKEN` to `secrets.env` (find yours in the ngrok dashboard). The
public URL is logged at startup:

```
INFO starting via ngrok url=https://xxxx.ngrok.app workspaces=1
```

Reserve a domain in the ngrok dashboard so the URL is stable across restarts.
With `server.ngrok.enabled: true` the `server.host`/`server.port` fields are
unused — the server does not bind a local TCP port.

## Expose with zrok

zrok is a self-hostable alternative to ngrok, built on OpenZiti. The server has
a **built-in zrok** client — no external `zrok` process and no `zrok
enable` state on this machine. Everything is driven from `config.yaml` + secrets;
the SDK never reads `~/.zrok` or `ZROK_*` ambient env vars. Enable **at most one**
of ngrok/zrok — each replaces the local TCP listener. If you enable both ngrok
and zrok, you'll give the AI model full sentience and start the singularity.

```yaml
server:
  zrok:
    enabled: true
    enableToken:
      env: ZROK_ENABLE_TOKEN          # in secrets.env
    apiEndpoint: https://api-v2.zrok.io   # optional; this is the default
    frontend: public                  # optional public frontend namespace (default)
    uniqueName: my-workspace-mcp      # reserve a stable share name (see below)
```

Add `ZROK_ENABLE_TOKEN` to `secrets.env` — this is your zrok account's
token (from the zrok web console select your user account, and look for
the "Account Token"). On startup the server creates an *ephemeral* zrok
environment, opens a public proxy share, and logs the public URL:

```
INFO starting via zrok url=https://my-workspace-mcp.shares.zrok.io workspaces=1
```

### Reserve a stable name (important)

**Set `uniqueName`, or your URL changes on every restart** — and since the
connector URL is baked into every claude.ai (and other client) registration, a
changing URL means re-registering each connector each time.

Without `uniqueName` you get a fresh random URL (e.g.
`https://xxr2b7tzfx64.shares.zrok.io`) every start — the same tradeoff as ngrok
without a reserved domain. With `server.zrok.enabled: true` the
`server.host`/`server.port` fields are unused.

> Each start creates a fresh *ephemeral* zrok environment and releases it on a
> clean shutdown (Ctrl-C / SIGTERM). An unclean exit (`kill -9`, crash) can leave
> one behind, and zrok's free tier caps concurrent environments — so leaked
> environments eventually make share creation fail with an opaque `500`. When a
> stable `uniqueName` is set, the server **self-heals**: on startup it reaps any
> environment this same name left over (`env-<uniqueName>`) before enabling a new
> one, so "last start wins" and a hard-killed predecessor cleans itself up next
> run. The reserved name itself is never touched. (Environments leaked under a
> *different* `uniqueName`, or before you set one, still need a one-time prune in
> the zrok web console.)

## Add to claude.ai

In claude.ai → **Settings → Connectors → Add custom connector**.

The connector URL is your tunnel domain plus `/mcp/<workspace>` — **one
workspace per connector**. The path segment selects the tree, so the URL *is* the
workspace; register a separate connector for each tree you want to expose.

- **URL:** `https://<your-domain>/mcp/<workspace>` (e.g.
  `https://xxxx.ngrok.app/mcp/default` or `.../mcp/notes`)

The workspace names come from `config.yaml` (`workspaces[].name`). Tools take no
`workspace` argument — the endpoint already picked it.

**With OAuth** (the only auth type claude.ai currently supports):

Enable `auth.oauth` in `config.yaml` (see `example/config.example.yaml`) and add
`OAUTH_CLIENT_SECRET` to `secrets.env`. Then supply:

- **Client ID:** the value you set for `auth.oauth.clientID`
- **Client Secret:** the value of `OAUTH_CLIENT_SECRET`

The authorization and token URLs are advertised by the server's
`.well-known` discovery endpoints, so claude.ai picks them up automatically —
you don't enter them by hand. (This is standard OAuth 2.0 metadata discovery;
other MCP clients that follow the spec should behave the same way.)

When claude.ai initiates the flow you'll see an Authorize page — one click grants
access. Access tokens are valid for 1 hour and automatically re-issued.

Then start a chat and ask Claude to use the connector, e.g.:

- "List the workspaces, then list the files in the `default` workspace."
- "Read `README.md` from the `default` workspace."
- "Grep the `docs` tree for `ASC workflow`."
- "What's the git status of the `default` workspace?"

## Tool surface (read-only)

| Tool             | What it does                                                        |
| ---------------- | ------------------------------------------------------------------- |
| `workspace_info` | Orientation for *this* workspace (`{name, isGitRepo, description, wellKnownFiles, orientation, preview}`). No params. Mirrors the connect-time `instructions` (fallback for hosts that ignore them) and inlines a capped preview of the top orientation file. |
| `file_read`      | Read one allowed file (`path`, optional `maxBytes`).               |
| `tree_search`    | Find/browse files by `path` glob and/or `where` content predicates (frontmatter-aware); each result carries its `size`. |
| `git_status`     | Branch + per-file status — git-repo workspaces only.              |

No tool takes a `workspace` argument: the workspace is fixed by the connector URL
(`/mcp/<workspace>`). All paths are workspace-relative and resolved through that
workspace's `os.Root`. Full input/output schemas and the error spec are in
[docs/design.md §5](docs/design.md).

## Safety model

In short: hard **containment** (one symlink/TOCTOU-safe `os.Root` per workspace)
plus a soft **policy** layer (per-workspace allow/deny globs + `.gitignore` +
dotfile backstop, block always wins); read-only by construction; a constant-time
bearer token (≥ 32 bytes, never logged; multiple accepted for rotation) with ngrok
edge auth on top; and an audit
log that records every call but never file contents or the token. The full
reasoning is in [docs/design.md §2](docs/design.md).

## Shutdown

Stop the `workspace-mcp` process (Ctrl-C). With built-in ngrok the tunnel tears down
automatically — no separate agent to stop.

## Layout

See [docs/design.md §3](docs/design.md) for the package map and how a tool call
flows through it. The search core under `grrep/` is vendored from
[bep/grrep](https://github.com/bep/grrep) (Apache-2.0 — see `NOTICE`); tests live
under `test/`.
