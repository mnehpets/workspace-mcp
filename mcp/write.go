// The opt-in write surface: file_create / file_overwrite / file_replace. Three
// explicit byte-level ops (PLAN §8.7), default off per workspace. There is no
// diff parser and no git automation — deterministic edits with uniqueness
// (expected_replacements) and optimistic-concurrency (base_sha256) guards. Every
// op writes raw bytes with zero normalization, through the workspace os.Root, and
// only when policy.CheckFile clears the target (the same gate a read uses).
package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

// fileWriteResult is the success payload for file_create and file_overwrite.
type fileWriteResult struct {
	Path         string `json:"path"`
	BytesWritten int    `json:"bytesWritten"`
	SHA256       string `json:"sha256"` // hash of the resulting (or would-be, on dry_run) file
	DryRun       bool   `json:"dryRun,omitempty"`
}

// fileReplaceResult is the success payload for file_replace.
type fileReplaceResult struct {
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
	SHA256       string `json:"sha256"`
	DryRun       bool   `json:"dryRun,omitempty"`
}

// writeGate is the shared front door for every write op: it enforces the opt-in
// flag, then validates and policy-checks the target exactly as a read would. A
// disabled workspace returns READ_ONLY; an out-of-policy or unsafe path returns
// POLICY_DENIED. The returned path is the cleaned workspace-relative form.
func (s *Server) writeGate(p string) (string, *toolError) {
	if !s.ws.Write.Enabled {
		return "", &toolError{Code: "READ_ONLY", Message: "writes are disabled for this workspace", Reason: "write_disabled"}
	}
	clean, err := Clean(p)
	if err != nil {
		return "", mapPathError(err)
	}
	if d := s.ws.Policy.CheckFile(clean); !d.Allowed {
		return "", mapPolicyDenied(d.Reason)
	}
	return clean, nil
}

// hashHex is the hex SHA-256 used everywhere a content hash is named (base_sha256,
// the sha256 result field, file_read's sha256, the audit trail).
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hashFileFull streams the hex SHA-256 of a file's full bytes through the
// workspace os.Root, matching what base_sha256 checks (readForHash). It returns
// "" if the file is unreadable, errors mid-read, or exceeds limit (a file past
// the editable limit has no comparable base hash — file_replace would reject it
// with FILE_TOO_LARGE anyway). limit mirrors the workspace read limit.
func hashFileFull(root *Root, rel string, limit int64) string {
	f, err := root.Open(rel)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil || n > limit {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readForHash reads a file's full bytes for hashing/matching, bounded by the
// workspace read limit. A file larger than the limit is FILE_TOO_LARGE (never a
// partial read that would corrupt a replace). A missing path maps to NOT_FOUND.
func (s *Server) readForHash(clean string) ([]byte, *toolError) {
	f, err := s.ws.Root.Open(clean)
	if err != nil {
		return nil, mapPathError(err)
	}
	defer f.Close()
	limit := s.ws.Read.MaxBytes
	buf := make([]byte, limit+1)
	n, err := io.ReadFull(f, buf)
	switch err {
	case nil, io.EOF, io.ErrUnexpectedEOF:
	default:
		return nil, mapPathError(err)
	}
	if int64(n) > limit {
		return nil, &toolError{Code: "FILE_TOO_LARGE", Message: "file exceeds the workspace read limit", Reason: "file_too_large"}
	}
	return buf[:n], nil
}

// checkBaseSHA verifies an optional caller-supplied base_sha256 against the file's
// current bytes — the optimistic-concurrency guard against a read-then-write race
// (the tree syncs from GitHub out of band). Empty base means "no guard".
func checkBaseSHA(base string, current []byte) *toolError {
	if base == "" {
		return nil
	}
	cur := hashHex(current)
	if !strings.EqualFold(strings.TrimSpace(base), cur) {
		return &toolError{Code: "BASE_SHA_MISMATCH", Message: "file changed since read; current sha256 is " + cur, Reason: "base_sha_mismatch"}
	}
	return nil
}

// --- file_create ---

type fileCreateArgs struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

func (s *Server) fileCreate(args json.RawMessage) (any, ToolEvent, error) {
	var a fileCreateArgs
	ev := ToolEvent{}
	if err := unmarshalArgs(args, &a); err != nil {
		return nil, ev, err
	}
	clean, te := s.writeGate(a.Path)
	if te != nil {
		return nil, ev, te
	}
	ev.Paths = []string{clean}

	f, err := s.ws.Root.CreateNew(clean)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, ev, &toolError{Code: "PATH_EXISTS", Message: "file already exists; use file_overwrite", Reason: "path_exists"}
		}
		return nil, ev, mapPathError(err)
	}
	defer f.Close()
	n, err := f.Write([]byte(a.Contents))
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	hash := hashHex([]byte(a.Contents))
	ev.Bytes, ev.Hash = n, hash
	return fileWriteResult{Path: clean, BytesWritten: n, SHA256: hash}, ev, nil
}

// --- file_overwrite ---

type fileOverwriteArgs struct {
	Path       string `json:"path"`
	Contents   string `json:"contents"`
	BaseSHA256 string `json:"base_sha256"`
	DryRun     bool   `json:"dry_run"`
}

func (s *Server) fileOverwrite(args json.RawMessage) (any, ToolEvent, error) {
	var a fileOverwriteArgs
	ev := ToolEvent{}
	if err := unmarshalArgs(args, &a); err != nil {
		return nil, ev, err
	}
	clean, te := s.writeGate(a.Path)
	if te != nil {
		return nil, ev, te
	}
	ev.Paths = []string{clean}

	// Must already exist (no O_CREATE): a typo'd path is NOT_FOUND, not a new file.
	info, err := s.ws.Root.Stat(clean)
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	if info.IsDir() {
		return nil, ev, newToolError("NOT_FOUND", "path is a directory")
	}
	// A base_sha256 guard (or a dry_run preview) needs the current bytes.
	if a.BaseSHA256 != "" || a.DryRun {
		cur, te := s.readForHash(clean)
		if te != nil {
			return nil, ev, te
		}
		if te := checkBaseSHA(a.BaseSHA256, cur); te != nil {
			return nil, ev, te
		}
	}

	hash := hashHex([]byte(a.Contents))
	if a.DryRun {
		ev.Hash = hash
		return fileWriteResult{Path: clean, BytesWritten: len(a.Contents), SHA256: hash, DryRun: true}, ev, nil
	}

	f, err := s.ws.Root.WriteExisting(clean)
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	defer f.Close()
	n, err := f.Write([]byte(a.Contents))
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	ev.Bytes, ev.Hash = n, hash
	return fileWriteResult{Path: clean, BytesWritten: n, SHA256: hash}, ev, nil
}

// --- file_replace ---

type fileReplaceArgs struct {
	Path                 string `json:"path"`
	OldStr               string `json:"old_str"`
	NewStr               string `json:"new_str"`
	ExpectedReplacements *int   `json:"expected_replacements"`
	BaseSHA256           string `json:"base_sha256"`
	DryRun               bool   `json:"dry_run"`
}

func (s *Server) fileReplace(args json.RawMessage) (any, ToolEvent, error) {
	var a fileReplaceArgs
	ev := ToolEvent{}
	if err := unmarshalArgs(args, &a); err != nil {
		return nil, ev, err
	}
	if a.OldStr == "" {
		return nil, ev, newToolError("INVALID_ARGS", "old_str must be non-empty")
	}
	expected := 1
	if a.ExpectedReplacements != nil {
		expected = *a.ExpectedReplacements
	}
	if expected < 1 {
		return nil, ev, newToolError("INVALID_ARGS", "expected_replacements must be >= 1")
	}
	clean, te := s.writeGate(a.Path)
	if te != nil {
		return nil, ev, te
	}
	ev.Paths = []string{clean}

	cur, te := s.readForHash(clean) // NOT_FOUND if absent, FILE_TOO_LARGE past the cap
	if te != nil {
		return nil, ev, te
	}
	if te := checkBaseSHA(a.BaseSHA256, cur); te != nil {
		return nil, ev, te
	}

	old := []byte(a.OldStr)
	count := bytes.Count(cur, old)
	if count != expected {
		return nil, ev, &toolError{
			Code:    "MATCH_COUNT_MISMATCH",
			Message: "found " + strconv.Itoa(count) + " occurrence(s) of old_str, expected " + strconv.Itoa(expected) + "; lengthen the anchor or set expected_replacements",
			Reason:  "match_count_mismatch",
		}
	}
	next := bytes.ReplaceAll(cur, old, []byte(a.NewStr))
	hash := hashHex(next)
	if a.DryRun {
		ev.Hash = hash
		return fileReplaceResult{Path: clean, Replacements: count, SHA256: hash, DryRun: true}, ev, nil
	}

	f, err := s.ws.Root.WriteExisting(clean)
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	defer f.Close()
	n, err := f.Write(next)
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	ev.Bytes, ev.Hash = n, hash
	return fileReplaceResult{Path: clean, Replacements: count, SHA256: hash}, ev, nil
}
