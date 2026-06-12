# Design — workspace-mcp

This document records the *design decisions* and the reasoning behind them, the
division of functionality across packages, the configuration file, and — most
importantly — the **MCP schema** (the wire surface seen by claude.ai / ChatGPT).

This is the durable reference. Where this doc and the code disagree, the code
wins — file/line references are given so claims stay checkable.

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
  parse them (`file_read` `allowBinary` → base64 + `mimeType`), not to build extractors.
- **Orientation is the one exception, and only when it's aggregation the model
  can't cheaply do** — e.g. a corpus-wide tag/frontmatter rollup, or per-workspace
  descriptions. A table of contents, not an analysis engine.

### Non-goals (hard)

No remote shell or command runner; no Git automation (commit/push/branch/rebase);
no file delete/move/rename; no second LLM/agent loop; no LSP/symbol index; no
multi-user SaaS; **no external binaries at all** (single self-contained Go binary).
Single-user local developer tool. Writing **is** supported, but only as an
explicit per-workspace opt-in (`write.enabled`, default off) over three exact-byte
ops — see §5.3; it is never the default posture and never a diff/patch engine.

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

`os.Root` is essential for **model-supplied paths** (`file_read` and the write ops
`file_create`/`file_overwrite`/`file_replace`): the path comes straight from the
model and may be hostile. The write ops resolve through the *same* `os.Root` and
clear the *same* `policy.CheckFile` a read does (block wins), so a write can never
escape containment or reach a blocked path — the writable surface is exactly the
readable one.

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

- **Read-only is the default posture.** A workspace writes only when its config
  sets `write.enabled: true` (§5.3); with it off the three write tools are absent
  from `tools/list` and any forced call returns `READ_ONLY`, so the `*os.Root` is
  used only for read methods. Where writes are granted they still ride that
  workspace's `*os.Root` + `policy.CheckFile`, so read-only remains the default
  build posture, not just a config value.
- **Auth is layered:** a server-wide bearer token (constant-time compared, ≥ 32
  bytes, sourced from `secrets.env`/OS env — never from `config.yaml`), and/or an
  OAuth 2.0 authorization code flow (for clients such as claude.ai that only support
  OAuth). 401 on missing/invalid with no hint as to which. AuthN is server-wide;
  **AuthZ is per-workspace policy**.
  - **Multiple static tokens for rotation.** Config accepts either a single
    `auth.bearerToken` or a list `auth.bearerTokens` (not both). The server accepts
    a request bearing *any* configured token, so an old and new token can both be
    valid during an overlap window — add the new, switch clients over, drop the old
    — without lockstep. The presented token is digested and compared against every
    expected one **without short-circuiting**, so timing reveals neither the token
    nor which (if any) matched ([mcp/auth.go]).
  - **OAuth access and refresh tokens** are self-contained AEAD blobs
    (ChaCha20-Poly1305), with keys derived via HKDF from `auth.oauth.clientSecret`
    using **distinct info strings per token type** — so an access token cannot be
    presented as a refresh token, or vice versa. No server-side token store;
    validation is decryption + expiry check. Rotating the client secret immediately
    invalidates all outstanding tokens of both kinds. Access tokens expire after
    1 hour, refresh tokens after 30 days; auth codes (held in memory, single-use)
    after 2 minutes. The token endpoint issues a fresh access/refresh pair for both
    the `authorization_code` and `refresh_token` grants ([mcp/oauth.go]).
- **Audit log:** every call records method, tool, workspace, resolved path(s),
  allow/deny + reason, and byte/match counts — never file contents, never the
  token ([mcp/server.go:154] `ToolsCall` → `s.log.ToolCall(ev)`).

### 2.4 The trust boundary ends at the tunnel frontend

The bearer/OAuth layer and the `os.Root` boundary protect against unauthorized
callers and path escape, but they say nothing about the *channel* once a
third-party tunnel fronts it. Both built-in tunnels (ngrok and zrok) terminate
TLS at **their own edge** — the public certificate is the operator's, not ours.
So at the point of termination the entire MCP stream is plaintext to the tunnel
operator: the bearer token, every tool argument, and every byte of file content
in responses. A malicious or compromised frontend is fully in-path — it can read
the whole conversation **and** inject or rewrite it (hand the model
attacker-chosen file contents, or alter a write tool's arguments). This is
inherent to *any* remote proxy whose cert you don't control; it is not specific
to zrok.

- **Name takeover — the sharper, zrok-specific variant.** claude.ai persists the
  connector as `<uniqueName>` + the bearer token and **replays that token on
  every request**, so whoever answers at that name receives the credential. If
  the namespace name were first-come / ephemeral, an attacker could claim it
  during any downtime window — a rebuild, a crash, the gap between `release()`
  and the next start — and passively harvest the next token claude.ai sends, with
  no TLS break required. zrok's name reservation being **account-scoped and
  persistent** closes that window: the name stays ours even while nothing is
  serving it. This is why [cmd/workspace-mcp/zrok.go] reserves the name
  idempotently and **never** calls `DeleteShareName` — auto-releasing on shutdown
  would re-open the takeover window. The durable *name* reservation, not the
  ephemeral share/environment, is the security boundary.
- **The only real fix is a frontend whose cert we control** — a reverse proxy on
  our own domain, terminating TLS on hardware we run, rather than a shared
  third-party edge. Until then the tunnel operator must be treated as in-path:
  keep bearer tokens rotatable (§2.3) so a leak is recoverable, bound the token's
  blast radius, and prefer the no-tunnel deployment (`127.0.0.1:PORT` behind a
  self-hosted reverse proxy, §6) whenever the workspace contents or the write
  surface are sensitive. The built-in tunnels optimize for reach and convenience,
  not for confidentiality against the operator.

---

## 3. Division of functionality (package map)

All application logic lives in `mcp/` — config, secrets, auth, registry, sandbox,
policy, search, logging, and the protocol surface are all one package, avoiding the
nesting overhead of `internal/` subdirectories for what is a single-binary app.
`grrep/` (vendored) and `gitaware/` remain separate because they have distinct
ownership and trust properties (see §2.2). Dependency arrows point one way: the
`mcp` package orchestrates; `grrep` and `gitaware` know nothing about MCP.

```
cmd/workspace-mcp/main.go   Wiring + transports. Loads config+secrets, builds the
                     workspace registry, mounts HTTP routes (/healthz, POST/GET
                     /mcp) or the -stdio loop, and dispatches the listener: plain
                     TCP, built-in ngrok (serveNgrok), or built-in zrok
                     (serveZrok). The only file that knows about net/http and
                     process lifecycle.
cmd/workspace-mcp/zrok.go   Built-in zrok tunnel (alternative to ngrok). An
                     in-memory env_core.Root drives the zrok Go SDK purely from
                     config+secrets — no ~/.zrok, no ZROK_* ambient state, no
                     identity file on disk. Reserves a stable share name, reaps
                     its own leaked ephemeral environments on restart, retries the
                     controller's transient 500s, releases the share+env on
                     shutdown. See §6.

mcp/                 Everything except the vendored search core and git layer.
  config.go          Typed YAML load with KnownFields(true) + semantic Validate().
  secrets.go         dotenv + os.Environ merge, {env: NAME} reference resolution.
                     No I/O beyond reading files.
  auth.go            Bearer-token + OAuth 2.0 endpoint.Processor. Constant-time
                     compare; 401 with no disclosure. Wired ahead of every route
                     but /healthz.
  oauth.go           OAuth 2.0 authorization code flow (required for claude.ai
                     connectors). Self-contained AEAD access tokens; no server-side
                     store.
  registry.go        The workspace registry: name → Workspace{ *os.Root, Policy,
                     Ignore, Read/Grep settings, IsGitRepo }. Built once at startup.
                     Keyed by the URL path segment (§5.0); also resolves workspace
                     descriptions and wellKnownFiles at startup.
  handler.go         BuildHandler: HTTP routes. One MCP endpoint per workspace at
                     /mcp/<name> (the workspace-per-URL router), /healthz, OAuth
                     discovery + endpoints, and the SSE keepalive stream.
  root.go            The os.Root wrapper. Clean() rejects absolute/`..` paths;
                     Open/Stat/ReadDir/WalkDir take workspace-relative paths and
                     stay in the sandbox. The containment primitive.
  policy.go          Glob allow/deny (block wins) + dotfile rule. CheckFile/CheckDir
                     return {Allowed, Reason}. The soft layer atop root.go.
  walk.go            fastwalk traversal: .git/dotfile skip, IgnoreSet + policy
                     filter, NUL-byte binary skip, each leaf opened via os.Root,
                     worker pool, match cap.
  search.go          tree_search engine: path-glob boundary + AND-combined where
                     predicates + frontmatter-fence splitting. Drives walk.go.
  write.go           The opt-in write surface: file_create/file_overwrite/
                     file_replace. Shared writeGate (write.enabled → Clean →
                     policy.CheckFile), base_sha256 optimistic-concurrency check,
                     exact-byte match/replace. No diff parser, no git automation.
  log.go             Redacting slog logger. ToolEvent carries the per-call record;
                     never logs content or the token.
  server.go          The protocol surface and the gate: initialize / tools/list /
                     tools/call / ping, error model, dispatch table.
  tools.go           Tool catalog + JSON Schemas + one handler per tool, each
                     running the full gate (enablement → path policy →
                     root/search/gitaware → limits → audit) on the endpoint's
                     bound workspace.

grrep/               Vendored from bep/grrep (Apache-2.0, SPDX retained, see
                     NOTICE). match.go verbatim (Matcher + literal pre-filter);
                     scan.go adapted to emit structured {path,line,text} instead of
                     CLI stdout; ignore.go = IgnoreSet (nested .gitignore/.ignore).

gitaware/            go-git (pure Go) git-awareness, metadata only. detect.go
                     (is it a repo?), status.go (Worktree().Status() + branch +
                     upstream tracking), upstream.go (ahead/behind against the
                     local remote-tracking ref, merge-base walk, no network),
                     tracked.go (tracked-file enumeration). Never a content path.

```

The shape of a tool call, end to end ([mcp/server.go], [mcp/tools.go]):

```
POST /mcp/<name> → route to that workspace's endpoint (mcp/handler.go; unknown → 404)
  → bearer (mcp/auth.go) → jsonrpc dispatch → ToolsCall (Server bound to the workspace)
  → allowlist tool name (else JSON-RPC InvalidParams)
  → handler: unmarshal args
           → per-workspace enablement (grep on? git?) → GREP_DISABLED / NOT_A_GIT_REPO
           → root.Clean(path) + policy.CheckFile/Dir  → POLICY_DENIED
           → do the one read (mcp/root.go / search / gitaware)
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
([mcp/config.go]). Validated semantically at startup ([mcp/config.go]):
≥ 1 workspace, unique names, a `default` must exist (the conventional endpoint
`/mcp/default` and the implicit stdio target), each `root` exists and is a directory, globs compile,
`read.maxBytes` positive, at most one of `ngrok`/`zrok` enabled (each replaces the
local listener) with its token present when on, and (HTTP mode only) port in range
(skipped when a tunnel is active) + resolved bearer ≥ 32 bytes.

### 4.1 Secrets never live in YAML

`config.yaml` holds **no secret values**. A secret field takes a *reference*:

```yaml
auth:
  bearerToken:
    env: MCP_BEARER_TOKEN     # name of an env var to read the value from
```

Resolution order ([mcp/secrets.go]): read the `secrets.env` file (`-env`, default
`./secrets.env`) via `godotenv`, then overlay `os.Environ()` so the **OS environment
overrides** dotenv (a deployment can inject the token without a file), then
resolve each `{ env: NAME }` against the merged map. A missing/empty referenced
var is a startup error. A plain-string literal is *allowed but discouraged* for
`bearerToken`. Commit `example/config.example.yaml` + `example/secrets.example.env`; gitignore the real
`config.yaml` + `secrets.env`.

For **rotation**, `auth` accepts a list `bearerTokens: [ {env: A}, {env: B} ]` in
place of the single `bearerToken` (set one or the other, not both); each resolves
the same way and each must be ≥ 32 bytes. See §2.3.

### 4.2 Shape

```yaml
server:
  host: 127.0.0.1                 # localhost only; used when no tunnel enabled
  port: 3850
  ngrok:                          # built-in tunnel; host/port ignored when enabled
    enabled: true
    authtoken: { env: NGROK_AUTHTOKEN }
    domain: my-host.ngrok.app     # optional: pin a stable domain
  zrok:                           # alternative built-in tunnel (enable at most one of ngrok/zrok)
    enabled: false
    enableToken: { env: ZROK_ENABLE_TOKEN }   # zrok account token; secrets only, never YAML
    apiEndpoint: https://api-v2.zrok.io       # optional; this is the default
    frontend: public              # optional public frontend namespace (default)
    uniqueName: my-workspace-mcp  # optional reserved name → stable URL across restarts
auth:
  bearerToken: { env: MCP_BEARER_TOKEN }
  oauth:                          # OAuth 2.0 authorization code flow (required by claude.ai)
    clientID: workspace-mcp       # public; stored directly in config
    clientSecret: { env: OAUTH_CLIENT_SECRET }
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
same dispatch over a local pipe with no bearer ([cmd/workspace-mcp/main.go]). MCP *is*
JSON-RPC 2.0, so the whole surface is the reflection-based jsonrpc registry from
`github.com/mnehpets/http`; slash-named methods use the `_ jsonrpc:"…"` struct-tag
override ([mcp/server.go:48]).

### 5.0 Workspace-per-URL routing

Workspace selection is by **route**, not by argument: each configured workspace
gets its own MCP endpoint at `POST/GET /mcp/<name>`, so a claude.ai connector URL
*is* a workspace ([mcp/handler.go], `BuildHandler`). The path segment maps to a
registry entry; an unknown segment is a plain HTTP **404** (no matching route),
not a domain error. This collapses what used to be step (1) of the per-call gate
(resolve `workspace` → `UNKNOWN_WORKSPACE`) into routing done once at the HTTP
layer, and removes the `workspace` argument from every tool. Auth stays
server-wide (one bearer/OAuth across all paths); per-workspace policy remains the
AuthZ layer. **stdio** has no URL, so it serves exactly one workspace — implicit
when only one is configured, else named via `-workspace` ([cmd/workspace-mcp/main.go],
`selectStdioWorkspace`).

### 5.1 Protocol methods

| Method | Behavior |
|---|---|
| `initialize` | Negotiate protocol version (supported, newest-first: `2025-11-25`, `2025-06-18`, `2025-03-26`, `2024-11-05`; unknown → our newest). Advertise `{ capabilities: { tools: {} } }` + serverInfo `{ name: "workspace-mcp", version }` + an `instructions` string (§5.5). ([mcp/server.go:67]) |
| `notifications/initialized` | Accept; no response. |
| `ping` | Respond with `{}`. |
| `tools/list` | Return the catalog (§5.3). The workspace is fixed by the endpoint URL (§5.0), so no tool takes a `workspace` param. ([mcp/server.go]) |
| `tools/call` | The gate. Unknown tool name → JSON-RPC `InvalidParams` (protocol error). Domain failures (bad path, non-git tree, …) are **not** JSON-RPC errors — they come back as a normal result with `isError: true` and a machine-readable code (§5.4). ([mcp/server.go]) |

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

Four read tools always ([mcp/tools.go]), plus three write tools advertised **only**
when the workspace sets `write.enabled` (§5.3 write surface). **No tool takes a
`workspace` argument** — the workspace is fixed by the endpoint URL (§5.0). All
`path` args are workspace-relative and resolve through that workspace's `os.Root`.
Schemas use `additionalProperties: false`. The read tools carry read-only MCP
**annotations** (§5.5); the write tools carry `readOnlyHint: false` (and
`destructiveHint: true` for overwrite/replace, false for create).

> Notation below: `in` = input properties (★ = required), `out` = the structured
> JSON inside the result's text block.

#### `workspace_info`
Orientation for *this* workspace (the tree this endpoint is bound to). No params.
Returns `orientation` — the same text as the initialize `instructions` string
(§5.5) **as of the time of the call** — so the tool is the dependable mirror for
hosts that ignore server `instructions`. Because `workspace_info` re-scans the tree
root on every call (task 25), `orientation` reflects the current on-disk state and
may differ from the connect-time `instructions` if well-known files (README, etc.)
were added or changed since the session started. The structured fields are the
machine-readable source the prose is built from. It is *not* a mandated first call
— a host that surfaced `instructions` already has everything here.
- **out** `{ "name": string, "isGitRepo": bool, "description": string,
  "wellKnownFiles": [ string, … ], "orientation": string, "preview"?: { "path":
  string, "content": string, "truncated": bool, "totalLines"?: int } }`. Never
  exposes the root path.
- `preview` inlines the start of the highest-priority well-known file (capped at
  200 lines / 16 KB, bounded by `read.maxBytes`, read live through `os.Root`,
  binary skipped) so the common "orient by reading the README" move needs no
  follow-up `file_read`. Absent when there is no well-known file.
- `wellKnownFiles` lists the orientation files present at the root, auto-detected
  by a closed convention recognizer (§5.5) — a curated stem set (`readme`, `index`,
  `_index`, `contents`, `toc`, `overview`, `about`, `agents`, `claude`) matched
  case-insensitively and extension-agnostically, presence-only, policy-gated,
  priority-ordered and capped at 5. `description` is the config `description` if
  set, else the first section of the highest-priority detected file (heading prose,
  whitespace-collapsed, capped). Both computed once at startup
  ([mcp/registry.go]).

#### `file_read`
Read one allowed file, optionally a line span.
- **in** `path` ★, `maxBytes` (capped by `read.maxBytes`),
  `startLine`/`endLine` (1-based inclusive, either omittable → open-ended),
  `allowBinary` (bool)
- **out** `{ "path": string, "content": string, "truncated": bool, "binary": bool,
  "sha256": string, "encoding"?: "base64", "mimeType"?: string, "startLine"?: int,
  "endLine"?: int, "totalLines"?: int, "notice"?: string }`
- Opens via `os.Root`; binary detected by a NUL probe over the first 8 KB. By
  default a binary file is flagged (`binary: true`, no `content`, regardless of any
  range); with `allowBinary` its raw bytes are returned base64-encoded
  (`encoding: "base64"` + a detected `mimeType` — extension type, else content
  sniff) for the platform to parse, the server extracting nothing. Same
  `os.Root`/policy/byte-cap limits as a text read. Oversize → `truncated: true`
  plus a steering `notice` (§5.5). With a range, the scan is bounded by
  `read.maxBytes`, the returned span is then capped by the arg `maxBytes`, and the
  line fields echo the resolved span + `totalLines` so the model can page;
  out-of-bounds ranges clamp, bad bounds → `INVALID_RANGE`. Every response carries
  `sha256` — the hex SHA-256 of the file's **full** bytes (the *same* hash
  `base_sha256` checks), computed over the whole file even for a ranged or
  `maxBytes`-truncated read, so a read-then-write loop carries it straight into a
  later `file_replace`/`file_overwrite` `base_sha256` with no extra round-trip
  (binary files report it too; empty for a file past `read.maxBytes`, which is
  uneditable anyway). ([mcp/tools.go])

#### `tree_search`
One tool to locate *and browse* files — by path, by content, or both — replacing
the former `tree_find`/`tree_grep` split (the boundary encoded two match
*vocabularies*, glob/name vs literal-or-regex content, not two tools) and also
the former `tree_list`: a where-less path-glob query enumerates files with their
sizes, which is all a directory listing offered (a separate listing tool was
redundant once any subtree could be enumerated by glob, and directory entries
themselves carried no information the file paths don't). Concurrent content
search over the vendored grrep core.
- **in** `workspace`, `path` (a doublestar **glob**, both the search boundary and
  a name filter, e.g. `docs/**/*.{md,txt}`; **omit (or `**/*`) for the whole tree**
  — `**` crosses directories, a single `*` does not, so `*` is root-level only),
  `where` (a list of body predicates, **AND-combined** — a file must
  satisfy every one; each `{ text ★, fixedString` (default **true** = literal
  substring; false = Go regexp with a literal pre-filter)`, caseInsensitive,
  wordBoundary }`), `includeMatches` (default **true**), `includeMetadata`
  (default **false**)
- **out** `{ "files": [ { "path": string, "size": int, "matches"?: [ { "line":
  int, "text": string } ], "metadataMatches"?: [ … ], "metadata"?: string } ],
  "truncated": bool, "notice"?: string }`. `size` (byte size, from the walk's dir
  entry) is always present.
- A **pure path-glob query (no `where`)** enumerates matching files with their
  sizes — it only walks, does **not** open files, and does **not** require
  `grep.enabled`. This is how a client answers "what files exist?" / browses
  structure — omit `path` (or `**/*`) for the whole tree, `docs/**` for a subtree,
  `*` for just the root level; there is no separate directory-listing tool.
  Content predicates additionally need `grep.enabled`.
- **Frontmatter-aware, parser-free.** A leading `---`…`---` fence is located
  *textually* (never a YAML parse): matches inside it are split into
  `metadataMatches` (vs body `matches`), so a *declared* topic
  (`tags: [california]`) is distinguishable from an incidental body mention, and
  `includeMetadata` returns the raw, unparsed fence body as `metadata`. Combined
  with a where-less enumeration it turns a plain listing into a triage pass — the
  model reads each file's own title/tags/summary and picks the right targets in
  one call instead of inferring from filenames. (This is the built-in "cheap
  orientation" stance of §1 — author-declared signal handed over raw, no
  server-side ranking.) This path stays cheap: it needs no `grep.enabled`, and it
  reads only the leading fence region per file (streamed, stopping at the closing
  `---`, bounded by a 64 KiB probe — capped further by `read.maxBytes`), never the
  body; a fence that does not close within the probe yields no metadata, the file
  still listed. Field-scoped predicates (matching *within* a named field) stay out
  of scope —
  a structured-metadata query is a non-goal (§1): the server finds the fence,
  never queries inside it.
- `fastwalk` traversal narrowed to the glob's literal prefix, policy + `IgnoreSet`
  + `.git`/dotfile skip, NUL-byte binary skip, each leaf opened via `os.Root`,
  worker pool sized by `grep.workers`, capped at `grep.maxMatches` (`truncated` +
  steering `notice` when hit, §5.5). Content read is bounded by `read.maxBytes`.
  `where` with grep disabled → `GREP_DISABLED`; bad regex → `INVALID_PATTERN` (no
  walk). `fastwalk` runs its callback **concurrently**, so the walk serializes its
  shared state — the results slice and the (non-thread-safe) gogitignore tree's
  `Match`/`EnsureNode` — under a mutex; the candidate scan then runs through its
  own worker pool writing to per-index slots (deterministic, path-sorted before
  the cap). ([mcp/walk.go], [mcp/tools.go])

#### `git_status`
Read-only status, git-repo workspaces only.
- **in** (none)
- **out** `{ "branch": string, "files": [ { "path": string, "status": string } ],
  "upstream"?: { "ref": string, "ahead": int, "behind": int, "inSync": bool,
  "capped"?: bool } }`
- go-git `Worktree().Status()` + `repo.Head()`; non-git workspace → `NOT_A_GIT_REPO`.
  No mutation, no `git` binary. ([mcp/tools.go], [gitaware/status.go])
- **`upstream`** — present only when a tracking branch is configured and its
  remote-tracking ref exists locally. Reports how many commits the local branch is
  ahead and behind `ref` (e.g. `refs/remotes/origin/main`). **Counts are as of the
  last fetch — no network call is made.** `inSync` is true when both counts are zero.
  `capped: true` means the walk hit the 1000-commit limit and counts are lower bounds.
  `upstream` is `null` (omitted) when no upstream is configured, the tracking ref has
  never been fetched, HEAD is detached, or the branch has no commits. ([gitaware/upstream.go])

#### `git_diff`
Read-only working-tree diff, git-repo workspaces only — the content companion to
`git_status`. Emits a **standard unified diff string** in a thin JSON envelope (not
per-hunk JSON): LLMs read unified diffs natively, and this tool only ever *emits*
them. Rationale recorded in [PLAN-git-diff.md] §0.1.
- **in** `path`? (a file or a directory prefix to scope to a subtree), `staged`?
  (index-vs-HEAD like `git diff --cached`; default worktree-vs-index like `git diff`)
- **out** `{ "files": [ { "path", "change", "additions", "deletions", "binary"?,
  "tooLarge"?, "symlink"? } ], "diff": string, "truncated": bool, "notice"?: string }`.
  `change` ∈ `added | modified | deleted | untracked`. Untracked files appear as
  all-new in the default (unstaged) mode. `files` is always complete even when `diff`
  is capped — so truncation is steerable ("re-run scoped to that path"). Empty diff
  (clean tree / unchanged scoped path) is success with `notice: "no changes"`.
- **Security — it returns content, so it gates like a read, unlike `git_status`.**
  Worktree bytes are read through the workspace `os.Root` via an injected
  `WorktreeReader` (symlink-/TOCTOU-safe); blob bytes come from go-git's object
  store under `.git/`. A scoped `path` is `Clean`+`policy.CheckFile`-gated
  (blocked → `POLICY_DENIED`, `..`/absolute → mapped); an unscoped diff silently
  excludes every policy-denied file, so a dirty `.env` never leaks. Symlinks are
  skipped (flagged, target never read). Per-file and total caps at `read.maxBytes`;
  binary/over-limit/symlink files are listed with a one-line marker, content
  skipped. No rename detection (a rename shows as delete + add). go-git, no `git`
  binary, no mutation. ([gitaware/diff.go], [mcp/tools.go])

#### Write surface — `file_create` / `file_overwrite` / `file_replace` (opt-in)

Three explicit byte-level ops mirroring the Claude Code edit tools — **not** a diff
parser, no git automation. Present in `tools/list` (and the `instructions` prose
flips to mention editing) **only** when the workspace sets `write.enabled: true`;
otherwise they are absent and any forced call returns `READ_ONLY`. Each runs the
shared `writeGate` (`write.enabled` → `Clean` → `policy.CheckFile`), so a write
target clears the *same* allow/block a read does, through the same `os.Root`. Every
op writes **raw bytes with zero normalization** (no whitespace trim, no line-ending
rewrite) — silent normalization would turn a safe rejection into a wrong-place
edit. The human reviews `git diff` and commits out of band; the server never
commits, pushes, deletes, moves, or renames. ([mcp/write.go])

#### `file_create`
- **in** `path` ★, `contents` ★
- **out** `{ "path": string, "bytesWritten": int, "sha256": string }`
- New file only (`O_CREATE|O_EXCL` through `os.Root`); an existing path is never
  clobbered → `PATH_EXISTS` ("use file_overwrite"). Missing parent dirs are
  auto-created inside the sandbox. No `base_sha256`/`dry_run` — a new path has
  nothing to race.

#### `file_overwrite`
- **in** `path` ★, `contents` ★, `base_sha256`?, `dry_run`?
- **out** `{ "path": string, "bytesWritten": int, "sha256": string, "dryRun"?: bool }`
- Full-file replace for files changing substantially. Must already exist
  (`O_TRUNC|O_WRONLY`, no `O_CREATE`) → absent path is `NOT_FOUND` ("use
  file_create"), so a typo can't silently create a stray file.

#### `file_replace`
- **in** `path` ★, `old_str` ★, `new_str` ★, `expected_replacements`?=1,
  `base_sha256`?, `dry_run`?
- **out** `{ "path": string, "replacements": int, "sha256": string, "dryRun"?: bool }`
- Matches `old_str` against raw file bytes and replaces with `new_str` (an empty
  `new_str` is the delete-text path). Rejects unless the occurrence count equals
  `expected_replacements` exactly → `MATCH_COUNT_MISMATCH` echoing the actual count
  (so the model knows whether to lengthen the anchor or bump the parameter).
  Default `1` is the uniqueness guarantee; the parameter exists for the deliberate
  "change all N" case. Empty `old_str` → `INVALID_ARGS`. The whole file is read
  bounded by `read.maxBytes` (larger → `FILE_TOO_LARGE`, never a partial replace).

**Cross-cutting guards.** `base_sha256` (optional hex sha256 of the file's current
full bytes — the *same* hash `file_read` returns as `sha256`) is the
optimistic-concurrency guard against the read-then-write race (the tree syncs from
GitHub out of band); on mismatch → `BASE_SHA_MISMATCH` returning the actual hash.
`dry_run` on overwrite/replace returns the count + resulting hash without writing,
so the model can confirm an `old_str` resolved uniquely before committing. Each
change audits a content hash (§2.3).

**Hard exclusions** — never in `tools/list`, always rejected in `tools/call`: any
file delete/move/rename, any shell/command execution, any Git mutation. The three
write ops above are the *only* mutation path, and only on a workspace that opts in.

### 5.4 Error spec

Domain failures return `isError: true` with `{ code, message, reason? }`
([mcp/server.go:194]). Codes:

| Code | When | `reason` values |
|---|---|---|
| `NOT_A_GIT_REPO` | git tool on a non-git workspace | — |
| `GREP_DISABLED` | `tree_search` with `where` predicates where `grep.enabled` is false | — |
| `POLICY_DENIED` | path blocked or outside the sandbox | `absolute_path`, `traversal`, `outside_root`, `blocked_glob`, dotfile reason |
| `NOT_FOUND` | missing path / path is a directory (file_read, file_overwrite, file_replace) | — |
| `INVALID_PATTERN` | bad regex (`fixedString:false`); no walk performed | — |
| `INVALID_ARGS` | arguments fail to unmarshal; empty `old_str` (file_replace); `expected_replacements` < 1 | — |
| `READ_ONLY` | a write tool called on a workspace without `write.enabled` (also absent from `tools/list`) | — |
| `PATH_EXISTS` | `file_create` onto an existing path ("use file_overwrite") | — |
| `MATCH_COUNT_MISMATCH` | `file_replace` occurrence count ≠ `expected_replacements`; message echoes the actual count; no write | — |
| `BASE_SHA_MISMATCH` | supplied `base_sha256` ≠ the file's current hash (changed since read); message returns the actual hash; no write | — |
| `FILE_TOO_LARGE` | write target exceeds `read.maxBytes` (can't read-modify-write safely); no partial write | — |
| `INTERNAL` | unexpected server error (no detail leaked) | — |

Three failures are *not* in this envelope: an **unknown tool name** is a JSON-RPC
`InvalidParams` protocol error; a **missing/invalid bearer** is an HTTP `401` (with
a `WWW-Authenticate: Bearer resource_metadata="…"` hint when OAuth is configured,
RFC 9728); and an **unknown workspace** is now an HTTP `404` — selection is by
route (§5.0), so there is no longer a per-call `UNKNOWN_WORKSPACE` domain error.
`NOT_FOUND` is deliberately indistinguishable from a policy denial where
distinguishing would leak the existence of a blocked path.

### 5.5 Orientation — how the model learns what to do

The server is only useful if the model knows *what it is for* and *how to
navigate*. Tool descriptions are prompts: every word shapes selection, so we
spend orientation cheaply at three layers, designed to be redundant because
general-purpose hosts may ignore any one of them.

- **Server `instructions`** ([mcp/server.go], `workspaceInstructions`) — a short
  string returned at `initialize`, before any reasoning. Because the URL already
  picked the tree (§5.0), it is **workspace-specific**: it folds in *this*
  workspace's `description` and detected `wellKnownFiles`, then the server's
  identity (a read-only window onto a local tree for research/workflow, *not* a
  coding agent; the model does the analysis), the orient-first flow
  (read README → locate → read), and the hard constraints
  (read-only; default-deny, so `NOT_FOUND` / `POLICY_DENIED` are *expected
  answers*, not transient errors to retry). **Best-effort:** the MCP spec lets
  servers send it, but some hosts ignore it (claude.ai's handling is unverified —
  a behavioral eval is still pending). So it is never load-bearing; the two layers
  below stand alone.
- **Tool descriptions** ([mcp/tools.go]) — each says *what* it does, *when* to
  reach for it, and its *boundary vs siblings* (e.g. `tree_search` = locate by
  path glob and/or content; `file_read` follows the locator).
  `workspace_info` returns `orientation` — the same text as `instructions` **as of
  the call** (it re-scans the tree root each time, so a README written mid-session
  appears immediately without a restart) — the tool-surface mirror that survives a
  host dropping `instructions`. Because the orientation is already embedded in
  `instructions`, it is *not* advertised as a mandated first call: a host that
  surfaced `instructions` already has it; call it when you need fresh state.
- **Tool annotations** ([mcp/tools.go]) — every tool carries MCP
  `{ readOnlyHint: true, openWorldHint: false }` plus a human `title`. A
  machine-readable restatement of "this only reads, and only from the local
  sandbox," which clients can surface in UI and trust decisions.

**Orientation files are auto-detected by convention, not configured.** A fourth,
quieter layer: `workspace_info` (and the workspace-specific `instructions`)
surface the tree's orientation files (`wellKnownFiles`) and a `description`. These
are found by a **closed,
server-owned recognizer** — a curated set of root-file stems (`readme`, `index`,
`_index`, `contents`, `toc`, `overview`, `about`, `agents`, `claude`) matched
case-insensitively and extension-agnostically, presence-only and policy-gated,
priority-ordered and capped. The choice is deliberately convention-over-config: a
per-workspace list would be a usability tax that rots on every rename, whereas a
freshly-pointed workspace "just works" if it follows any common doc/notes
convention. To teach the server a new name, add a stem in code (with a test) — not
a config knob ([mcp/registry.go]). The same priority order
picks which file the `description` falls back to.

**Truncation steers, it doesn't just flag.** When a result is capped
(`file_read` by bytes, `tree_search` by its match cap) the response adds a
`notice` string telling the model how to get the rest — narrow the `path` glob,
add a more specific `where` predicate, raise `maxBytes` — instead of letting it
treat a partial result as complete.

---

## 6. Transport & deployment

- **Streamable HTTP first.** `POST /mcp` (JSON-RPC) + `GET /mcp` (SSE keepalive via
  `SSERenderer`), both behind the bearer `Processor`; `/healthz` is unauthenticated
  ([cmd/workspace-mcp/main.go:95]).
- **stdio mode** (`-stdio`) is **mostly for testing and local development** (MCP
  Inspector, Claude Desktop/Code as a subprocess) — it is *not* the primary use
  case. The point of this server is remote: claude.ai / ChatGPT reaching local
  resources over HTTPS, which is the HTTP+tunnel path above. stdio uses the same
  dispatch and tool gating, no HTTP listener, no bearer (trusted local pipe). It
  currently reuses the HTTP path via a synthetic `httptest` round-trip per message
  — a known shim to be replaced by a transport-agnostic `jsonrpc` dispatch entry
  point later ([cmd/workspace-mcp/main.go:147]). Note claude.ai itself only connects to
  *remote* MCP servers, so stdio is never the claude.ai path.
- **Exposure.** With no tunnel enabled (default) the server binds `127.0.0.1:PORT`
  and an external reverse proxy or `ngrok`/`zrok` process fronts it. Two built-in
  tunnels can replace that listener — **enable at most one**, since each takes over
  the listener; validation rejects both:
  - **ngrok** (`server.ngrok.enabled`): the server dials ngrok directly via the Go
    SDK — no external process or `ngrok.yml`. Reserve a `domain` for a stable URL.
  - **zrok** (`server.zrok.enabled`, [cmd/workspace-mcp/zrok.go]): an OpenZiti-based
    alternative driven entirely from config+secrets. The crux is that the zrok SDK
    normally loads an *enabled* environment from `~/.zrok` (or `ZROK_*` env); we
    deliberately bypass that with an in-memory `env_core.Root` built from the
    resolved `enableToken`+`apiEndpoint`, so the server stays self-contained and
    reproducible — same posture as ngrok taking its authtoken from a SecretRef. At
    startup it reserves the configured `uniqueName` in the frontend namespace (the
    reserved *name*, not the environment, is what keeps the URL stable across
    restarts), enables an *ephemeral* environment + public proxy share, and serves
    over an in-memory ziti listener (the identity never touches disk). On clean
    shutdown the share and environment are released; an unclean exit is self-healed
    on the next start by reaping the prior `env-<uniqueName>` environment, and the
    controller's occasional transient 500 on share creation is retried. The public
    per-workspace URLs (`<frontend>/mcp/<name>`) are logged at startup, as with ngrok.

  Both built-in tunnels terminate TLS at the operator's edge, so the operator is
  in-path on the cleartext stream; the persistent zrok name reservation also
  guards against credential theft via name takeover. See §2.4 — a self-hosted
  reverse proxy whose cert we control is the only deployment that closes that gap.

  Either way claude.ai reaches the server as a custom connector / remote MCP,
  authenticating via the OAuth 2.0 authorization code flow (`GET /oauth/authorize`
  → approve page → `POST /oauth/token` → AEAD access + refresh token pair; the
  refresh token buys a new pair without re-approval). The tunnel only replaces the
  listener — the bearer/OAuth, per-workspace policy, and `os.Root` boundary are
  unchanged.

---

## 7. Deferred & future (pointers, not commitments)

The payoff of going standalone — a flag-gated write surface — has **landed**, but
reshaped from the originally-planned single unified-diff `tree_patch` into three
explicit byte-level ops (`file_create`/`file_overwrite`/`file_replace`, §5.3): no
`go-gitdiff`, no diff parser at all, just deterministic exact-byte edits with
uniqueness (`expected_replacements`) and optimistic-concurrency (`base_sha256`)
guards, through `os.Root` + the same policy a read clears, auditing a content hash,
never committing/pushing. What remains deferred here: a **batch/envelope** op (the
single-call view→edit→view loop is the honest pattern for now), transaction/commit
verbs (the deferred git-workflow design; `dry_run` + `base_sha256` are its hooks),
and the research-workflow cousin "capture results back" (append a finding to an
inbox note). Move/rename/delete stay out — delete is the highest blast radius; add
only on real need.

Other candidates, each measured against the §1 thin-pipe rule: read-only `git_log`
(whole-workspace + per-file), `git_blame`, `git_show`, `git_diff`; an
author-controlled repo manifest; a meaning-ranked mode on `tree_search` (an opt-in
semantic upgrade, not a new tool). A corpus-wide `tree_metadata` tag/frontmatter
rollup was considered and **rejected** — it serves database-like querying of a
*known* schema, contradicting the orient-an-*unfamiliar*-tree premise; content
search already finds metadata values as plain text. (Workspace-per-URL routing
with a per-tree `workspace_info`, `description`/`wellKnownFiles` orientation,
ranged `file_read`, raw-binary `file_read`, and the consolidated `tree_search`
have since landed.) Anything that would have the server
rank by meaning, build graphs, or summarize is **out** — that is the model's job.

**MCP `resources/list` + `resources/read` — considered, deprioritized.** The
protocol's resources surface fits a workspace's files naturally, but resources are
*application-driven*: in practice a **human** browses/attaches them, and LLMs don't
reliably request them without a strong user hint. That's a category mismatch with
this server's premise — the *model* should automatically discover and select files
while it reasons — so the dependable surface stays **tools** (`tree_search` +
`file_read`).
