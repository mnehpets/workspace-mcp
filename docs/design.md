# Design — workspace-mcp

This document records the *design decisions* and the reasoning behind them, the
division of functionality across packages, the configuration file, and — most
importantly — the **MCP schema** (the wire surface seen by claude.ai / ChatGPT).

`PLAN.md` is a scratch planning doc and will be deleted; this is the durable
reference. Where this doc and the code disagree, the code wins — file/line
references are given so claims stay checkable.

---

## 1. What this is, and the one guiding principle

workspace-mcp is a **standalone Go MCP server** that gives a remote LLM front-end
(primarily the **claude.ai web app**, also ChatGPT or any MCP client) safe,
read-only access to one or more **local directory trees** ("workspaces") over an
HTTPS tunnel.

The use case is **not coding.** Local CLI agents (Claude Code, Codex) already edit
code directly on the machine. This server exists so a *web* assistant can use a
local repo as **extra context for research and workflow** — notes, docs, papers,
datasets, a knowledge base.

That use case produces the single design rule everything else is measured against:

> **The server is a thin, safe pipe.** It does only what the model *cannot* do
> from the outside — reach local files and git — plus the cheap *orientation* that
> tells the model what to ask for. It never does analysis the model does better
> with raw content.

Consequences of the rule (and why several "obvious" features are deliberately
absent):

- **No semantic search / embeddings.** Retrieval-by-meaning is the platform's job
  (RAG) and is exactly the heavy lifting we hand off. We expose raw content +
  lexical search; the model reasons.
- **No link graph, no recency analysis, no summarization, no outline extraction.**
  The model derives these from raw content trivially; git already answers
  "what changed". Replicating them buys nothing.
- **Hand over raw bytes, don't transform them.** For non-text research artifacts
  (PDF, images) the right move is to deliver the raw bytes and let the platform
  parse them (planned — `file_read` binary delivery), not to build extractors.
- **Orientation is the one exception, and only when it's aggregation the model
  can't cheaply do** — e.g. a corpus-wide tag/frontmatter rollup, or per-workspace
  descriptions. A table of contents, not an analysis engine.

### Non-goals (hard)

No remote shell or command runner; no arbitrary write (writes are a deliberately
deferred, flag-gated task); no Git automation (commit/push/branch/rebase); no
second LLM/agent loop; no LSP/symbol index; no multi-user SaaS; **no external
binaries at all** (single self-contained Go binary). Single-user local developer
tool.

---

## 2. Security model — the core decision

Security is the reason the project is shaped the way it is, so it comes before the
feature surface.

### 2.1 Two distinct layers: containment vs. policy

| Layer | Mechanism | Guarantee |
|---|---|---|
| **Containment (hard)** | one `os.Root` per workspace (`os.OpenRoot`, Go 1.24+) | Every filesystem op stays inside the root **even through symlinks**, and resists TOCTOU races. A symlink to `/etc/passwd` cannot be followed out. This is *the wall.* |
| **Policy (soft)** | per-workspace allow/deny globs + `.gitignore` + dotfile rules | Refuses things that are *inside* the sandbox but shouldn't be served (`.git/**`, `.env`, keys, `node_modules`). `blockGlobs` always wins. Sits *on top* of containment, not as the boundary. |

A path must clear **both** layers to be served. Model-supplied absolute paths and
any `..` are rejected *before* resolution ([fsroot.Clean], surfaced as
`POLICY_DENIED`/`absolute_path`|`traversal` in [mcp/server.go:215]); everything
else resolves through that workspace's `*os.Root`. We never hand-roll traversal
checks as the boundary — that is precisely what `os.Root` is for.

### 2.2 Where `os.Root` is load-bearing — and where it isn't

`os.Root` is essential for **model-supplied paths** (`file_read`, the deferred
patch tool): the path comes straight from the model and may be hostile.

Two classes of helper deliberately read **outside** `os.Root`, which is safe
because they never serve file *content* to the model — they produce metadata only:

- **go-git** (status, tracked-file enumeration) reads via its own `go-billy`/`osfs`.
- **grrep's `IgnoreSet`** reads `.gitignore`/`.ignore` while walking.

The trust split: these decide *what exists / what's ignored / what changed*;
`os.Root` decides *what content crosses the boundary*. The grep walker is the one
nuance — it reads content, but over a tree we control (`fastwalk` skips
non-regular files so symlinks are never followed, and skips `.git`), and it still
opens each matched leaf through `os.Root`. Rule of thumb: **anything reading
content the model can aim goes through `os.Root`; pure-metadata helpers may walk
freely.**

### 2.3 Read-only by construction, and auth

- **Read-only is a build property, not a config toggle.** No code path writes
  (until the deferred patch task lands behind a flag). The `*os.Root` is used only
  for read methods.
- **Auth is layered:** a server-wide bearer token (constant-time compared, ≥ 32
  bytes, sourced from `.env`/OS env — never from `config.yaml`), plus the ngrok
  edge (OAuth/IP-allow/reserved domain). 401 on missing/invalid with no hint as to
  which. AuthN is server-wide; **AuthZ is per-workspace policy**.
  - **Multiple tokens for rotation.** Config accepts either a single
    `auth.bearerToken` or a list `auth.bearerTokens` (not both). The server accepts
    a request bearing *any* configured token, so an old and new token can both be
    valid during an overlap window — add the new, switch claude.ai over, drop the
    old — without lockstep. The presented token is digested and compared against
    every expected one **without short-circuiting**, so timing reveals neither the
    token nor which (if any) matched ([auth/bearer.go]). This is the minimal slice
    of the larger "per-client tokens / scoping / hot-reload" idea (still future);
    tokens here carry no identity and aren't distinguished in the audit log.
- **Audit log:** every call records method, tool, workspace, resolved path(s),
  allow/deny + reason, and byte/match counts — never file contents, never the
  token ([mcp/server.go:154] `ToolsCall` → `s.log.ToolCall(ev)`).

---

## 3. Division of functionality (package map)

Packages live at the **repo root**, not under `internal/` — this is an
application, not a library for external import, so `internal/` would add nesting
without buying anything. Each package has one job, and the dependency arrows point
one way: `mcp` orchestrates; everything below it is a mechanism that knows nothing
about MCP.

```
cmd/shim/main.go     Wiring + transports. Loads config+secrets, builds the
                     workspace registry, mounts HTTP routes (/healthz, POST/GET
                     /mcp) or the -stdio loop. The only package that knows about
                     net/http and process lifecycle.

config/              Pure config. config.go: typed YAML load with KnownFields(true)
                     + semantic Validate(). secrets.go: dotenv + os.Environ merge,
                     {env: NAME} reference resolution. No I/O beyond reading files.

auth/                Bearer-token endpoint.Processor. Constant-time compare; 401
                     with no disclosure. Wired ahead of every route but /healthz.

workspace/           The registry: name → Workspace{ *os.Root, Policy, Ignore,
                     Read/Grep settings, IsGitRepo }. Built once at startup.
                     Lookup by name → UNKNOWN_WORKSPACE otherwise. This is where a
                     tool call turns a `workspace` string into capabilities.

fsroot/              The os.Root wrapper. Clean() rejects absolute/`..` paths;
                     Open/Stat/ReadDir/WalkDir take workspace-relative paths and
                     stay in the sandbox. The containment primitive.

policy/              Glob allow/deny (block wins) + dotfile rule. CheckFile/CheckDir
                     return {Allowed, Reason}. The soft layer atop fsroot.

grrep/               Vendored from bep/grrep (Apache-2.0, SPDX retained, see
                     NOTICE). match.go verbatim (Matcher + literal pre-filter);
                     scan.go adapted to emit structured {path,line,text} instead of
                     CLI stdout; ignore.go = IgnoreSet (nested .gitignore/.ignore).

search/              Drives grrep over a workspace. grep.go: fastwalk + per-leaf
                     os.Root open + policy/ignore filter + worker pool + match cap.
                     find.go: fuzzy filename search over the filtered tree.

gitaware/            go-git (pure Go) git-awareness, metadata only. detect.go
                     (is it a repo?), status.go (Worktree().Status() + branch),
                     tracked.go (tracked-file enumeration). Never a content path.

mcp/                 The protocol surface and the gate. server.go: initialize /
                     tools/list / tools/call / ping, error model, dispatch table.
                     tools.go: tool catalog + JSON Schemas + one handler per tool,
                     each running the full gate (resolve workspace → enablement →
                     path policy → fsroot/search/gitaware → limits → audit).

audit/               Redacting slog logger. ToolEvent carries the per-call record;
                     never logs content or the token.
```

The shape of a tool call, end to end ([mcp/server.go:154], [mcp/tools.go]):

```
POST /mcp → bearer (auth/) → jsonrpc dispatch → ToolsCall
  → allowlist tool name (else JSON-RPC InvalidParams)
  → handler: unmarshal args
           → resolve `workspace` (workspace/)        → UNKNOWN_WORKSPACE
           → per-workspace enablement (grep on? git?) → GREP_DISABLED / NOT_A_GIT_REPO
           → fsroot.Clean(path) + policy.CheckFile/Dir → POLICY_DENIED
           → do the one read (fsroot / search / gitaware)
           → apply size/match limits
  → audit-log the ToolEvent (allow/deny + reason + counts)
  → wrap result as MCP content (or isError)
```

---

## 4. Configuration

Config is a **YAML file** (`-config`, default `./config.yaml`), chosen over flat
`KEY=value` because the shape is genuinely nested — a list of workspaces, each
with its own policy globs, read/grep limits. Parsed into a typed struct with
`KnownFields(true)` so an unknown key is an *error*, not a silent typo
([config/config.go:77]). Validated semantically at startup ([config/config.go:94]):
≥ 1 workspace, unique names, a `default` must exist (it's the fallback for the
`workspace` param), each `root` exists and is a directory, globs compile,
`read.maxBytes` positive, and (HTTP mode only) port in range + resolved bearer
≥ 32 bytes.

### 4.1 Secrets never live in YAML

`config.yaml` holds **no secret values**. A secret field takes a *reference*:

```yaml
auth:
  bearerToken:
    env: SHIM_BEARER_TOKEN     # name of an env var to read the value from
```

Resolution order ([config/secrets.go]): read the `.env` file (`-env`, default
`./.env`) via `godotenv`, then overlay `os.Environ()` so the **OS environment
overrides** dotenv (a deployment can inject the token without a file), then
resolve each `{ env: NAME }` against the merged map. A missing/empty referenced
var is a startup error. A plain-string literal is *allowed but discouraged* for
`bearerToken`. Commit `config.example.yaml` + `.env.example`; gitignore the real
`config.yaml` + `.env`.

For **rotation**, `auth` accepts a list `bearerTokens: [ {env: A}, {env: B} ]` in
place of the single `bearerToken` (set one or the other, not both); each resolves
the same way and each must be ≥ 32 bytes. See §2.3.

### 4.2 Shape

```yaml
server:
  host: 127.0.0.1                 # localhost only; ngrok fronts it
  port: 3850
  publicURL: https://<reserved-subdomain>.ngrok.app
auth:
  bearerToken: { env: SHIM_BEARER_TOKEN }
workspaces:                       # one or more; `workspace` param selects, default "default"
  - name: default
    root: /absolute/path/to/tree  # this workspace's os.Root sandbox
    respectGitignore: true        # via grrep IgnoreSet (works on any tree)
    policy:                        # allow/deny atop os.Root; blockGlobs wins
      allowGlobs: ["**/*.md", "**/*.txt", "docs/**", "README*"]
      blockGlobs: [".git/**", "**/.env", "**/.env.*", "**/*secret*", "**/*.pem",
                   "**/*.key", "**/id_rsa*", "**/.ssh/**", "**/node_modules/**"]
    read:  { maxBytes: 1000000 }
    grep:  { enabled: true, workers: 0, maxMatches: 500 }   # workers 0 = GOMAXPROCS
log:
  level: info
```

Permissions (`policy` / `read` / `grep` / `respectGitignore`) are **per-workspace,
never global** — one workspace's policy can never widen another's. This mirrors the
security model: containment is per-`os.Root`, and policy rides each root
independently.

---

## 5. MCP schema (the wire surface)

The transport is **Streamable HTTP**: `POST /mcp` carries JSON-RPC 2.0
(`jsonrpc.Endpoint`), `GET /mcp` is the SSE stream; a `-stdio` mode reuses the
same dispatch over a local pipe with no bearer ([cmd/shim/main.go]). MCP *is*
JSON-RPC 2.0, so the whole surface is the reflection-based jsonrpc registry from
`github.com/mnehpets/http`; slash-named methods use the `_ jsonrpc:"…"` struct-tag
override ([mcp/server.go:48]).

### 5.1 Protocol methods

| Method | Behavior |
|---|---|
| `initialize` | Negotiate protocol version (supported, newest-first: `2025-06-18`, `2025-03-26`, `2024-11-05`; unknown → our newest). Advertise `{ capabilities: { tools: {} } }` + serverInfo `{ name: "workspace-mcp", version }` + an `instructions` string (§5.5). ([mcp/server.go:67]) |
| `notifications/initialized` | Accept; no response. |
| `ping` | Respond with `{}`. |
| `tools/list` | Return the catalog (§5.3). Workspace-*independent*; the `workspace` param's enum is filled from config so the model sees valid names + the default. ([mcp/server.go:123]) |
| `tools/call` | The gate. Unknown tool name → JSON-RPC `InvalidParams` (protocol error). Domain failures (bad path, unknown workspace, …) are **not** JSON-RPC errors — they come back as a normal result with `isError: true` and a machine-readable code (§5.4). ([mcp/server.go:154]) |

### 5.2 Result envelope

Every `tools/call` result is wrapped as MCP content. The tool's structured value
is JSON-serialized into a single text block ([mcp/server.go:245]):

```jsonc
// success
{ "content": [ { "type": "text", "text": "<JSON of the tool result>" } ] }
// domain failure
{ "content": [ { "type": "text", "text": "{\"code\":\"…\",\"message\":\"…\",\"reason\":\"…\"}" } ],
  "isError": true }
```

### 5.3 Tools

Six tools today ([mcp/tools.go:58]). **Every tool except `workspace_list` takes a
`workspace` string** (optional; omit → `"default"`; advertised as a JSON-Schema
`enum` of configured names). All `path` args are workspace-relative and resolve
through that workspace's `os.Root`. Schemas use `additionalProperties: false`.
Every tool also carries read-only MCP **annotations** (§5.5).

> Notation below: `in` = input properties (★ = required), `out` = the structured
> JSON inside the result's text block.

#### `workspace_list`
Discover the configured trees. No params.
- **out** `{ "workspaces": [ { "name": string, "isGitRepo": bool } ] }`. Never
  exposes roots. *(Planned: `description` + `wellKnownFiles` — PLAN §12.12.)*

#### `tree_list`
List directory entries.
- **in** `workspace`, `path` (dir, default root), `recursive` (bool)
- **out** `{ "entries": [ { "path": string, "type": "file"|"dir", "size": int } ] }`
- Backed by `os.Root` `ReadDir`/`WalkDir`, filtered by the workspace's `IgnoreSet`
  (when `respectGitignore`) and its policy globs. ([mcp/tools.go:171])

#### `file_read`
Read one allowed file.
- **in** `path` ★, `workspace`, `maxBytes` (capped by `read.maxBytes`)
- **out** `{ "path": string, "content": string, "truncated": bool, "binary": bool, "notice"?: string }`
- Opens via `os.Root`; binary detected by a NUL probe over the first 8 KB — when
  binary, `content` is omitted and `binary: true`. Oversize → `truncated: true`
  plus a steering `notice` (§5.5). ([mcp/tools.go:284]) *(Planned: `startLine`/`endLine` ranges — PLAN §12.13; raw
  base64 binary delivery — PLAN §12.15.)*

#### `tree_find`
Fuzzy filename search.
- **in** `query` ★, `workspace`, `limit` (default 100)
- **out** `{ "files": [ string, … ], "truncated"?: bool, "notice"?: string }`
  (workspace-relative paths; capped at `limit` → `truncated` + steering `notice`,
  §5.5). ([mcp/tools.go:362])

#### `tree_grep`
Concurrent content search (vendored grrep core). Requires `grep.enabled`.
- **in** `pattern` ★, `workspace`, `path` (subtree, default root),
  `fixedString` (default **true** = literal substring; false = Go regexp with a
  literal pre-filter), `caseInsensitive`, `wordBoundary`
- **out** `{ "matches": [ { "path": string, "line": int, "text": string } ],
  "truncated": bool, "notice"?: string }`
- `fastwalk` traversal, policy + `IgnoreSet` + `.git`/dotfile skip, NUL-byte binary
  skip, each leaf opened via `os.Root`, worker pool sized by `grep.workers`, capped
  at `grep.maxMatches` (`truncated` + steering `notice` when hit, §5.5). Disabled →
  `GREP_DISABLED`; bad regex → `INVALID_PATTERN` (no walk). ([mcp/tools.go:392])

#### `git_status`
Read-only status, git-repo workspaces only.
- **in** `workspace`
- **out** `{ "branch": string, "files": [ { "path": string, "status": string } ] }`
- go-git `Worktree().Status()` + `repo.Head()`; non-git workspace → `NOT_A_GIT_REPO`.
  No mutation, no `git` binary. ([mcp/tools.go:438])

**Hard exclusions** — never in `tools/list`, always rejected in `tools/call`: any
write/create/delete/move/rename (until the deferred patch task), any shell/command
execution, any Git mutation.

### 5.4 Error spec

Domain failures return `isError: true` with `{ code, message, reason? }`
([mcp/server.go:194]). Codes:

| Code | When | `reason` values |
|---|---|---|
| `UNKNOWN_WORKSPACE` | named workspace not configured | — |
| `NOT_A_GIT_REPO` | git tool on a non-git workspace | — |
| `GREP_DISABLED` | `tree_grep` where `grep.enabled` is false | — |
| `POLICY_DENIED` | path blocked or outside the sandbox | `absolute_path`, `traversal`, `outside_root`, `blocked_glob`, dotfile reason |
| `NOT_FOUND` | missing path / path is a directory (file_read) | — |
| `INVALID_PATTERN` | bad regex (`fixedString:false`); no walk performed | — |
| `INVALID_ARGS` | arguments fail to unmarshal | — |
| `INTERNAL` | unexpected server error (no detail leaked) | — |

Two failures are *not* in this envelope: an **unknown tool name** is a JSON-RPC
`InvalidParams` protocol error, and a **missing/invalid bearer** is an HTTP `401`
with no body detail. `NOT_FOUND` is deliberately indistinguishable from a policy
denial where distinguishing would leak the existence of a blocked path.

### 5.5 Orientation — how the model learns what to do

The server is only useful if the model knows *what it is for* and *how to
navigate*. Tool descriptions are prompts: every word shapes selection, so we
spend orientation cheaply at three layers, designed to be redundant because
general-purpose hosts may ignore any one of them.

- **Server `instructions`** ([mcp/server.go]) — a short string returned at
  `initialize`, before any reasoning: the server's identity (a read-only window
  onto local trees for research/workflow, *not* a coding agent; the model does the
  analysis), the orient-first flow (`workspace_list` → read README → locate →
  read), and the hard constraints (read-only; default-deny, so `NOT_FOUND` /
  `POLICY_DENIED` are *expected answers*, not transient errors to retry).
  **Best-effort:** the MCP spec lets servers send it, but some hosts ignore it
  (claude.ai's handling is unverified — see PLAN §12.16). So it is never
  load-bearing; the two layers below stand alone.
- **Tool descriptions** ([mcp/tools.go]) — each says *what* it does, *when* to
  reach for it, and its *boundary vs siblings* (e.g. `tree_find` = locate by name,
  `tree_grep` = locate by content; `file_read` follows the locators).
  `workspace_list` is worded as the **"start here"** entry point — the natural
  first call that survives a host dropping `instructions`.
- **Tool annotations** ([mcp/tools.go]) — every tool carries MCP
  `{ readOnlyHint: true, openWorldHint: false }` plus a human `title`. A
  machine-readable restatement of "this only reads, and only from the local
  sandbox," which clients can surface in UI and trust decisions.

**Truncation steers, it doesn't just flag.** When a result is capped
(`file_read` by bytes, `tree_grep`/`tree_find` by their caps) the response adds a
`notice` string telling the model how to get the rest — narrow the `path`, tighten
the `pattern`, raise `maxBytes`/`limit` — instead of letting it treat a partial
result as complete.

---

## 6. Transport & deployment

- **Streamable HTTP first.** `POST /mcp` (JSON-RPC) + `GET /mcp` (SSE keepalive via
  `SSERenderer`), both behind the bearer `Processor`; `/healthz` is unauthenticated
  ([cmd/shim/main.go:95]).
- **stdio mode** (`-stdio`) is **mostly for testing and local development** (MCP
  Inspector, Claude Desktop/Code as a subprocess) — it is *not* the primary use
  case. The point of this server is remote: claude.ai / ChatGPT reaching local
  resources over HTTPS, which is the HTTP+tunnel path above. stdio uses the same
  dispatch and tool gating, no HTTP listener, no bearer (trusted local pipe). It
  currently reuses the HTTP path via a synthetic `httptest` round-trip per message
  — a known shim to be replaced by a transport-agnostic `jsonrpc` dispatch entry
  point later ([cmd/shim/main.go:147]). Note claude.ai itself only connects to
  *remote* MCP servers, so stdio is never the claude.ai path.
- **Exposure.** The server binds `127.0.0.1`; **ngrok (or equivalent) exposes only
  this server** over HTTPS, with edge auth (OAuth / IP-allow / reserved domain) on
  top of the bearer. claude.ai reaches it as a custom connector / remote MCP.

---

## 7. Deferred & future (pointers, not commitments)

The payoff of going standalone is a **`tree_patch`** write tool done right —
flag-gated, default off, never started before the read surface is solid: parse a
unified/git diff with `go-gitdiff`, require context to match (reject on drift),
write **through `os.Root`** restricted to `allowGlobs`, audit a content hash, never
commit/push. The research-workflow cousin is "capture results back" (append a
finding to an inbox note) — also a write, same umbrella.

Other candidates, each measured against the §1 thin-pipe rule: read-only `git_log`
(whole-workspace + per-file), `git_blame`, `git_show`, `git_diff`; richer
`workspace_list` (config/README-derived `description` + `wellKnownFiles`); a
`tree_metadata` tag/frontmatter rollup (the one orientation-aggregation exception);
ranged + raw-binary `file_read`; an author-controlled repo manifest. Anything that
would have the server rank by meaning, build graphs, or summarize is **out** — that
is the model's job.
