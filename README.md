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

## Build

```sh
go build -o shim ./cmd/shim
```

Requires Go 1.26+ (the `go.mod` toolchain); the `os.Root` sandbox itself needs 1.24+.

## Configure

1. Copy the examples and edit:

   ```sh
   cp config.example.yaml config.yaml
   cp .env.example .env
   ```

2. Edit `config.yaml`: set each workspace's `root` (absolute path), and tune the
   per-workspace `policy.allowGlobs` / `policy.blockGlobs`, `read.maxBytes`, and
   `grep` settings. There must be a workspace named `default` (it is the fallback
   for the `workspace` tool parameter). See `config.example.yaml` for the full
   shape.

3. Put the bearer token in `.env` (never in `config.yaml`). Generate a strong one:

   ```sh
   echo "SHIM_BEARER_TOKEN=$(openssl rand -hex 32)" > .env
   ```

   The config references it by name:

   ```yaml
   auth:
     bearerToken:
       env: SHIM_BEARER_TOKEN
   ```

   Secrets resolve from the `.env` file overlaid by the OS environment (the OS
   environment **wins**), so a deployment can inject `SHIM_BEARER_TOKEN` without a
   file. A missing/empty referenced variable is a startup error.

   **Rotation:** to roll the token without a lockstep cutover, list multiple tokens
   instead of one (set `bearerTokens`, not `bearerToken`) — the server accepts any
   of them:

   ```yaml
   auth:
     bearerTokens:
       - env: SHIM_BEARER_TOKEN        # current
       - env: SHIM_BEARER_TOKEN_NEXT   # next — switch claude.ai over, then drop this
   ```

`config.yaml` and `.env` are gitignored. Only the `*.example.*` files are committed.

## Run

```sh
./shim -config config.yaml -env .env
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

```sh
./shim -stdio -config config.yaml
```

Try it with the MCP Inspector:

```sh
npx @modelcontextprotocol/inspector ./shim -stdio -config config.yaml
```

Or drive it by hand (newline-delimited JSON-RPC on stdin):

```sh
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
  | ./shim -stdio -config config.yaml
```

Note: claude.ai itself only connects to *remote* servers, so use the HTTP +
ngrok path for it; stdio is for local clients and testing.

### With the Claude CLI (Claude Code)

A ready-to-use stdio registration lives in `.mcp.json` (gitignored — it holds
absolute host paths), pointing at a prebuilt `./shim` and `config.local.yaml`:

```sh
go build -o shim ./cmd/shim          # .mcp.json references the absolute binary path
claude mcp list                      # shows: workspace-mcp ... ⏸ Pending approval
claude                               # run once, approve the project MCP server
```

`config.local.yaml` is a host-specific test config (also gitignored) with your
local workspace roots.

To use it from a *different* project directory, register it there instead of
relying on this repo's `.mcp.json`:

```sh
claude mcp add-json workspace-mcp '{
  "type": "stdio",
  "command": "/absolute/path/to/shim",
  "args": ["-stdio", "-config", "/absolute/path/to/config.local.yaml"]
}'
```

## Expose with ngrok

```sh
cp ngrok.example.yml ngrok.yml      # edit authtoken + reserved domain + port
ngrok start --config ngrok.yml workspace-mcp
```

Reserve a domain in the ngrok dashboard so the URL is stable, and enable **edge
auth** (basic auth / OAuth / IP allow-list) — do not rely on the bearer token
alone. ngrok must expose **only this server**.

## Add to claude.ai

In claude.ai → **Settings → Connectors → Add custom connector**:

- **URL:** `https://<your-reserved-subdomain>.ngrok.app/mcp`
- **Authentication:** bearer token — the value of `SHIM_BEARER_TOKEN`.

Then start a chat and ask Claude to use the connector, e.g.:

- "List the workspaces, then list the files in the `default` workspace."
- "Read `README.md` from the `default` workspace."
- "Grep the `docs` tree for `ASC workflow`."
- "What's the git status of the `default` workspace?"

## Tool surface (read-only)

| Tool             | What it does                                                        |
| ---------------- | ------------------------------------------------------------------- |
| `workspace_list` | List configured workspaces (`{name, isGitRepo}`). No params.        |
| `file_read`      | Read one allowed file (`path`, optional `maxBytes`).               |
| `tree_search`    | Find/browse files by `path` glob and/or `where` content predicates (frontmatter-aware); each result carries its `size`. |
| `git_status`     | Branch + per-file status — git-repo workspaces only.              |

Every tool except `workspace_list` takes a `workspace` string (defaults to
`"default"`). All paths are workspace-relative and resolved through that
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

Stop the `shim` process (Ctrl-C) and stop the ngrok agent. With the tunnel down,
the server is unreachable from claude.ai.

## Layout

See [docs/design.md §3](docs/design.md) for the package map and how a tool call
flows through it. The search core under `grrep/` is vendored from
[bep/grrep](https://github.com/bep/grrep) (Apache-2.0 — see `NOTICE`); tests live
under `test/`.
