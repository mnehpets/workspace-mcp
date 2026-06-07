# PLAN.md — claude.ai → Local Git Tree (standalone Go MCP server)

## 1. Objective

Give the **claude.ai web app** a safe, read-only way to access the files of one or
more **local directory trees** (each a named *workspace*), by exposing a small
**standalone Go MCP server** ("the server") over a public HTTPS tunnel. A
workspace whose tree is a Git repository gains extra, git-aware operations
(status; later diff) — but git-ness is a *detected, additive* property, not the
core abstraction. The unit is a directory tree; the tools operate on trees.

**It depends on no other program running locally** — no second process, no
external binary. The server is the whole thing: it sandboxes each workspace's
directory, exposes a handful of named read-only MCP tools over them, and refuses
everything else. Writing is deferred to a later task (the last section of §12).

Target workflow:

```text
claude.ai (web app)
  → custom connector / remote MCP over HTTPS
  → ngrok public URL
  → Go MCP server   (this project; bearer-authenticated, default-deny, read-only)
  → per-workspace os.Root sandbox (selected by the `workspace` param)
  → local directory tree(s) — git repos get extra operations
  → human reviews / commits / pushes out-of-band
```

The server is a **policy front-end over sandboxed directory trees**, not a proxy.
It exposes a small `workspace_* / tree_* / file_* / git_*` tool surface and nothing
else reaches the filesystem.

---

## 2. Scope decisions (locked)

- **Read-only MVP.** Tree ops (list / read / find / grep) on any workspace; git
  ops (status) where the tree is a git repo. No writes.
- **Multiple workspaces, one server.** Config declares one or more named
  workspaces; every tool takes a `workspace` param defaulting to `"default"`.
  Each workspace is an independent `os.Root` sandbox.
- **Permissions are per-workspace.** Path policy (allow/block globs), gitignore
  handling, and read/grep limits are configured per workspace, not globally.
- **Hard sandbox via `os.Root`.** Containment is enforced by the OS-level root
  (one per workspace), not by string munging. Path policy (globs) sits *on top*
  as allow/deny, not as the security boundary.
- **Writes are a later task** using deterministic diff application. Off by
  default; designed, not built, in MVP.

---

## 3. Non-goals

Do **not** build: a remote shell or command runner; arbitrary filesystem write;
Git automation (commit/push/branch/rebase/reset); a second LLM / agent loop;
LSP/symbol indexing; Project-Knowledge sync; a multi-user SaaS. Single-user local
developer tool.

---

## 4. External assumptions

1. The server runs locally, bound to `127.0.0.1`, with one or more workspaces,
   each a directory tree (`workspaces[].root`). A tree need not be a Git repo;
   git-only operations are simply unavailable on non-git trees.
2. claude.ai custom connectors reach remote MCP servers from Anthropic cloud
   infrastructure, so a tunnel is required (localhost is unreachable to them).
3. claude.ai's remote-MCP transport is **Streamable HTTP** (one endpoint: JSON-RPC
   over POST, plus an SSE GET stream).
4. ngrok (or equivalent) exposes **only this server**.
5. The human reviews/commits changes themselves; MVP never writes.
6. **No external binaries at all.** Search is native Go (§6); git-awareness
   (status, tracked-file listing) is **go-git** (pure Go). The server is a single
   self-contained binary.

---

## 5. The sandbox primitive: `os.Root`

This is the security core, so be precise about it:

- **`os.Root`** (`os.OpenRoot(dir)`, Go 1.24+) is a real sandbox. Its methods
  (`Open`, `Stat`, `Create`, `OpenFile`, `ReadDir`, …) are guaranteed to stay
  within the root **even in the presence of symlinks**, and resist TOCTOU
  races — a symlink inside the tree that points at `/etc/passwd` cannot be
  followed out. **All filesystem access goes through a single `*os.Root` opened
  once at startup.** We are on Go 1.26, so this is available.

### 5.1 Where `os.Root` is load-bearing — and where it isn't

`os.Root` is essential for **model-supplied paths**: `file_read` and the deferred
`tree_patch` take a path straight from the model, which may be `../../etc/passwd`
or a symlink. Those *must* resolve through the selected workspace's `os.Root`,
full stop.

Two classes of helper deliberately read **outside** `os.Root`, and that's fine
because **they never serve file content to the model** — they produce metadata
only (lists, status codes, ignore decisions):

- **go-git** (status, tracked-file enumeration) reads via its own
  `go-billy`/`osfs`.
- **grrep's `IgnoreSet`** reads `.gitignore`/`.ignore` files via `os.Open` while
  walking.

The trust split: these decide *what exists / what's ignored / what changed*;
`os.Root` decides *what content crosses the boundary*. A symlink can't trick them
into leaking, because they aren't the leak path.

**The grep walker is the one nuance.** It *does* read content, but over a tree we
control: `fastwalk` skips non-regular files (so symlinks are never followed) and
skips `.git`, closing the escape vector by omission. We still open each matched
leaf through `os.Root` (`root.Open(rel)`) so even grep content reads ride the
same boundary. So: anything reading content that the model can *aim* — through
`os.Root`; metadata helpers may walk freely.

---

## 6. Technology stack

Go (`go 1.26`), built on **`github.com/mnehpets/http`**.

From that library (confirmed by reading source):

- **`jsonrpc`** — `jsonrpc.NewEndpoint()` + `Register(ns, receiver)`: a
  reflection-based JSON-RPC 2.0 registry. MCP is JSON-RPC 2.0, so this is the MCP
  core. Handlers are `func(ctx, params Struct) (Result, error)`. Slash-named MCP
  methods (`tools/list`, `tools/call`, `notifications/initialized`) are expressed
  via the `_ jsonrpc:"tools/list"` struct-tag name override.
- **`endpoint`** — typed handlers, `Renderer`s, and the **`Processor`** middleware
  interface (where bearer auth lives).
- **`endpoint.SSERenderer`** — iterator-based SSE for the Streamable-HTTP stream.

To add (the library lacks it): a constant-time **bearer-token `Processor`**
(`auth/` package), 401 on missing/invalid, no disclosure of which. Upstream later
only if clean.

Other deps:
- **`github.com/go-git/go-git/v5`** — pure-Go git. Used for git-awareness only
  (§5.1), and only on workspaces detected as git repos: `repo.Worktree().Status()`
  for `git_status` and the index/worktree for tracked-file enumeration. No `git`
  binary needed. Metadata only — never the content path. (`.gitignore` matching
  lives in grrep's `IgnoreSet`, below, to keep one ignore engine.)
- **`github.com/bluekeyes/go-gitdiff`** — parse & apply unified/git diffs for the
  deferred `tree_patch` task (`gitdiff.Parse` → `gitdiff.Apply`). Deterministic
  context-checked application; pulled in only with that task.
- stdlib `os` (`os.Root`), `io/fs`, `bytes`, `regexp`, `log/slog` (redacting audit
  log); `gopkg.in/yaml.v3` for the YAML config and `github.com/joho/godotenv` for
  the `.env` secrets file (§7.1).

**Search: vendor & adapt `github.com/bep/grrep` (Apache-2.0), don't reinvent.**
grrep is ~734 LOC, cleanly factored, and its matching core is exactly our model.
Copy with attribution (retain SPDX headers; add a NOTICE):
- `internal/match.go` → **verbatim.** `CompileMatcher`/`Matcher` already give us
  fixed-string default + regex opt-in + a literal pre-filter extracted from the
  regex AST (slide `bytes.Index`, run the regex engine only on candidate lines).
  Better than hand-rolling; no changes.
- The scan core from `main.go` (`scanWholeBody`/`scanWholeRegex`/`scanFileStream`,
  NUL-byte binary detection, CRLF handling, `sync.Pool` buffers) → copy and
  refactor the one CLI-ism: emit structured `{path,line,text}` matches instead of
  writing `path:line:text` to stdout.
- The walker (`fastwalk` + per-dir ignore) → adopt, with **one sandbox change**:
  open each candidate leaf via `os.Root` (`root.Open(rel)`) instead of
  `os.Open(path)`. grrep already skips non-regular files (symlinks are never
  followed) and skips `.git`, so the escape vector is closed by omission; routing
  the leaf open through `os.Root` adds TOCTOU/symlink rigor at ~zero cost while
  keeping fastwalk's fast parallel traversal.

New deps (all small pure-Go libs — still **no external binary**):
`github.com/charlievieth/fastwalk`, `github.com/bep/gogitignore`,
`golang.org/x/sync/errgroup`.

Ignore handling — pick **one** engine: standardize on grrep's `IgnoreSet`
(`bep/gogitignore`: nested `.gitignore` + `.ignore`, per-dir matchers) for
`tree_list`/`tree_find`/`tree_grep`, and use go-git **only** for `git_status` +
tracked enumeration. One ignore notion across all read tools, not two.

No heavy MCP SDK — the surface (initialize + tools/list + tools/call) is small and
the jsonrpc registry already handles the protocol.

---

## 7. Configuration

Config is a YAML file (`config.yaml`), loaded and validated at startup. YAML
because the config is structured — a list of workspaces, each with nested policy
globs, search options, and limits — which flat `KEY=value` would flatten badly.
Ship a `config.example.yaml`; the path is given via a `-config` flag (default
`./config.yaml`).

```yaml
# config.example.yaml
server:
  host: 127.0.0.1                 # localhost only; ngrok fronts it
  port: 3850
  publicURL: https://<reserved-subdomain>.ngrok.app

auth:
  bearerToken:
    env: SHIM_BEARER_TOKEN         # resolved from .env / OS env, never stored here

# One or more named directory trees. The tool `workspace` param selects one and
# defaults to "default". Permissions (policy/read/grep/gitignore) are per-workspace.
workspaces:
  - name: default
    root: /absolute/path/to/a/tree     # this workspace's os.Root sandbox
    respectGitignore: true             # via grrep IgnoreSet (works on any tree)
    policy:                            # allow/deny on top of os.Root (BLOCKED wins)
      allowGlobs:
        - "**/*.md"
        - "**/*.go"
        - "**/*.txt"
        - "docs/**"
        - "README*"
      blockGlobs:
        - ".git/**"
        - "**/.env"
        - "**/.env.*"
        - "**/*secret*"
        - "**/*credential*"
        - "**/*.pem"
        - "**/*.key"
        - "**/id_rsa*"
        - "**/.ssh/**"
        - "**/node_modules/**"
    read:
      maxBytes: 1000000
    grep:
      enabled: true
      workers: 0                       # 0 = GOMAXPROCS
      maxMatches: 500

  - name: notes                        # a second, tighter workspace
    root: /absolute/path/to/notes
    respectGitignore: false
    policy:
      allowGlobs: ["**/*.md"]
      blockGlobs: ["**/.git/**"]
    read:
      maxBytes: 250000
    grep:
      enabled: true
      workers: 0
      maxMatches: 200

log:
  level: info
```

### 7.1 Secrets via dotenv

`config.yaml` holds **no secret values**. A secret-valued field (currently
`auth.bearerToken`) takes a reference, not a literal:

```yaml
bearerToken:
  env: SHIM_BEARER_TOKEN     # name of an env var to read the value from
```

Resolution order at startup:
1. Read a `.env` file (path via `-env`, default `./.env`) with
   `github.com/joho/godotenv` into a `map[string]string`.
2. Overlay the process environment — `os.Environ()` **overrides** dotenv values
   (so a deployment can inject `SHIM_BEARER_TOKEN` without a file).
3. Resolve each `{ env: NAME }` reference against that merged map; a missing or
   empty referenced var is a startup error.

A secret field may also be given as a plain string, but that's discouraged and
flagged in validation for `bearerToken` (keep tokens out of the YAML). `.env`:

```dotenv
# .env  (gitignored — never commit)
SHIM_BEARER_TOKEN=replace-with-long-random-token   # >= 32 random bytes
```

Validation: at least one workspace; names unique; a workspace named `default`
should exist (it's the `workspace` param's fallback — without it, every call must
name a workspace explicitly); each `root` exists & is a directory; globs compile;
every `{ env: … }` reference resolves to a non-empty value. Commit
`config.example.yaml` and `.env.example`; gitignore `config.yaml` and `.env`.
Config parsed with `gopkg.in/yaml.v3` into a typed struct; `KnownFields(true)` so
unknown keys are an error, not a silent typo.

---

## 8. Exposed tool surface (read-only)

Tool families: **`workspace_*`** (discovery), **`tree_*`** (directory/tree-wide),
**`file_*`** (single file), **`git_*`** (git-repo only — else `NOT_A_GIT_REPO`).
**Every tool except `workspace_list` takes a `workspace` string** selecting the
tree, defaulting to `"default"`. All `path` args are workspace-relative and
resolved through that workspace's `os.Root`. Per-workspace policy/limits (§7) apply.

### 8.1 `workspace_list`
List configured workspaces (so the model can discover them). No params →
`{ "workspaces": [{ "name", "isGitRepo", "description", "wellKnownFiles" }] }`.
Does not expose roots.
- **`description`** — what the tree is *for*, so the model can pick by intent on
  the first hop. Resolved at startup: an optional per-workspace `description` in
  config (authoritative) → else the first section of the tree's `README.md` (the
  text under the top heading, trimmed to a small cap) → else absent. See task §12.12.
- **`wellKnownFiles`** — which agent/orientation files exist at the tree root, of
  a fixed set (`README.md`, `AGENTS.md`, `CLAUDE.md`), e.g.
  `["README.md", "CLAUDE.md"]`. Presence only — metadata, not content. See §12.12.

### 8.2 `tree_list`
List entries under a directory. `{ "workspace": "default", "path": "docs",
"recursive": false }` → `{ "entries": [{ "path", "type", "size" }] }`. Backed by
`Root`-scoped `ReadDir`/`WalkDir`, filtered by the workspace's `IgnoreSet`
(grrep/`gogitignore`) when its `respectGitignore` is set (and by its policy globs).

### 8.3 `file_read`
Read one allowed file, optionally a line range. `{ "workspace": "default",
"path": "docs/x.md", "maxBytes": 100000, "startLine": 1, "endLine": 100 }` →
`{ "path", "content", "truncated", "binary", "startLine", "endLine", "totalLines" }`.
Opens via `Root.Open`; enforces the workspace's `read.maxBytes`; detects binary and
flags or refuses it.
- **`startLine` / `endLine`** (optional, 1-based, inclusive) — return only that
  span instead of the whole file; either may be omitted (open-ended toward the
  start/end). The response echoes the resolved `startLine`/`endLine` and reports
  `totalLines` so the model can page (e.g. request the next 100). `maxBytes` still
  caps the *returned* span. Line ranges apply to text only; a binary file is
  flagged/refused as today regardless of range. See task §12.13.

### 8.4 `tree_find`
Fuzzy filename search. `{ "workspace": "default", "query": "scale-out" }` →
`{ "files": [...] }`.

### 8.5 `tree_grep` (if the workspace's `grep.enabled`)
Concurrent content search, powered by the vendored grrep core (§6).
`{ "workspace": "default", "pattern": "ASC workflow", "path": "docs",
"fixedString": true }` → `{ "matches": [{ "path","line","text" }], "truncated" }`.
- **`fixedString: true` (default)** — literal substring (`Matcher` fast path).
- **`fixedString: false`** — Go `regexp` with grrep's literal pre-filter.
- Optional `caseInsensitive` / `wordBoundary` map to grrep's `MatchOpts` (`-i`/`-w`).
`fastwalk` traversal filtered by `IgnoreSet` + policy globs + `.git`/dotfile skip;
binary files skipped (NUL probe); each leaf opened via `os.Root`; worker pool
sized by the workspace's `grep.workers`. Cap at `grep.maxMatches`
(`truncated: true` when hit).

### 8.6 `git_status` (git-repo workspaces only)
Read-only git status for orientation, via go-git `Worktree().Status()` + current
branch (`repo.Head()`) → `{ "branch", "files": [{ "path","status" }] }`. On a
non-git workspace: `NOT_A_GIT_REPO`. No mutation, no `git` binary.

### 8.7 Hard exclusions
Never exposed, always rejected: any write/create/delete/move/rename (until the
deferred `tree_patch` task), any shell/command execution, any Git mutation.
Absent from `tools/list` and rejected in `tools/call`.

---

## 9. MCP protocol behavior

Server to claude.ai; sandboxed-FS client internally. Via the jsonrpc registry:

- `initialize` → protocol version, `{ capabilities: { tools: {} } }`, server info;
  reject unsupported versions.
- `notifications/initialized` → accept (notification, no response).
- `tools/list` → only the `tree_*` / `git_*` tools with JSON Schemas (each
  includes the `workspace` param). The list is workspace-independent; a tool may
  still fail per-call for a specific workspace (disabled grep, non-git tree).
- `tools/call` → the gate. Per call: (1) bearer already validated at HTTP layer;
  (2) tool name allowlisted; (3) args validate against schema; (4) resolve the
  `workspace` (default `"default"`) → its `os.Root` + policy, else
  `UNKNOWN_WORKSPACE`; (5) per-workspace enablement (`grep.enabled`,
  git-repo-ness) checked; (6) every `path` arg passes that workspace's policy
  (§10) and resolves through its `os.Root`; (7) perform the one mapped read;
  (8) size-limit/sanitize; (9) audit-log (including the workspace).
- `ping` → respond if sent.

### 9.1 Transport
Streamable HTTP first: `POST /mcp` (JSON-RPC via `jsonrpc.Endpoint`) and
`GET /mcp` (SSE via `SSERenderer`). Add legacy SSE transport only if claude.ai
requires it. A local **stdio** transport is also available (`-stdio`) for
subprocess clients (MCP Inspector, Claude Code/Desktop) — same dispatch and tool
gating, no HTTP listener, no bearer (trusted local pipe). See §16 for the planned
native-stdio cleanup.

---

## 10. Security model

### 10.1 Default-deny
Allowed only if: tool allowlisted; args valid; the `workspace` resolves and the
tool is enabled for it; every path resolves inside **that workspace's** `os.Root`
**and** matches its `policy.allowGlobs` **and** matches none of its
`policy.blockGlobs`. Else denied. Permissions are per-workspace — one workspace's
policy never widens another's.

### 10.2 Containment vs. policy (two distinct layers)
- **Containment (hard):** one `os.Root` per workspace. Symlink-safe, TOCTOU-safe.
  The wall. A path resolved under workspace A's root can never reach workspace B's.
- **Policy (soft):** glob allow/deny + `.gitignore` + dotfile rules. Refuses
  things that are *inside* the sandbox but shouldn't be served (`.git/**`, `.env`,
  keys, `node_modules`). `policy.blockGlobs` always wins.

Reject model-supplied absolute paths and any `..` before resolution; then let
`os.Root` enforce the rest. Don't hand-roll traversal checks as the boundary —
that's what `os.Root` is for.

### 10.3 Read-only by construction
No code path writes (until the deferred patch task lands, behind a flag). The
`*os.Root` is used only
for read methods in MVP. Read-only is a property of the build, not just config.

### 10.4 Auth, layered
- **Server bearer token**, constant-time compared, ≥ 32 random bytes; 401 on
  missing/invalid, no detail. Sourced from `.env`/OS env via the `{ env: … }`
  reference (§7.1) — never from `config.yaml`. (AuthN is server-wide; AuthZ is
  per-workspace policy.)
- **ngrok edge** control (OAuth / basic / IP-allow / reserved domain) — not
  obscurity alone.
- Never log the token; keep `.env` out of version control.

### 10.5 Audit log
Per request: timestamp, MCP method, tool, redacted args, resolved path(s),
allow/deny + reason, byte/match counts. No file contents by default. Token never
logged.

---

## 11. Repository layout

> The directory is still named `vscode-mcp-shim` from earlier iterations. Rename
> to e.g. `workspace-mcp` when convenient (out of MVP scope).

```text
workspace-mcp/
  PLAN.md
  README.md
  NOTICE                   # attribution for vendored grrep (Apache-2.0)
  go.mod
  config.example.yaml      # committed; copy to config.yaml (gitignored)
  .env.example             # committed; copy to .env (gitignored) for secrets
  .gitignore               # ignores config.yaml and .env
  cmd/shim/main.go         # wire config, workspaces, MCP endpoint, listen
  config/
    config.go              # YAML load + validation (typed struct)
    secrets.go             # godotenv + os.Environ merge; resolve {env:NAME} refs
  workspace/registry.go    # name → {*os.Root, policy, IgnoreSet, isGitRepo}
  mcp/
    server.go              # initialize / tools/list / tools/call
    tools.go               # workspace_list/tree_*/file_*/git_* defs + JSON schemas
  fsroot/root.go           # os.Root wrapper: safe open/read/list/walk (per workspace)
  policy/policy.go         # glob allow/deny, dotfile rules (per workspace)
  grrep/                   # vendored from bep/grrep (Apache-2.0, SPDX retained)
    match.go               #   verbatim: Matcher + CompileMatcher
    scan.go                #   adapted: scan core → structured {path,line,text}
    ignore.go              #   IgnoreSet (gogitignore): nested .gitignore/.ignore
  search/
    grep.go                # fastwalk + os.Root leaf-open, drives grrep core
    find.go                # fuzzy filename search
  gitaware/
    detect.go              # is this tree a git repo? (go-git PlainOpen)
    status.go              # go-git Worktree().Status() + branch
    tracked.go             # tracked-file enumeration (index/worktree)
  auth/bearer.go           # bearer-token endpoint.Processor
  audit/log.go             # redacting structured logger
  patch/patch.go           # (deferred task) go-gitdiff apply, sandboxed writes
  test/
    fsroot_escape_test.go  # symlink/.. escape attempts must fail
    policy_test.go
    secrets_test.go        # env override + {env:NAME} resolution
    mcp_list_test.go       # only workspace_list/tree_*/file_*/git_* exposed
    workspace_test.go      # unknown workspace, cross-workspace isolation
    grep_test.go
```

Packages live at the repo root, not under `internal/` — this is an application,
not a library meant for external import, so `internal/` would add nesting without
buying anything.

---

## 12. Task list

Ordered, self-contained sections. Work top to bottom; each section is independently
buildable, has its own tests, and ends with a **Done when** gate. Don't start a
section until the previous one's gate is green. Sections 3–4 are the security
spine — nothing that serves content to the model ships before their escape/policy
tests pass.

### 1. Scaffold, config, secrets, health
- [x] Module + directory skeleton per §11; `go build ./...` clean.
- [x] `config/`: load the `-config` YAML (default `./config.yaml`) into a typed
      struct via `gopkg.in/yaml.v3` with `KnownFields(true)`. Supports a
      `workspaces` list.
- [x] `config/secrets.go`: `godotenv` reads `-env` (default `./.env`) into
      a `map[string]string`; overlay `os.Environ()` (OS **overrides** dotenv);
      resolve each `{ env: NAME }` config reference (e.g. `auth.bearerToken`) — a
      missing/empty referenced var is a startup error.
- [x] Validate: ≥ 1 workspace, unique names, a `default` exists; each `root` is an
      existing dir; port in range; resolved `bearerToken` ≥ 32 bytes; globs compile.
- [x] `audit/`: `slog` logger that redacts the bearer token and never logs file
      contents.
- [x] `cmd/shim`: wire config + secrets + logger; serve `GET /healthz` → `{"ok":true}`.
- [x] `config.example.yaml` + `.env.example` committed; `config.yaml` + `.env`
      gitignored.
- **Done when:** `go run ./cmd/shim -config config.yaml -env .env` serves
  `/healthz`; malformed/unknown-key config or an unresolved secret fails fast with
  a clear error.

### 2. Bearer auth
- [x] `auth/`: constant-time bearer `endpoint.Processor`; 401 on missing/invalid
      with no hint as to which.
- [x] Wire it ahead of every route except `/healthz`.
- [x] Tests: missing / wrong / valid token; assert the token never appears in logs.
- **Done when:** unauthenticated requests get 401, valid passes, redaction test green.

### 3. Sandbox core — `fsroot` over `os.Root` + workspace registry (the spine)
- [x] `fsroot/`: open a tree's `root` with `os.OpenRoot`; expose safe
      `Open`/`Stat`/`ReadDir`/walk taking workspace-relative paths.
- [x] `workspace/`: build a registry at startup — one entry per configured
      workspace holding `{*os.Root, policy, IgnoreSet, isGitRepo}` (git-ness via
      `gitaware.Detect`). Lookup by name; `UNKNOWN_WORKSPACE` otherwise.
- [x] Reject model paths that are absolute or contain `..` before resolution;
      everything else resolves through that workspace's `*os.Root`.
- [x] **`fsroot_escape_test.go` (the centerpiece):** absolute path rejected; `..`
      rejected; in-tree symlink → `/etc/passwd` cannot read it; in-tree symlink to
      another in-tree file behaves; concurrent-rename/TOCTOU cannot escape.
- [x] `workspace_test.go`: unknown workspace rejected; a path valid in one
      workspace cannot reach another's tree.
- **Done when:** every escape test fails to read anything outside the root, and
  cross-workspace access is impossible. The boundary is ours and provable.

### 4. Path policy
- [x] `policy/`: `policy.allowGlobs` allow + `policy.blockGlobs` deny (deny always
      wins); dotfile rule; shares the absolute/`..` rejection with fsroot.
- [x] Tests: allowed globs pass; `.env`, `.git/**`, keys, `node_modules` blocked;
      `docs/../.env` blocked; dotfiles handled per rule.
- **Done when:** policy tests green; a path must clear **both** fsroot and policy
  to be served.

### 5. Search — vendor & adapt grrep
- [x] `grrep/` package: `match.go` **verbatim** (SPDX retained); scan core adapted
      to emit structured `{path,line,text}`; `IgnoreSet` (gogitignore); add `NOTICE`.
- [x] `search/grep.go`: `fastwalk` traversal with `.git`/dotfile skip, the
      workspace's `IgnoreSet` + policy filter, NUL-byte binary skip, **each leaf
      opened via `os.Root`**, worker pool sized by the workspace's `grep.workers`,
      cap `grep.maxMatches`.
- [x] `search/find.go`: fuzzy filename search over the filtered tree.
- [x] Tests: literal + regex hit; `fixedString`/`caseInsensitive`/`wordBoundary`;
      ignore + policy respected; binary skipped; cap → `truncated`.
- **Done when:** grep/find return correct in-sandbox results, no external binary.

### 6. Git-awareness — go-git
- [x] `gitaware/`: `Detect(root)` (is it a git repo?); `git_status` via
      `Worktree().Status()` + branch from `repo.Head()`; tracked-file enumeration
      via index/worktree.
- [x] Tests against a temp repo with staged / unstaged / untracked files, and a
      non-git tree (Detect false → `git_status` yields `NOT_A_GIT_REPO`).
- **Done when:** status returns branch + per-file codes on git workspaces and
  `NOT_A_GIT_REPO` on others; no `git` binary used.

### 7. MCP server — handshake + tool-list invariant
- [x] `mcp/`: register `initialize`, `notifications/initialized`, `tools/list`,
      `tools/call` on the jsonrpc endpoint (slash names via `_ jsonrpc:"…"`).
- [x] `initialize` returns protocol version + `{capabilities:{tools:{}}}`; reject
      unsupported versions.
- [x] `tools/list` returns only `workspace_list` + `tree_*` / `file_*` / `git_*`
      with JSON Schemas (each tool but `workspace_list` includes a `workspace` param).
- [x] `mcp_list_test.go`: no other surface leaks; `tools/call` rejects unknown
      tool names.
- **Done when:** an MCP client completes the handshake and sees only intended tools.

### 8. Read tools wired through `tools/call`
- [x] Implement `workspace_list` / `tree_list` / `file_read` / `tree_find` /
      `tree_grep` / `git_status`, each (except `workspace_list`): bearer (done) →
      name allowlist → schema validate → resolve `workspace` → per-workspace
      enablement → path policy → fsroot/search/gitaware → size/count limits →
      audit log.
- [x] Map failures to the error spec (§13).
- [x] Integration test: two workspaces (one git, one not) + sample files + a
      symlink escape + a `.env`; `initialize` → `tools/list` → `workspace_list` →
      `tree_list` → `file_read` → `tree_grep` → `git_status`; allowed succeed,
      escapes/blocked/unknown-workspace/non-git fail; audit records present.
- **Done when:** all read tools work end-to-end and the integration test is green.

### 9. Transport + ngrok
- [x] `POST /mcp` (jsonrpc) + `GET /mcp` (SSE via `SSERenderer`), Streamable-HTTP.
- [x] Docs: `ngrok http 127.0.0.1:<server.port>` exposes only this server; reserved
      domain + edge auth; example `ngrok.yml`. Never expose anything else.
- **Done when:** reachable through the tunnel; unauthorized requests fail at both
  edge and server.

### 10. claude.ai connector + docs
- [x] README: add as a custom connector (URL + bearer), the safety model, the
      read-only guarantee, example prompts, shutdown.
- [x] Manual verify: a live claude.ai session lists / reads / greps the repo and
      **cannot** reach any write/shell tool.
- **Done when:** a real claude.ai session drives the read tools end-to-end. ← MVP.

### 11. Deferred — `tree_patch` (writes, done right) · flag, default off
The payoff of going standalone. Do **not** start until sections 1–10 are green.
- [ ] Input: a unified/git diff (or structured old/new-string hunks).
- [ ] Parse with `go-gitdiff`; apply against current content and **require context
      to match** — reject on drift (model re-reads and retries).
- [ ] Write **through `os.Root`** (`Root.Create`/`OpenFile`); restrict to
      `policy.allowGlobs`, honor `policy.blockGlobs`; validate each file before any
      write; optional all-or-nothing across a multi-file patch.
- [ ] Audit a content hash per change. Never commit or push — the human reviews
      `git diff`.
- [ ] Failure: context mismatch → `PATCH_CONFLICT` ("re-read and retry"); no
      partial write.
- **Done when:** patches apply deterministically and reversibly via Git; conflicts
  and out-of-policy/escape targets are refused. Deterministic, no second LLM.

### 12. Richer `workspace_list` — purpose + well-known files
Independent of the patch task; can land any time after MVP (§8.1). Promotes the
"Enrich workspace descriptions" and (partly) "Repo manifest" bullets from §16.
- [ ] Config: add an optional `description` string per workspace (parsed by
      `config/`; `KnownFields(true)` already in place). Discouraged-but-allowed to
      omit. No secret handling.
- [ ] Registry (`workspace/`): at startup resolve a `description` per workspace —
      config value if set, else the first section of the tree's `README.md` (text
      under the first heading, whitespace-collapsed, trimmed to a small char cap),
      else empty. Read the README **through the workspace's `os.Root` + policy**
      (it's content); a blocked/missing README simply yields no description.
- [ ] Registry: detect which of a fixed set — `README.md`, `AGENTS.md`,
      `CLAUDE.md` — exist at the tree root (presence only, via `Root.Stat`).
      Compute once at startup; store on the registry entry. (Task §12.17 later
      broadens this to a server-maintained recognizer — auto-detecting common doc/
      notes conventions like `index.md` by convention, still no config.)
- [ ] `mcp/`: extend `workspace_list`'s result + JSON Schema to include
      `description` (string, omitempty) and `wellKnownFiles` (string array). Still
      no params; still never exposes roots.
- [ ] Optionally fold `description` into the `workspace` enum text on the other
      tools so the model picks by intent on the first hop (the §16 cousin).
- [ ] Tests: config description wins over README; README-derived fallback parses
      the first section only and respects the cap; policy-blocked README yields no
      description; `wellKnownFiles` reflects exactly the present subset.
- **Done when:** `workspace_list` returns a useful per-workspace `description` and
  an accurate `wellKnownFiles` list, sourced config-first then README, with no root
  disclosure and no content leak past policy.

### 13. Ranged `file_read` — line spans
Independent; can land any time after MVP (§8.3). A long file shouldn't force a
whole-file read.
- [ ] `mcp/` + `file_read`: add optional `startLine` / `endLine` (1-based,
      inclusive) to the args + JSON Schema; either omittable (open-ended).
- [ ] Implementation: open via `os.Root` as today, then return only the requested
      line span. Apply `read.maxBytes` to the **returned span**; preserve binary
      detection/refusal (ranges are text-only). Count and return `totalLines`; echo
      the resolved `startLine`/`endLine`; set `truncated` if `maxBytes` clipped the
      span.
- [ ] Validate the range: `startLine`/`endLine` ≥ 1 and `startLine` ≤ `endLine`
      when both given; out-of-bounds clamps to the file rather than erroring (an
      empty span past EOF returns empty content with the true `totalLines`).
- [ ] Tests: full-file read unchanged when range omitted; `1-100` returns exactly
      those lines; open-ended `startLine`-only and `endLine`-only; past-EOF clamps;
      `maxBytes` clips a span → `truncated`; binary still flagged regardless of range.
- **Done when:** `file_read` can return an exact line span with correct
      `totalLines`/`truncated`, behaves identically to today when no range is given,
      and never bypasses policy, `os.Root`, or binary handling.

### 14. Tag / frontmatter index — corpus orientation
A cheap, corpus-wide table of contents the model can't build without reading every
file. Orientation, not analysis — belongs with the §12.12 description family.
- [ ] Parse YAML frontmatter from text files (markdown `---` blocks) via a pure-Go
      parser (`gopkg.in/yaml.v3`, with `yuin/goldmark` if richer markdown parsing is
      wanted). Read each file **through the workspace's `os.Root` + policy + ignore**.
- [ ] Aggregate across the workspace: the set of tags and frontmatter field names,
      and which files carry each. Bound the walk like `tree_grep` (skip binary,
      respect `IgnoreSet` + policy, cap files scanned).
- [ ] Expose a `tree_metadata` (tree-wide) tool: `{ "workspace": "default" }` →
      `{ "tags": [{ "tag", "files": [...] }], "fields": [{ "name", "files": [...] }] }`.
      Optionally filter to files matching a tag/field so the model can narrow.
- [ ] Tests: frontmatter parsed; rollup correct; blocked/ignored/binary files
      excluded; no root disclosure.
- **Done when:** `tree_metadata` returns an accurate, policy-respecting rollup of
      tags/frontmatter across a workspace, built without the model reading every file.

### 15. Raw-binary delivery in `file_read`
Hand raw bytes to claude.ai/ChatGPT and let *them* parse PDFs/images — the server
extracts nothing (§8.3). The model can't reach the local bytes; that's the one
thing only the server can do.
- [ ] `file_read`: for non-text/binary files, instead of refusing, return the raw
      bytes as a base64 blob (or an MCP resource), with a detected MIME type:
      `{ "path", "content", "encoding": "base64", "mimeType", "truncated" }`.
      Keep the text path (and `binary: true` flag) for callers that want refusal.
- [ ] Enforce `read.maxBytes`, `os.Root`, and policy exactly as for text reads;
      binary delivery never widens the sandbox or policy.
- [ ] Add an arg to opt into binary (e.g. `allowBinary: true`) so existing text
      callers keep today's refuse-binary behavior by default.
- [ ] Tests: a PDF/image returns base64 + mimeType under the byte cap; oversize →
      `truncated`; policy/`os.Root` still gate the path; text reads unchanged.
- **Done when:** `file_read` can deliver raw binary content for the platform to
      parse, under the same byte/policy/sandbox limits, without server-side extraction.

### 16. Orientation & tool-selection eval (claude.ai)
Validate that the orientation work (server `instructions`, the reworked tool
descriptions, the read-only annotations, the `workspace_list` "start here"
framing) actually changes model behavior — and settle the open question of
whether claude.ai honors the `instructions` field at all. Functional verification
already exists (§10, §14); this is the *behavioral* eval the research prescribes
("hit-rate" testing), not a correctness check.
- [ ] Author a small fixed set (~6–10) of sample claude.ai prompts spanning the
      intended flows: discover workspaces, orient (read README), locate-by-name vs
      locate-by-content, ranged read of a large file, git status. Keep them in the
      repo (e.g. `test/eval/prompts.md`) so the set is stable and re-runnable.
- [ ] For each, record whether the live session (a) picks the *right* tool and
      (b) **orients first** (calls `workspace_list` / reads README before drilling
      in) rather than guessing paths. This is manual against a real connector;
      capture pass/fail + notes, not an automated harness.
- [ ] **Instructions probe:** put a distinctive, falsifiable directive in the
      `instructions` string (e.g. a specific first-call convention) and check
      whether behavior reflects it — the only practical way to tell if claude.ai
      surfaces `instructions`. Record the verdict; if ignored, lean harder on the
      description/`workspace_list` layer and note it in [docs/design.md §5.5].
- [ ] Feed failures back into the description/annotation/`instructions` wording and
      re-run; iterate until the hit-rate is acceptable. Treat the prose as prompts
      under test, per the writing-tools-for-agents guidance.
- **Done when:** the prompt set runs against a live claude.ai connector with a
      recorded hit-rate, the instructions-honored question is answered, and any
      wording fixes from the first pass have landed. Pairs with the §10 / §14 live
      checks (folds into §12.10's verification rather than duplicating it).

### 17. Auto-detect orientation files by convention (no config)
Task §12.12 detects a **fixed three** — `README.md`, `AGENTS.md`, `CLAUDE.md`.
Those are coding-agent conventions; this server's audience is research/notes/docs/
papers, where the orientation file is often `index.md`, an `_index.md`, a table of
contents, or `ABOUT`/`OVERVIEW`, and AGENTS/CLAUDE are usually absent. The fix is
**not** a config knob (every user would hand-maintain a list that rots on rename);
it's a broader **server-maintained recognizer** so a freshly-pointed workspace
just works. Depends on §12.12 landing first.
- [ ] Registry (`workspace/`): replace the hardcoded three with a built-in,
      curated stem set recognized **case-insensitively and extension-agnostically**
      at the tree root only — e.g. `readme`, `index`, `_index`, `contents`, `toc`,
      `overview`, `about`, `agents`, `claude`. Probe via `Root.Stat`/a single root
      `ReadDir`, presence only (metadata, not content), each candidate gated by the
      workspace's policy/`os.Root` exactly as in §12.12.
- [ ] Return the actual filenames found (so `README.rst` or `index.md` surface as
      themselves), newest-convention-first by a fixed priority order; cap the list
      (e.g. ≤ 5) so a noisy root can't flood `workspace_list`.
- [ ] Description fallback (§12.12) follows the same priority: derive the
      `description` from the highest-priority detected orientation file's first
      section, not `README.md` specifically.
- [ ] `mcp/`: no schema change — `workspace_list`'s `wellKnownFiles` already
      carries whatever subset is present (§12.12).
- [ ] Keep the recognizer a **closed, server-owned list** — deliberately not
      user-extensible. If a real corpus needs a name we don't recognize, add it to
      the built-in set (a one-line code change with a test), don't add a config
      surface. Revisit only if that proves insufficient in practice.
- [ ] Docs: note in [docs/design.md §5.5] that orientation files are auto-detected
      by convention (and *why* it's convention-over-config).
- [ ] Tests: each recognized stem/extension detected; case-insensitivity; non-root
      matches ignored; policy-blocked names omitted; the cap holds; priority order
      drives both the list and the description fallback.
- **Done when:** a workspace points the model at its real orientation file(s)
      without any per-workspace configuration, across common doc/notes conventions,
      staying presence-only and policy-gated.

---

## 13. Failure modes & error spec

- **Unknown workspace** → `UNKNOWN_WORKSPACE` (the named workspace isn't configured).
- **Git tool on a non-git workspace** → `NOT_A_GIT_REPO`.
- **Path escapes sandbox / blocked** → `POLICY_DENIED` + `reason`
  (`outside_root`, `blocked_glob`, `absolute_path`, `traversal`).
- **File not found** → `NOT_FOUND` (don't distinguish from denied where it would
  leak existence of blocked paths).
- **Bearer missing/invalid** → HTTP 401, no detail.
- **Oversize read** → truncated content + `"truncated": true`.
- **Bad regex pattern** (`fixedString: false`) → `INVALID_PATTERN` with the compile
  error; no walk performed.
- **Bad line range** (task 13: `startLine`/`endLine` < 1, or `startLine` >
  `endLine`) → `INVALID_RANGE`; out-of-bounds-but-ordered ranges clamp instead.
- **Patch context mismatch** (task 11) → `PATCH_CONFLICT`, "re-read the file and
  retry"; no partial write.

---

## 14. Runtime checklist (pre-flight, before real trees)

- [ ] Each `workspaces[].root` is an intended tree; server bound to `127.0.0.1`.
- [ ] Bearer token set in `.env` (not `config.yaml`); ngrok exposes only this
      server; edge auth on.
- [ ] Each workspace's `policy.allowGlobs` / `policy.blockGlobs` reviewed.
- [ ] `fsroot_escape_test` green; a `.env` read fails; a symlink escape fails;
      cross-workspace access fails.
- [ ] No write/shell tool in `tools/list`.
- [ ] A test read + grep succeed against a real claude.ai session.

---

## 15. Done definition (MVP = task 10 complete)

1. claude.ai connects to the server as a remote MCP server over HTTPS.
2. It can discover workspaces and list / read / fuzzy-find / grep allowed files in
   each, plus git status on git-repo workspaces.
3. No write/edit/create/delete/shell/Git-mutation path exists (pre-patch task).
4. Containment is enforced by a per-workspace `os.Root`; policy refuses sensitive
   in-tree paths; cross-workspace access is impossible.
5. Escape attempts (`..`, absolute, symlink-out) provably fail in tests.
6. All policy decisions audit-logged with the token redacted.
7. README explains safe operation, the read-only guarantee, and shutdown.

---

## 16. Future enhancements (each its own decision)

- Read-only `git_diff` (working-tree diff) for orientation.
- **Read-only `git_log` (commit history).** Surface recent commit history for
  orientation, via go-git's `repo.Log(&git.LogOptions{...})` (still pure Go, no
  binary). Two scopes from one tool:
  - **Whole-workspace:** `{ "workspace": "default", "limit": 50 }` → newest-first
    `{ "commits": [{ "hash", "author", "date", "subject" }] }`.
  - **Per-file:** add `"path": "docs/x.md"` → only commits that touched that path
    (`LogOptions.FileName` / `PathFilter`), so the model can ask "how did this file
    evolve?". The `path` rides the workspace's `os.Root` + policy like every other
    path arg; a blocked/absent path → `POLICY_DENIED`/`NOT_FOUND` (history is
    metadata, but gate the path the same way). Git-repo workspaces only, else
    `NOT_A_GIT_REPO`. Cap commits (`limit`, default + max); never expose full diffs
    here (that's `git_diff`). Optionally include short stats per commit.
- **Read-only `git_blame` (line authorship).** Per-line last-touched info for one
  file, via go-git `git.Blame(commit, path)`:
  `{ "workspace": "default", "path": "docs/x.md" }` →
  `{ "path", "lines": [{ "line", "hash", "author", "date" }] }` (optionally the
  line text, subject to `read.maxBytes`). Honors the same `os.Root` + policy +
  binary handling as `file_read`; git-repo workspaces only, else `NOT_A_GIT_REPO`.
  Composes naturally with ranged `file_read` (§12.13) — blame a span, then read it.
  Note go-git blame can be heavy on large/long-history files; bound it (line cap,
  maybe a size guard) and document the cost.
- Per-client tokens (scoped to specific workspaces), rotation without restart,
  session expiry.
- Rate limits and a per-session byte budget.
- OAuth connector instead of static bearer.
- Optional symbol search — only if it earns its weight; pull an LSP/ctags library
  per-feature, not a whole running program.
- Upstream the bearer `Processor` into `github.com/mnehpets/http`.
- **Native stdio dispatch in `jsonrpc`.** The `-stdio` transport currently reuses
  the HTTP path by driving the handler in-process per message (a synthetic
  `httptest` request/recorder round-trip in `cmd/shim`). It works and reuses all
  tool gating verbatim, but routes stdio through an HTTP shim. Improve
  `github.com/mnehpets/http`'s `jsonrpc` to expose a transport-agnostic dispatch
  entry point (e.g. `HandleMessage(ctx, []byte) ([]byte, error)`, with `nil` for
  notifications) so a stdio server can call it directly with no `net/http` /
  `httptest` involvement. Then rewrite `serveStdio` against that API and drop the
  in-process HTTP round-trip. (Not started — deliberate; do after MVP.)
- **Discovery in a large repo.** `tree_grep`/`tree_find` are lexical; in a big
  tree the model can't easily find the *right* file without already knowing a
  term. Add a higher-level discovery aid. Two candidate approaches (pick one, or
  offer both):
  - **Repo manifest.** Honor a checked-in manifest (e.g. `.workspace-mcp.yaml` or
    a `MANIFEST.md`) that maps paths/areas to human descriptions; expose it via a
    `workspace_overview` (or `tree_manifest`) tool so the model gets an annotated
    map before drilling in. Cheap, deterministic, author-controlled, no index to
    maintain — but only as good/fresh as the hand-written doc.
  - **Semantic search.** Build/maintain an embedding index over allowed files and
    add a `tree_search` tool that ranks by meaning, not substring. Far better
    recall on "where is X handled?" queries — but needs an embedding model
    (local or API), an index to build/refresh/invalidate (mind the os.Root +
    policy + ignore filters so nothing blocked is ever indexed), and storage.
    Heavier; keep it optional and per-workspace.
  Likely start with the manifest (low cost, immediate UX win) and treat semantic
  search as a later, opt-in upgrade. (Not started.)
- **Enrich workspace descriptions.** *Promoted to task §12.12* (config/README-derived
  `description` + `wellKnownFiles` on `workspace_list`). What remains here as future
  work: the in-tree **repo manifest** half (the `tree_manifest`/`workspace_overview`
  surface above) and folding `description` into the per-tool `workspace` enum text,
  if §12.12's optional step isn't taken. (If we don't enrich it, reconsider whether
  `workspace_list` earns its slot at all.)
