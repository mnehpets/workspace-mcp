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
  once at startup.** We are on Go 1.25, so this is available.

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

Go (`go 1.25`), built on **`github.com/mnehpets/http`**.

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
`{ "workspaces": [{ "name", "isGitRepo" }] }`. Does not expose roots.

### 8.2 `tree_list`
List entries under a directory. `{ "workspace": "default", "path": "docs",
"recursive": false }` → `{ "entries": [{ "path", "type", "size" }] }`. Backed by
`Root`-scoped `ReadDir`/`WalkDir`, filtered by the workspace's `IgnoreSet`
(grrep/`gogitignore`) when its `respectGitignore` is set (and by its policy globs).

### 8.3 `file_read`
Read one allowed file. `{ "workspace": "default", "path": "docs/x.md",
"maxBytes": 100000 }` → `{ "path", "content", "truncated", "binary" }`. Opens via
`Root.Open`; enforces the workspace's `read.maxBytes`; detects binary and flags
or refuses it.

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
requires it.

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
> to e.g. `git-tree-mcp` when convenient (out of MVP scope).

```text
git-tree-mcp/
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
- [ ] Module + directory skeleton per §11; `go build ./...` clean.
- [ ] `config/`: load the `-config` YAML (default `./config.yaml`) into a typed
      struct via `gopkg.in/yaml.v3` with `KnownFields(true)`. Supports a
      `workspaces` list.
- [ ] `config/secrets.go`: `godotenv` reads `-env` (default `./.env`) into
      a `map[string]string`; overlay `os.Environ()` (OS **overrides** dotenv);
      resolve each `{ env: NAME }` config reference (e.g. `auth.bearerToken`) — a
      missing/empty referenced var is a startup error.
- [ ] Validate: ≥ 1 workspace, unique names, a `default` exists; each `root` is an
      existing dir; port in range; resolved `bearerToken` ≥ 32 bytes; globs compile.
- [ ] `audit/`: `slog` logger that redacts the bearer token and never logs file
      contents.
- [ ] `cmd/shim`: wire config + secrets + logger; serve `GET /healthz` → `{"ok":true}`.
- [ ] `config.example.yaml` + `.env.example` committed; `config.yaml` + `.env`
      gitignored.
- **Done when:** `go run ./cmd/shim -config config.yaml -env .env` serves
  `/healthz`; malformed/unknown-key config or an unresolved secret fails fast with
  a clear error.

### 2. Bearer auth
- [ ] `auth/`: constant-time bearer `endpoint.Processor`; 401 on missing/invalid
      with no hint as to which.
- [ ] Wire it ahead of every route except `/healthz`.
- [ ] Tests: missing / wrong / valid token; assert the token never appears in logs.
- **Done when:** unauthenticated requests get 401, valid passes, redaction test green.

### 3. Sandbox core — `fsroot` over `os.Root` + workspace registry (the spine)
- [ ] `fsroot/`: open a tree's `root` with `os.OpenRoot`; expose safe
      `Open`/`Stat`/`ReadDir`/walk taking workspace-relative paths.
- [ ] `workspace/`: build a registry at startup — one entry per configured
      workspace holding `{*os.Root, policy, IgnoreSet, isGitRepo}` (git-ness via
      `gitaware.Detect`). Lookup by name; `UNKNOWN_WORKSPACE` otherwise.
- [ ] Reject model paths that are absolute or contain `..` before resolution;
      everything else resolves through that workspace's `*os.Root`.
- [ ] **`fsroot_escape_test.go` (the centerpiece):** absolute path rejected; `..`
      rejected; in-tree symlink → `/etc/passwd` cannot read it; in-tree symlink to
      another in-tree file behaves; concurrent-rename/TOCTOU cannot escape.
- [ ] `workspace_test.go`: unknown workspace rejected; a path valid in one
      workspace cannot reach another's tree.
- **Done when:** every escape test fails to read anything outside the root, and
  cross-workspace access is impossible. The boundary is ours and provable.

### 4. Path policy
- [ ] `policy/`: `policy.allowGlobs` allow + `policy.blockGlobs` deny (deny always
      wins); dotfile rule; shares the absolute/`..` rejection with fsroot.
- [ ] Tests: allowed globs pass; `.env`, `.git/**`, keys, `node_modules` blocked;
      `docs/../.env` blocked; dotfiles handled per rule.
- **Done when:** policy tests green; a path must clear **both** fsroot and policy
  to be served.

### 5. Search — vendor & adapt grrep
- [ ] `grrep/` package: `match.go` **verbatim** (SPDX retained); scan core adapted
      to emit structured `{path,line,text}`; `IgnoreSet` (gogitignore); add `NOTICE`.
- [ ] `search/grep.go`: `fastwalk` traversal with `.git`/dotfile skip, the
      workspace's `IgnoreSet` + policy filter, NUL-byte binary skip, **each leaf
      opened via `os.Root`**, worker pool sized by the workspace's `grep.workers`,
      cap `grep.maxMatches`.
- [ ] `search/find.go`: fuzzy filename search over the filtered tree.
- [ ] Tests: literal + regex hit; `fixedString`/`caseInsensitive`/`wordBoundary`;
      ignore + policy respected; binary skipped; cap → `truncated`.
- **Done when:** grep/find return correct in-sandbox results, no external binary.

### 6. Git-awareness — go-git
- [ ] `gitaware/`: `Detect(root)` (is it a git repo?); `git_status` via
      `Worktree().Status()` + branch from `repo.Head()`; tracked-file enumeration
      via index/worktree.
- [ ] Tests against a temp repo with staged / unstaged / untracked files, and a
      non-git tree (Detect false → `git_status` yields `NOT_A_GIT_REPO`).
- **Done when:** status returns branch + per-file codes on git workspaces and
  `NOT_A_GIT_REPO` on others; no `git` binary used.

### 7. MCP server — handshake + tool-list invariant
- [ ] `mcp/`: register `initialize`, `notifications/initialized`, `tools/list`,
      `tools/call` on the jsonrpc endpoint (slash names via `_ jsonrpc:"…"`).
- [ ] `initialize` returns protocol version + `{capabilities:{tools:{}}}`; reject
      unsupported versions.
- [ ] `tools/list` returns only `workspace_list` + `tree_*` / `file_*` / `git_*`
      with JSON Schemas (each tool but `workspace_list` includes a `workspace` param).
- [ ] `mcp_list_test.go`: no other surface leaks; `tools/call` rejects unknown
      tool names.
- **Done when:** an MCP client completes the handshake and sees only intended tools.

### 8. Read tools wired through `tools/call`
- [ ] Implement `workspace_list` / `tree_list` / `file_read` / `tree_find` /
      `tree_grep` / `git_status`, each (except `workspace_list`): bearer (done) →
      name allowlist → schema validate → resolve `workspace` → per-workspace
      enablement → path policy → fsroot/search/gitaware → size/count limits →
      audit log.
- [ ] Map failures to the error spec (§13).
- [ ] Integration test: two workspaces (one git, one not) + sample files + a
      symlink escape + a `.env`; `initialize` → `tools/list` → `workspace_list` →
      `tree_list` → `file_read` → `tree_grep` → `git_status`; allowed succeed,
      escapes/blocked/unknown-workspace/non-git fail; audit records present.
- **Done when:** all read tools work end-to-end and the integration test is green.

### 9. Transport + ngrok
- [ ] `POST /mcp` (jsonrpc) + `GET /mcp` (SSE via `SSERenderer`), Streamable-HTTP.
- [ ] Docs: `ngrok http 127.0.0.1:<server.port>` exposes only this server; reserved
      domain + edge auth; example `ngrok.yml`. Never expose anything else.
- **Done when:** reachable through the tunnel; unauthorized requests fail at both
  edge and server.

### 10. claude.ai connector + docs
- [ ] README: add as a custom connector (URL + bearer), the safety model, the
      read-only guarantee, example prompts, shutdown.
- [ ] Manual verify: a live claude.ai session lists / reads / greps the repo and
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
- Per-client tokens (scoped to specific workspaces), rotation without restart,
  session expiry.
- Rate limits and a per-session byte budget.
- OAuth connector instead of static bearer.
- Optional symbol search — only if it earns its weight; pull an LSP/ctags library
  per-feature, not a whole running program.
- Upstream the bearer `Processor` into `github.com/mnehpets/http`.
