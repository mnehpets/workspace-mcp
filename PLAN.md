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
developer tool. **No structured-metadata query** (field-scoped predicates over YAML
frontmatter, database-style facets): it serves querying of a *known* schema, which
contradicts this server's orient-an-*unfamiliar*-tree premise — content search already
finds metadata values as plain text. If ever needed it's a separate, specialised tool.
(Surfacing the raw frontmatter block as text, or splitting matches by the `---` fence, is
fine — that's bytes and position, not a parse or a query. See §8.4.)

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
(`mcp/auth.go`), 401 on missing/invalid, no disclosure of which. Upstream later
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
`tree_search`, and use go-git **only** for `git_status` +
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
`example/config.example.yaml` and `example/secrets.example.env`; gitignore
`config.yaml` and `secrets.env`.
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
- **`wellKnownFiles`** — which orientation files exist at the tree root, recognized
  by convention (case-insensitive, extension-agnostic stems: `readme`, `index`,
  `_index`, `contents`, `toc`, `overview`, `about`, `agents`, `claude`), policy-gated,
  priority-ordered, capped — e.g. `["README.md", "CLAUDE.md"]`. Presence only —
  metadata, not content. See §12.12 / §12.17.

### 8.2 `tree_list` — FOLDED INTO `tree_search`
Removed as a separate tool. A where-less `tree_search` path-glob query enumerates
files with their `size` (e.g. `path: "*"` for the root, `"docs/**"` for a subtree),
which is everything a directory listing offered; directory entries themselves were
dropped (file paths already convey structure). See §8.4.

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

### 8.4 `tree_search` (content search needs the workspace's `grep.enabled`)
One tool to locate files — by path and by content — replacing the former
`tree_find`/`tree_grep` split. The find/grep boundary encoded two match *vocabularies*
(glob/name vs literal-or-regex content), not two tools; one tool with a path glob and
typed content predicates expresses both, without the selection tax of two lexical tools.
Returns a flat file list; the caller chooses whether to hydrate matched lines per hit.
`{ "workspace": "default", "path": "docs/**/*.md",
   "where": [ { "text": "ASC workflow", "fixedString": true } ],
   "includeMatches": true }`
→ `{ "files": [{ "path", "matches"?, "metadataMatches"?, "metadata"? }], "truncated" }`
(`matches`/`metadataMatches` are each a list of `{ "line","text" }`; `metadata` is a string).
- **`path`** — a glob selecting candidate files (boundary *and* name filter in one;
  e.g. `docs/**/*.{md,txt}`). Omit for the whole tree. Fuzzy name matching was
  considered and dropped: an LLM globs broad then narrows by inspecting results, so
  typo-tolerant ranking is wasted on it — which collapses the old `query` into `path`.
- **`where`** — content predicates over the file body, AND-combined; a file must
  satisfy every one. Each predicate is a `text` expression + modifiers.
  - **`fixedString: true` (default)** — literal substring (grrep `Matcher` fast path).
  - **`fixedString: false`** — Go `regexp` with grrep's literal pre-filter.
  - Optional `caseInsensitive` / `wordBoundary` map to grrep `MatchOpts` (`-i`/`-w`),
    **per predicate** (not a top-level mode).
- **`includeMatches`** (default on) — attach the matched lines (`line`,`text`) per file
  so the caller can triage from snippets; set false for just the path list. Matches that
  fall inside a leading `---`…`---` **frontmatter fence** come back separately as
  `metadataMatches` (vs body `matches`), so the caller can tell a *declared* topic
  (`tags: [california]`) from an incidental body mention. Fence detection only — **no**
  YAML parser; it's the same boundary `includeMetadata` uses.
- **`includeMetadata`** (default off) — attach each file's frontmatter block as **raw,
  unparsed text** (the bytes between the leading fences) in `metadata`. The caller parses
  it if it cares; the server hands over bytes, never a typed/queryable schema. No-op on
  files without a fence.
- Field-scoped *predicates* (matching within a named field) stay out of scope — see §3.
  The server finds the fence; it never parses or queries the YAML inside it.
Backed by the vendored grrep core (§6): `fastwalk` traversal filtered by `IgnoreSet` +
policy globs + `.git`/dotfile skip; binary files skipped (NUL probe); each leaf opened
via `os.Root`; worker pool sized by the workspace's `grep.workers`; capped at
`grep.maxMatches` (`truncated: true` when hit). A pure path-glob query (no `where`)
needs only the walk; content predicates additionally need `grep.enabled`.

### 8.5 `git_status` (git-repo workspaces only)
Read-only git status for orientation, via go-git `Worktree().Status()` + current
branch (`repo.Head()`) → `{ "branch", "files": [{ "path","status" }] }`. On a
non-git workspace: `NOT_A_GIT_REPO`. No mutation, no `git` binary.

### 8.6 Hard exclusions
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

```text
workspace-mcp/
  PLAN.md
  README.md
  NOTICE                         # attribution for vendored grrep (Apache-2.0)
  go.mod
  .gitignore
  example/
    config.example.yaml          # copy to config.yaml (gitignored) and edit
    secrets.example.env          # copy to secrets.env (gitignored) for secrets
    mcp.example.json             # .mcp.json template for Claude CLI registration
  cmd/workspace-mcp/
    main.go                      # wire config, workspaces, MCP endpoint, listen
    main_test.go
  mcp/
    config.go                    # YAML load + validation (typed struct)
    secrets.go                   # godotenv + os.Environ merge; resolve {env:NAME} refs
    auth.go                      # bearer-token + OAuth 2.0 Processor
    oauth.go                     # OAuth 2.0 authorization code flow
    registry.go                  # name → {*os.Root, policy, IgnoreSet, isGitRepo}
    root.go                      # os.Root wrapper: safe open/read/list/walk (per workspace)
    policy.go                    # glob allow/deny, dotfile rules (per workspace)
    search.go                    # tree_search engine: path glob + where predicates,
                                 #   frontmatter-fence split
    walk.go                      # fastwalk traversal + os.Root leaf-open + worker pool
    log.go                       # redacting slog logger
    server.go                    # initialize / tools/list / tools/call
    tools.go                     # workspace_list/file_read/tree_search/git_status defs
  grrep/                         # vendored from bep/grrep (Apache-2.0, SPDX retained)
    match.go                     #   verbatim: Matcher + CompileMatcher
    scan.go                      #   adapted: scan core → structured {path,line,text}
    ignore.go                    #   IgnoreSet (gogitignore): nested .gitignore/.ignore
  gitaware/
    detect.go                    # is this tree a git repo? (go-git PlainOpen)
    status.go                    # go-git Worktree().Status() + branch
    tracked.go                   # tracked-file enumeration (index/worktree)
  openspec/                      # OpenAPI specs and changelogs
    specs/
    changes/
  test/
    auth_test.go
    fileread_binary_test.go
    fileread_range_test.go
    fsroot_escape_test.go        # symlink/.. escape attempts must fail
    gitaware_test.go
    integration_test.go
    mcp_test.go                  # only workspace_list/file_read/tree_search/git_status exposed
    mcphelp_test.go
    orientation_test.go
    policy_test.go
    search_test.go               # tree_search: path glob, where predicates, fence split
    secrets_test.go              # env override + {env:NAME} resolution
    workspace_test.go            # unknown workspace, cross-workspace isolation
```

Packages live at the repo root, not under `internal/` — this is an application,
not a library meant for external import, so `internal/` would add nesting without
buying anything.

---

## 12. Task list

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
- [ ] **Track calls-to-first-read** — the number of tool calls the model makes
      before it reads the right file(s) — as a secondary metric. It is *not* as
      critical as correctness (a slow-but-right session still passes), but a lower
      count directly cuts latency and token cost, so the wording/steering should be
      tuned to minimize it (e.g. one metadata-enriched `tree_search` instead of a
      list pass then a re-list). Note the confound: early testing shows a ~100%
      "hit-rate" precisely when the model *doesn't* use the workspace at all and
      answers from its own knowledge — so calls-to-first-read must be read
      alongside "did it actually consult the tree", not in isolation.
- [ ] **Instructions probe:** put a distinctive, falsifiable directive in the
      `instructions` string (e.g. a specific first-call convention) and check
      whether behavior reflects it — the only practical way to tell if claude.ai
      surfaces `instructions`. Record the verdict; if ignored, lean harder on the
      description/`workspace_list` layer and note it in [docs/design.md §5.5].
- [ ] Feed failures back into the description/annotation/`instructions` wording and
      re-run; iterate until the hit-rate is acceptable. Treat the prose as prompts
      under test, per the writing-tools-for-agents guidance.
- **Done when:** the prompt set runs against a live claude.ai connector with a
      recorded hit-rate *and* a recorded calls-to-first-read, the
      instructions-honored question is answered, and any wording fixes from the
      first pass have landed. Pairs with the §10 / §14 live checks (folds into
      §12.10's verification rather than duplicating it).

### 17. Workspace-per-URL — select the workspace by route, drop the `workspace` param
Move workspace selection from a tool argument to the HTTP route: one workspace per
endpoint (`POST /mcp/<name>`, `GET /mcp/<name>`), so a claude.ai connector URL *is*
a workspace. This collapses a whole column of accidental complexity — the
`workspace` param, its config-driven enum, the `"default"` fallback, and
`UNKNOWN_WORKSPACE` as a per-call domain error all disappear; "resolve workspace"
stops being step (4) of the gate (§9, §10.1) and becomes routing done once at the
HTTP layer. The registry stays; it's keyed by the path segment instead of an arg.
Rationale and trade-offs are in [docs/design.md] (the single-workspace-at-a-time
use case; register more endpoints for more trees).
- [x] Route the MCP endpoints under a workspace path segment in `BuildHandler`
      ([mcp/handler.go]); map `<name>` → registry entry at dispatch, 404 (not
      a domain error) on an unknown segment. Keep auth server-wide (one bearer/OAuth
      for all paths; AuthZ stays per-workspace policy). *(buildHandler moved from
      cmd into mcp as the reusable `BuildHandler`, one endpoint per workspace.)*
- [x] Drop the `workspace` property from every tool's input schema and handler
      ([mcp/tools.go]); remove the enum-from-config plumbing and the
      `UNKNOWN_WORKSPACE` path. The selected `*os.Root`/policy comes from the route.
      *(`Server` now binds one `*Workspace`; `resolveWS`/`mapWorkspaceError` removed.)*
- [x] **stdio has no URL** — add `-workspace <name>` (or require a single-workspace
      config in stdio mode) so `serveStdio` ([cmd/workspace-mcp/main.go]) binds one
      workspace. Document it. *(`selectStdioWorkspace`: implicit when one configured,
      named via `-workspace` otherwise.)*
- [x] **OAuth well-known under a path prefix** — verify how claude.ai resolves
      `.well-known/oauth-protected-resource` / `oauth-authorization-server` (RFC 9728)
      when the MCP endpoint sits under `/mcp/<name>`. One auth server for all paths is
      the intent; confirm the protected-resource metadata still resolves. *(Served at
      both the bare path and `.../mcp/<name>`; `resource` echoes the per-endpoint path,
      one `authorization_servers` entry. Live claude.ai confirmation still pending.)*
- [x] **Emit `WWW-Authenticate` on the 401** (RFC 9728, MCP 2025-11-25) — the bare
      401 ([mcp/auth.go]) should carry `WWW-Authenticate: Bearer resource_metadata="…"`
      pointing at this endpoint's `.well-known/oauth-protected-resource`. *(Added via
      `Bearer.WithResourceMetadata`, enabled only when OAuth is configured; names the
      per-`<name>` resource from the request path.)*
- [x] **Remove `workspace_list`** — with one workspace per endpoint, cross-workspace
      discovery has no job. Its orientation payload (`description` + `wellKnownFiles`)
      moves to the two tools-only homes below (§18-adjacent). Update
      `test/workspace_test.go`, `test/mcp_test.go`, and the §5.3/§8.1 docs.
- [x] **Orientation home (1): `initialize` instructions.** The instructions string
      is now workspace-specific (the URL picked the tree) — render that workspace's
      `description` + detected `wellKnownFiles` (already computed in [mcp/registry.go])
      into it, so the model is told what the tree is and where to start before any
      reasoning ([mcp/server.go]). *(Rendered via a `text/template`.)*
- [x] **Orientation home (2): a no-arg orient tool.** Replace `workspace_list` with a
      no-param tool (e.g. `workspace_info` / `orient`) returning *this* workspace's
      `description` + `wellKnownFiles`. *(Implemented as `workspace_info`. It returns
      the SAME payload as the initialize `instructions` — an `orientation` field with
      that exact text plus the structured source fields — so the two are true mirrors.
      Since the orientation is already embedded in `instructions`, the model is NOT told
      to "call workspace_info first"; the tool is purely the fallback for hosts that
      ignore `instructions`.)*
- **Done when:** a connector points at `/mcp/<name>`, no tool takes a `workspace`
      arg, `workspace_list` is gone, the model is oriented via instructions and/or the
      orient tool, stdio binds one workspace, and cross-workspace isolation (separate
      `os.Root` per endpoint) still holds in tests. ✓ *(All green except a pre-existing,
      unrelated `TestResolveBearerTokensNoneSetIsError` failure.)*

### 18. Spec conformance — MCP 2025-11-25
Pick up the revision's relevant deltas. Most of the 2025-11-25 changes don't touch a
read-only tools-only server (tasks, OIDC discovery — already done, incremental scope —
N/A with one global scope, sampling/elicitation/roots — client features, icons —
cosmetic). Two small ones are worth taking; a third lives in §17 (`WWW-Authenticate`).
- [ ] **Negotiate `2025-11-25`.** Prepend it to `supportedProtocols` (newest-first)
      ([mcp/server.go]); the existing fallback logic needs no other change.
- [ ] **Origin-header validation → HTTP 403.** This revision makes it explicit:
      Streamable HTTP servers must reject invalid `Origin` with 403 (DNS-rebinding
      defense). Add an allowlisted-origins check ahead of `/mcp` in `buildHandler`
      ([cmd/workspace-mcp/main.go]) — configurable, defaulting to the claude.ai origin;
      most relevant on the ngrok-fronted path.
- [ ] **Confirm input-validation errors stay tool-execution errors** (SEP-1303), not
      JSON-RPC protocol errors — our `INVALID_ARGS` already returns via the `isError`
      envelope (§13 / design §5.4); just verify the unmarshal path doesn't bubble a
      protocol error instead, so the model can self-correct.
- **Done when:** `initialize` negotiates `2025-11-25`, a request with a disallowed
      `Origin` gets a 401-independent 403, and the arg-validation path is confirmed to
      surface as a tool error.

### 19. Prompts — optional `prompts/` dir → `prompts/list` + `prompts/get`
Serve user-authored prompt templates from an optional per-workspace directory, via
the MCP `prompts/*` methods. Purely additive: read-only, same `os.Root`/policy
boundary, no security-model change. Composes with §17 — each endpoint = workspace =
its own prompts dir. Unlike resources (rejected, [docs/design.md §7]), prompts are
*meant* to be user-invoked (slash-command-style), so their needs-a-human property is
exactly right; this is the on-brand "research/workflow" surface (§1).
- [ ] Optional config field (e.g. `workspaces[].promptsDir`, workspace-relative);
      absent → server advertises no `prompts` capability for that workspace.
- [ ] Prompt file format: markdown with frontmatter (`name`, `description`,
      `arguments: [{name, description, required}]`) + a `{{arg}}` template body. One
      file = one prompt; decide the arg-substitution convention and document it.
- [ ] Implement `prompts/list` (enumerate the dir) and `prompts/get` (read + fill
      args) as new methods on the server ([mcp/server.go]); advertise the `prompts`
      capability at `initialize` only when a `promptsDir` is set.
- [ ] Read the dir through `os.Root`. Decide policy treatment: the dir is
      author-curated content *meant* to be served, so lean **exempt from
      `blockGlobs`** while still inside the sandbox (don't let a stray `*.md` block
      glob hide prompts) — but document the choice.
- [ ] Validate on the local `claude` CLI first (per the dev-loop), then confirm
      claude.ai surfaces prompts before treating them as load-bearing.
- **Done when:** a workspace with a `promptsDir` advertises `prompts`, `prompts/list`
      enumerates the templates, `prompts/get` returns a filled prompt, and a workspace
      without the dir is unaffected.

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
2. It can discover workspaces and list / read / search (by path glob and body
   content) allowed files in each, plus git status on git-repo workspaces.
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
  `httptest` request/recorder round-trip in `cmd/workspace-mcp`). It works and reuses all
  tool gating verbatim, but routes stdio through an HTTP shim. Improve
  `github.com/mnehpets/http`'s `jsonrpc` to expose a transport-agnostic dispatch
  entry point (e.g. `HandleMessage(ctx, []byte) ([]byte, error)`, with `nil` for
  notifications) so a stdio server can call it directly with no `net/http` /
  `httptest` involvement. Then rewrite `serveStdio` against that API and drop the
  in-process HTTP round-trip. (Not started — deliberate; do after MVP.)
- **Discovery in a large repo.** `tree_search` (§8.4) is lexical; in a big tree the
  model can't easily find the *right* file without already knowing a term. Add a
  higher-level discovery aid. Two candidate approaches (pick one, or offer both):
  - **Repo manifest.** Honor a checked-in manifest (e.g. `.workspace-mcp.yaml` or
    a `MANIFEST.md`) that maps paths/areas to human descriptions; expose it via a
    `workspace_overview` (or `tree_manifest`) tool so the model gets an annotated
    map before drilling in. Cheap, deterministic, author-controlled, no index to
    maintain — but only as good/fresh as the hand-written doc.
  - **Semantic ranking.** Build/maintain an embedding index over allowed files and
    add a meaning-ranked mode to `tree_search` (a `semantic`/`rank` option, not a new
    tool — the collapsed surface stays one search tool). Far better recall on "where is
    X handled?" queries — but needs an embedding model (local or API), an index to
    build/refresh/invalidate (mind the os.Root + policy + ignore filters so nothing
    blocked is ever indexed), and storage. Heavier; keep it optional and per-workspace.
  Likely start with the manifest (low cost, immediate UX win) and treat semantic
  ranking as a later, opt-in upgrade. (Not started.)
- **Enrich workspace descriptions.** *Promoted to task §12.12* (config/README-derived
  `description` + `wellKnownFiles` on `workspace_list`). What remains here as future
  work: the in-tree **repo manifest** half (the `tree_manifest`/`workspace_overview`
  surface above) and folding `description` into the per-tool `workspace` enum text,
  if §12.12's optional step isn't taken. (If we don't enrich it, reconsider whether
  `workspace_list` earns its slot at all.)
