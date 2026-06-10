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
- **Writes are a later task** (task 11 / §8.7): three exact-byte ops
  (create/overwrite/replace), not diff application. Per-workspace, default off;
  designed, not built, in MVP.

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

`os.Root` is essential for **model-supplied paths**: `file_read` and the write ops
(`file_create`/`file_overwrite`/`file_replace`, §8.7) take a path straight from the
model, which may be `../../etc/passwd`
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
- *No diff-parsing dependency.* The write surface (§8.7 / task 11) is exact-byte
  ops — `bytes.Count`/`bytes.ReplaceAll` over raw file content plus `crypto/sha256`
  for the optimistic-concurrency check — so `go-gitdiff` (or any unified-diff parser)
  is **not** pulled in.
- stdlib `os` (`os.Root`), `io/fs`, `bytes`, `regexp`, `crypto/sha256`, `log/slog` (redacting audit
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
    env: MCP_BEARER_TOKEN         # resolved from .env / OS env, never stored here

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
  env: MCP_BEARER_TOKEN     # name of an env var to read the value from
```

Resolution order at startup:
1. Read a `.env` file (path via `-env`, default `./.env`) with
   `github.com/joho/godotenv` into a `map[string]string`.
2. Overlay the process environment — `os.Environ()` **overrides** dotenv values
   (so a deployment can inject `MCP_BEARER_TOKEN` without a file).
3. Resolve each `{ env: NAME }` reference against that merged map; a missing or
   empty referenced var is a startup error.

A secret field may also be given as a plain string, but that's discouraged and
flagged in validation for `bearerToken` (keep tokens out of the YAML). `.env`:

```dotenv
# .env  (gitignored — never commit)
MCP_BEARER_TOKEN=replace-with-long-random-token   # >= 32 random bytes
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

## 8. Exposed tool surface (read-only by default; writes opt-in)

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
`{ "path", "content", "truncated", "binary", "startLine", "endLine", "totalLines", "sha256" }`.
Opens via `Root.Open`; enforces the workspace's `read.maxBytes`; detects binary and
flags or refuses it.
- **`startLine` / `endLine`** (optional, 1-based, inclusive) — return only that
  span instead of the whole file; either may be omitted (open-ended toward the
  start/end). The response echoes the resolved `startLine`/`endLine` and reports
  `totalLines` so the model can page (e.g. request the next 100). `maxBytes` still
  caps the *returned* span. Line ranges apply to text only; a binary file is
  flagged/refused as today regardless of range. See task §12.13.
- **`sha256`** — the hex SHA-256 of the file's **full** bytes (the same hash
  `base_sha256` checks, §8.7.4), independent of any line range or `maxBytes`
  truncation. This is the logical place to capture the hash: a read-then-write loop
  reads the file, carries its `sha256` straight into a subsequent
  `file_replace`/`file_overwrite` `base_sha256`, and gets the optimistic-concurrency
  guard with no extra round-trip. Computed over the whole file even when only a span
  is returned, so the guard covers the real on-disk state, not the slice. Binary
  files still report `sha256` (the hash is over bytes, not text).

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
  files without a fence. (Per-file hashing is **not** done here — `sha256` lives on
  `file_read`, §8.3, the natural read-then-write capture point; `tree_search` is a
  locator, not a content-hasher, and hashing every match would cost a full-file read per
  hit that the search itself doesn't need.)
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
Never exposed, always rejected: any shell/command execution, any Git mutation
(commit/push/branch/reset), and any file delete/move/rename. The opt-in write ops
of §8.7 (create/overwrite/replace) are the *only* mutation path, and only when a
workspace sets `write.enabled`; everything else mutating is absent from
`tools/list` and rejected in `tools/call`.

### 8.7 Write surface (opt-in, default off) — task 11

The deferred write task, reshaped from a single unified-diff `tree_patch` into
**three explicit byte-level ops** mirroring the Claude Code edit tools. The editing
loop needs all three — replace a unique span, overwrite a substantially-changed
file, create a new file — and a surface missing any one collapses back to
read-only. No diff parser, no git automation: deterministic edits with uniqueness
and optimistic-concurrency guards. The human reviews `git diff` and commits out of
band (§3 still forbids commit/push/branch).

**Per-workspace, opt-in, default off.** A workspace writes only when its config sets
`write.enabled: true`. With it off, the three tools are absent from `tools/list` and
any forced call returns `READ_ONLY`. Read-only stays the default posture of the
build (§10.3); writing is an explicit per-tree grant.

**Writable surface == readable surface.** A write target passes the *same*
`policy.CheckFile` (block wins, dotfile backstop, allowlist) a read does — no
separate write-allowlist to drift. Containment is the same per-workspace `os.Root`;
`Clean` rejects absolute/`..` first. So nothing writable is unreadable and nothing
blocked (`.env`, keys, `.git/**`) is writable.

#### 8.7.1 `file_create`
`{ "path": "docs/new/05.md", "contents": "…" }` → `{ "path", "bytesWritten", "sha256" }`.
New file only — `O_CREATE|O_EXCL` through `os.Root`; an existing path is **never**
clobbered (`PATH_EXISTS` → use `file_overwrite`). Missing parent dirs are
auto-created (`MkdirAll`, inside the sandbox). No `base_sha256`/`dry_run` — a new
path has nothing to race.

#### 8.7.2 `file_overwrite`
`{ "path", "contents", "base_sha256"?, "dry_run"? }` → `{ "path", "bytesWritten", "sha256" }`.
Full-file replace for files changing substantially (quoting `old_str` would be
silly). Fails if the path is absent (`NOT_FOUND` → use `file_create`), so a typo
can't silently create a stray file. `O_TRUNC|O_WRONLY`, no `O_CREATE`.

#### 8.7.3 `file_replace`
`{ "path", "old_str", "new_str", "expected_replacements"?=1, "base_sha256"?, "dry_run"? }`
→ `{ "path", "replacements", "sha256" }`.
Matches `old_str` against raw file bytes, replaces with `new_str`. The server counts
occurrences and rejects unless the count equals `expected_replacements` exactly
(`MATCH_COUNT_MISMATCH`, echoing the actual count, so the model knows whether to
lengthen the anchor or bump the parameter). Default `1` is the uniqueness guarantee
that makes the op safe; the parameter exists for the deliberate "change all N" case.
Empty `old_str` → `INVALID_ARGS`. The whole file is read (bounded by
`read.maxBytes`; larger → `FILE_TOO_LARGE`, never a partial replace).

#### 8.7.4 Cross-cutting behavior (this is where the reliability lives)
- **Exact-match, zero normalization.** No whitespace trim, no line-ending rewrite,
  no trailing-newline strip — match and write raw bytes. Silent normalization turns
  a safe rejection into a wrong-place edit. (Workspace convention is `\n`.)
- **`base_sha256` (optional, hex sha256 of the file's current full bytes), supported
  day one.** When supplied, the server rejects on mismatch (`BASE_SHA_MISMATCH`,
  returning the actual hash) — the only guard against the read-then-write race, which
  is real here because the tree syncs from GitHub out of band. Optional because a
  fresh hash isn't always in hand, but built now, not retrofitted.
- **`dry_run` on replace/overwrite.** Returns match count + before/after hashes
  without writing — lets the model confirm an `old_str` resolved uniquely before
  committing.
- **Structured, specific rejections** are part of the contract — each maps to a
  distinct next move: `MATCH_COUNT_MISMATCH` (with count), `BASE_SHA_MISMATCH`,
  `NOT_FOUND`, `PATH_EXISTS`, `POLICY_DENIED` (`outside_root`/`traversal`/
  `absolute_path`/`blocked_glob`), `READ_ONLY` (writes disabled), `FILE_TOO_LARGE`.
- **Audit** a content hash per change (§10.5). Never commit or push.

#### 8.7.5 Deliberately out of scope (now)
No batch/envelope op (single calls in the view→edit→view loop is the honest
pattern). No transaction/commit verbs (that's the deferred git-workflow design;
`dry_run` + `base_sha256` are the hooks it will build on). No move/rename/delete
(not needed for the editing loop; delete is the highest blast radius — add only on
real need).

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

### 10.3 Read-only by default; writes are an explicit per-workspace grant
A workspace is read-only unless its config sets `write.enabled: true` (§8.7). With
it off, no code path writes: the three write tools are absent from `tools/list` and
a forced call returns `READ_ONLY`, and the `*os.Root` is used only for read methods.
Where writes are granted, every write still resolves through that workspace's
`*os.Root` and clears the *same* `policy.CheckFile` a read does (block wins), so a
write can never escape containment or reach a blocked path. Read-only remains the
default posture of the build, not just a config value.

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

### 11. Write surface — `file_create` / `file_overwrite` / `file_replace` · per-workspace, default off
The payoff of going standalone. Reshaped from a single unified-diff `tree_patch`
into three explicit byte-level ops (§8.7) — no diff parser, no `go-gitdiff`.
Do **not** start until sections 1–10 are green. Full design in §8.7; this is the
build checklist.
- [x] **Config + gate.** Add `workspaces[].write.enabled` (default false) to the
      typed config (`mcp/config.go`) and carry it onto `Workspace` (`mcp/registry.go`).
      A shared `writeGate(path)` checks `write.enabled` (else `READ_ONLY`), then
      `Clean` + `policy.CheckFile` — the *same* allow/block a read uses, so the
      writable surface equals the readable one.
- [x] **Sandbox primitives** (`mcp/root.go`): `CreateNew` (`MkdirAll` parent then
      `O_CREATE|O_EXCL`), `WriteExisting` (`O_TRUNC|O_WRONLY`, no `O_CREATE`) — both
      through the workspace's `*os.Root`.
- [x] **`file_create`** — new file only; `PATH_EXISTS` on collision; auto-creates
      missing parent dirs; no `base_sha256`/`dry_run`.
- [x] **`file_overwrite`** — full-file replace; `NOT_FOUND` if absent; optional
      `base_sha256` and `dry_run`.
- [x] **`file_replace`** — exact-byte `old_str`→`new_str`; reject unless occurrence
      count == `expected_replacements` (default 1) with `MATCH_COUNT_MISMATCH`
      echoing the actual count; empty `old_str` → `INVALID_ARGS`; whole file read
      bounded by `read.maxBytes` (`FILE_TOO_LARGE` past it); optional `base_sha256`
      and `dry_run`.
- [x] **Cross-cutting:** zero normalization (raw bytes); `base_sha256` (hex sha256 of
      current bytes) rejects on drift with `BASE_SHA_MISMATCH`; `dry_run` returns
      counts/hashes without writing; audit a content hash per change. Never commit or
      push — the human reviews `git diff`.
- [x] **Surface gating:** the three tools appear in `tools/list` and the instructions
      prose flip to "writes allowed" *only* when `write.enabled`; write tools carry
      `readOnlyHint:false` (and `destructiveHint:true` for overwrite/replace).
- [x] **Tests** (`test/write_test.go`): each op's success + each structured rejection,
      no-normalization (trailing newline/CRLF preserved), cross-workspace isolation,
      and tools/list visibility on/off.
- **Done when:** the three ops apply deterministically through `os.Root`+policy,
  every structured rejection is returned with its distinct code, a write-disabled
  workspace exposes no write tool, and changes are reviewable/reversible via Git.
  Deterministic, no second LLM.

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
- [x] **Negotiate `2025-11-25`.** Prepend it to `supportedProtocols` (newest-first)
      ([mcp/server.go]); the existing fallback logic needs no other change.
- [x] **Origin-header validation → HTTP 403.** This revision makes it explicit:
      Streamable HTTP servers must reject invalid `Origin` with 403 (DNS-rebinding
      defense). Add an allowlisted-origins check ahead of `/mcp`. *(Implemented as the
      `OriginCheck` processor in [mcp/origin.go], wired into the per-workspace `/mcp/<name>`
      chain ahead of `Bearer` in `BuildHandler` ([mcp/handler.go]); configured via
      `server.allowedOrigins`, with `["*"]` to disable. The spec defines no client-sent
      Origin value and legitimate MCP traffic here is Origin-less, so there is NO baked-in
      default: an empty allowlist accepts Origin-less requests and 403s any present Origin
      (the only thing that sends one is a browser — the rebinding case) before auth runs.
      Note: for this server auth is the real boundary — the Origin check is spec-required
      defense-in-depth, load-bearing only for an unauthenticated browser-reachable
      plain-HTTP localhost deployment.)*
- [x] **Confirm input-validation errors stay tool-execution errors** (SEP-1303), not
      JSON-RPC protocol errors. *(Verified: `unmarshalArgs` ([mcp/tools.go]) returns an
      `INVALID_ARGS` `*toolError`, which `ToolsCall` ([mcp/server.go]) maps to an `isError`
      result with a `nil` JSON-RPC error — the unmarshal path never bubbles a protocol
      error, so the model can self-correct.)*
- **Done when:** `initialize` negotiates `2025-11-25`, a request with a disallowed
      `Origin` gets a 401-independent 403, and the arg-validation path is confirmed to
      surface as a tool error. ✓ *(All green; new `TestOriginValidation` covers the 403/
      pass-through cases, `TestInitializeUnknownProtocolNegotiatesDown` updated to the new
      newest version.)*

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

### 20. End-to-end tool-use eval (live `claude`)
The Go suite proves each op and rejection deterministically, but it can't tell us
whether a *model*, handed only the tool descriptions, drives the surface correctly:
picks the right tool, supplies the guardrail params, and recovers from a structured
rejection rather than thrashing. This is the behavioral companion to §16's
orientation eval, extended to the read/search/write edges — author a fixed,
re-runnable set of natural-language scenarios, feed them to a live `claude` session
against a writable workspace (dev loop: local CLI first, claude.ai as later
confirmation — see [[dev-loop-local-cli]]), and record whether the model does the
right thing. Keep the set in the repo (e.g. `test/eval/prompts.md`, alongside §16's
orientation prompts); record pass/fail + notes per scenario, and feed failures back
into the tool descriptions / `instructions` wording (treat prose as prompts under
test). Manual against a live session — not an automated harness.

**Read / search scenarios** (the paths the unit tests can't judge for tool-selection
quality):
- [ ] **Find-by-name vs find-by-content.** "Where's the config example?" → a path-glob
      `tree_search` (no `where`); "which files mention `base_sha256`?" → a `where`
      content predicate. Confirm the model picks the right axis instead of always
      grepping (or always globbing).
- [ ] **Ranged read on a large file.** Ask about something deep in a file past
      `read.maxBytes`; confirm the model pages with `startLine`/`endLine` (using the
      echoed `totalLines`) rather than giving up on a truncated read or re-reading the
      head repeatedly.
- [ ] **Frontmatter vs body distinction.** Ask "which notes are *tagged* california"
      (vs merely mention it); confirm the model reads `metadataMatches` separately from
      body `matches` and doesn't conflate a declared tag with an incidental mention.
- [ ] **`sha256` capture for a later write.** Confirm the model carries the `sha256`
      returned by a `file_read` straight into a subsequent
      `file_replace`/`file_overwrite` `base_sha256`, getting the optimistic-concurrency
      guard without a separate hash pass.
- [ ] **Policy-denied / not-found handling.** Ask it to read a blocked path (`.env`, a
      key) or a missing file; confirm it reports the `POLICY_DENIED`/`NOT_FOUND`
      gracefully and doesn't loop retrying or try to escape via `..`.

**Write scenarios** (exercise the guardrails the unit tests fire but a model must
*reach* on its own):
- [ ] **Tool choice.** "Create a new file X" → `file_create` (not overwrite); "rewrite
      this whole file" → `file_overwrite`; "change just this section" → `file_replace`.
- [ ] **Create-vs-overwrite recovery.** Ask it to create a file that already exists;
      confirm it reads the `PATH_EXISTS` rejection and switches to `file_overwrite`
      (rather than retrying create verbatim). Mirror with overwrite-on-absent →
      `NOT_FOUND` → `file_create`.
- [ ] **Anchor uniqueness under `MATCH_COUNT_MISMATCH`.** Ask for an edit whose `old_str`
      occurs multiple times; confirm the model lengthens the anchor or sets
      `expected_replacements` deliberately, and doesn't blindly bump the count to silence
      the error.
- [ ] **Concurrency guard.** Pass a captured `base_sha256`, simulate out-of-band drift,
      and confirm the model surfaces/handles `BASE_SHA_MISMATCH` (re-reads, retries)
      rather than forcing the write.
- [ ] **`dry_run` before commit.** Check whether the model uses `dry_run` to confirm an
      `old_str` resolved uniquely before the real write on ambiguous edits.
- [ ] **Delete-text via empty `new_str`.** Ask it to remove a section; confirm it reaches
      for `file_replace` with an empty `new_str` (there is no delete tool) and lands the
      excision in the right spot — the workflow gap noted in earlier manual testing.
- [ ] **No-delete/move/rename boundary.** Ask for a rename/move/delete; confirm the model
      recognizes the boundary (those tools are absent by design, §8.7.5) and proposes a
      git-side cleanup rather than hallucinating a tool. Capture the friction (stray files
      after create-then-relocate) as a known rough edge, not a bug.
- **Done when:** the scenario set runs against a live `claude` session on a writable
  workspace with recorded pass/fail per scenario, the read/search edges and the
  rejection-recovery / delete-text write paths are confirmed model-reachable, and any
  wording fixes from the first pass have landed. Pairs with §16 (shared eval-prompts
  home) and the §14 pre-flight.

### 21. Read-only `git_diff` — working-tree diff for orientation
Surface the working-tree diff so the model can see *what changed* before reading or
editing — the natural companion to `git_status` (which lists changed paths; this
shows the content). Git-repo workspaces only (else `NOT_A_GIT_REPO`), read-only, no
`git` binary (go-git, pure Go, like `git_status`). Metadata-adjacent but it *does*
return file content, so the path arg gates exactly like a read.
- [ ] **`git_diff` tool** ([mcp/tools.go]) — `{ "path"?: string, "staged"?: bool }`
      → `{ "diff": string, "truncated": bool, "notice"?: string }` (or a structured
      per-file hunk list — decide during build; a unified-diff string is the simpler
      first cut). Whole-worktree when `path` omitted; scoped to one file when given.
      `staged: true` diffs the index vs HEAD, else worktree vs index/HEAD.
- [ ] **go-git backend** ([gitaware/], new `diff.go` beside [gitaware/status.go]) —
      derive the patch from `Worktree().Status()` + the relevant trees
      (`Patch`/`Object` APIs). Reads via go-git's own billy FS (metadata-style), but
      because it emits content, see the gating bullet.
- [ ] **Path gating + limits.** Any `path` rides the workspace's `os.Root` + policy
      like every other path arg — a blocked/absent path → `POLICY_DENIED`/`NOT_FOUND`
      (never diff a `.env`/key). Cap the emitted diff by `read.maxBytes`; on the cap,
      `truncated: true` + a steering `notice` (narrow to a `path`), per §8.4's
      truncation-steers convention. Skip/flag binary file diffs.
- [ ] **Annotations + tests** — `readOnlyHint: true`, `openWorldHint: false` like the
      other reads; `test/gitaware_test.go` covers a dirty worktree (added/modified/
      deleted), staged vs unstaged, a path-scoped diff, a blocked path → `POLICY_DENIED`,
      and a non-git workspace → `NOT_A_GIT_REPO`.
- **Done when:** `git_diff` returns the working-tree (and staged) diff for a git
  workspace, honors `os.Root` + policy on any `path`, caps by `read.maxBytes` with a
  steering notice, and returns `NOT_A_GIT_REPO` off a git repo — no `git` binary, no
  mutation. Composes with `git_status` (list changed → diff them) and ranged
  `file_read`.

### 22. Relocate `sha256` from `tree_search` to `file_read`
Move the optimistic-concurrency hash off the locator and onto the reader. `sha256`
was attached to `tree_search` results under `includeMetadata` (the old §12.11
companion item), but a search is a *locator* and hashing every hit forces a full-file
read the walk doesn't otherwise need. `file_read` is the logical home: the
read-then-write loop already reads the file, so it can carry the file's hash straight
into a subsequent `file_replace`/`file_overwrite` `base_sha256` with no extra
round-trip. Design updated in §8.3 / §8.4.
- [x] **Remove from `tree_search`** ([mcp/search.go]): drop the per-file hashing and
      the `sha256` field from the returned file shape; the `includeMetadata` gate now
      attaches only the frontmatter `metadata` block. Update `test/search_test.go`.
      *(`hashFileFull` moved to [mcp/write.go] beside the other hash helpers; the two
      `TestSearchSHA256*` cases removed.)*
- [x] **Add to `file_read`** ([mcp/tools.go]): attach the hex SHA-256 of the file's
      **full** bytes (`crypto/sha256`, read through `os.Root`) as a `sha256` field on
      every `file_read` response (§8.3) — the *same* hash `base_sha256` checks.
      Computed over the whole file even for a ranged read (`startLine`/`endLine`) or a
      `maxBytes`-truncated one, so the guard covers the real on-disk state, not the
      returned slice. Binary files report it too (hash is over bytes). Independent of
      `write.enabled` — it is a read. *(When nothing was truncated `data` already is the
      whole file, so it's hashed directly; otherwise re-read via `hashFileFull` bounded
      by `read.maxBytes` — empty for a file past the cap, which is uneditable anyway.)*
- [x] **Tests** ([test/fileread_sha256_test.go]): `sha256` present and equals the file's
      real hash; stable across a ranged/truncated read of the same file; round-trips
      into a `file_replace` `base_sha256` (matches → write, drift →
      `BASE_SHA_MISMATCH`).
- **Done when:** `tree_search` no longer returns `sha256`, every `file_read` returns
  the full-file `sha256`, and the captured hash feeds a write's `base_sha256` guard
  without a separate hash pass.

### 23. Built-in zrok tunnel (config-driven, not ambient zrok env)
Add zrok as a second built-in tunnel option beside ngrok (§ config `server.ngrok`),
using the zrok **Go SDK** in-process — no external `zrok` process, no `zrok.yml`. The
non-negotiable constraint: drive the SDK **entirely from our `config.yaml` + secret
refs**, *not* from zrok's ambient environment. The SDK's default path loads an
enabled environment from `~/.zrok` (or `ZROK_*` env vars); we deliberately bypass
that and construct the zrok root/share from values we resolve ourselves, so the
server is self-contained and reproducible (same posture as the ngrok integration,
which takes its authtoken from a `{ env: … }` SecretRef, not ngrok's own config).
- [x] **Config shape** ([mcp/config.go]): add `server.zrok` mirroring `NgrokConfig`
      — `enabled: bool`, an `enableToken` **SecretRef** (the zrok account/enable
      token, resolved via `.env`/OS env like `ngrok.authtoken`), an optional
      `apiEndpoint` (default zrok.io), and a share target (`shareMode`/`backendMode`
      and optional reserved `uniqueName`/`frontend`). Add a `ResolveZrokEnableToken`
      resolver beside `ResolveNgrokAuthtoken`. Validation: exactly one of
      `ngrok.enabled` / `zrok.enabled` may be true (they both replace the local TCP
      listener); `enableToken` required when `zrok.enabled`.
- [x] **Construct the root from config, not disk** ([cmd/workspace-mcp/main.go], new
      `serveZrok` beside `serveNgrok`): build the zrok environment `*Root` from the
      resolved `enableToken` + `apiEndpoint` in memory rather than calling the SDK's
      `environment.LoadRoot()` (which reads `~/.zrok`). This is the crux of the task —
      if the SDK only supports a disk-loaded root, wrap/populate an in-memory root from
      our values; do **not** fall back to ambient `ZROK_*` env or a `~/.zrok` file.
- [x] **Create the share + serve** : open a zrok share (proxy/HTTP backend) for the
      handler, log the resulting public URL at startup (like ngrok's
      `listener.URL()`), and `http.Serve` over the share's listener. Ensure clean
      shutdown releases the share (especially ephemeral shares) so we don't leak
      reservations.
- [x] **Config example + docs**: add a commented `server.zrok` block to
      `example/config.example.yaml` and `config.local.yaml`, and a
      `ZROK_ENABLE_TOKEN` line to `example/secrets.example.env`. Note the
      one-tunnel-at-a-time rule and that the token comes from secrets, never the YAML.
- [x] **Tests** ([test/zrok_config_test.go]): zrok config parses;
      `ResolveZrokEnableToken` resolves from env, errors when unset, and is empty
      when disabled; both-tunnels-enabled and missing-enableToken configs are
      rejected. (The live tunnel dial itself stays a manual/pre-flight check, like
      ngrok.)
- **Done when:** `server.zrok.enabled: true` with an `enableToken` secret ref brings
  up an in-process zrok tunnel whose URL is logged at startup, the SDK is fed solely
  from our config (no `~/.zrok` / `ZROK_*` ambient dependency), ngrok and zrok are
  mutually exclusive, and the auth/policy/`os.Root` boundary is unchanged (the tunnel
  only replaces the listener).
- **Status (2026-06-10, written via the web connector — not yet compiled):**
  implemented against zrok SDK **v2** (`github.com/openziti/zrok/v2@v2.0.4`, latest
  tag). `env_core.Root` turned out to be an *interface*, so [cmd/workspace-mcp/zrok.go]
  implements an in-memory `zrokRoot` (the SDK calls we use only exercise `Client()`,
  `Environment()`, `IsEnabled()`; disk-flavored methods are stubbed). An *ephemeral*
  environment is enabled at startup and disabled on shutdown (SIGINT/SIGTERM handler;
  share deleted then environment disabled, `sync.Once`-guarded). `sdk.NewListener`
  insists on a ziti identity *file*, but `ziti.NewConfigFromFile` is just
  ReadFile+Unmarshal, so we inline `json.Unmarshal` + `ziti.NewContext` and the key
  never touches disk. **v2 API drift vs this plan:** the REST share request dropped
  `Reserved`/`UniqueName`; stable names are now `NameSelections` (`namespace:name`),
  so config `frontend`/`uniqueName` map there, and `shareMode`/`backendMode` are
  deliberately fixed at public/proxy (anything else defeats the purpose).
- **Status (2026-06-10, finished in Claude Code):** `go get ...@v2.0.4 && go mod
  tidy && go build ./...` all clean — the hand-verified v2.0.4 code compiles as
  written (no API drift beyond the NameSelections change already noted). `go vet`
  and the full `go test ./...` pass. Added the `ZROK_ENABLE_TOKEN` line to
  `example/secrets.example.env`, the commented `server.zrok` block to
  `config.local.yaml`, and [test/zrok_config_test.go] (parse + resolver +
  validation). The compiled `cmd/workspace-mcp/workspace-mcp` binary is now
  gitignored. **Task complete** — the live tunnel dial remains a manual pre-flight
  check (needs a real zrok account token), as with ngrok.

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
- **Write to a read-only workspace** (task 11; `write.enabled` not set) → `READ_ONLY`;
  the write tools are also absent from `tools/list` for that workspace.
- **`file_create` onto an existing path** → `PATH_EXISTS` ("use file_overwrite").
- **`file_overwrite`/`file_replace` on a missing path** → `NOT_FOUND`.
- **`file_replace` occurrence count ≠ `expected_replacements`** → `MATCH_COUNT_MISMATCH`
  with the actual count found; no write.
- **`base_sha256` mismatch** (file changed since read) → `BASE_SHA_MISMATCH` with the
  file's actual hash ("re-read and retry"); no write.
- **Write target exceeds `read.maxBytes`** (file too large to read-modify-write
  safely) → `FILE_TOO_LARGE`; no partial write.

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
3. No shell/command, file delete/move/rename, or Git-mutation path exists; file
   create/overwrite/replace exist only on workspaces that opt in via `write.enabled`
   (§8.7), and are absent entirely on read-only (default) workspaces.
4. Containment is enforced by a per-workspace `os.Root`; policy refuses sensitive
   in-tree paths; cross-workspace access is impossible.
5. Escape attempts (`..`, absolute, symlink-out) provably fail in tests.
6. All policy decisions audit-logged with the token redacted.
7. README explains safe operation, the read-only guarantee, and shutdown.

---

## 16. Future enhancements (each its own decision)

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
