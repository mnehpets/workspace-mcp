// SPDX-License-Identifier: Apache-2.0
//
// Adapted from github.com/bep/grrep (main.go scan core). The original wrote
// "path:line:text" to stdout; this version emits structured Match values and
// drops the CLI-only invert/quiet modes. Binary detection (NUL probe), CRLF
// handling, and the sync.Pool buffering are preserved. See NOTICE.

package grrep

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"sync"
)

const (
	peekSize      = 8000
	readerBufSize = 1 << 20 // 1 MiB; bufio fallback for files exceeding scanBufSize.
	scanBufSize   = 1 << 20 // 1 MiB; whole-file pool buffer.
)

var readerPool = sync.Pool{
	New: func() any {
		return bufio.NewReaderSize(nil, readerBufSize)
	},
}

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, scanBufSize)
		return &b
	},
}

// Match is a single content match.
type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// trimCR strips a trailing carriage return so CRLF and LF line endings look the
// same to matching and output.
func trimCR(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\r' {
		return line[:n-1]
	}
	return line
}

// ScanFile reads f and returns all matching lines, labelled with displayPath.
// Binary files (a NUL byte in the head) return nil. f must be seekable: files
// larger than the pool buffer are streamed from the start.
func ScanFile(displayPath string, f io.ReadSeeker, m *Matcher) []Match {
	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)
	buf := *bufp

	n, err := io.ReadFull(f, buf)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF):
		// File fit in the buffer (possibly empty).
		return scanWholeBody(displayPath, buf[:n], m)
	case err == nil:
		// Buffer filled exactly; probe for extra bytes.
		var probe [1]byte
		if k, _ := f.Read(probe[:]); k == 0 {
			return scanWholeBody(displayPath, buf, m)
		}
		if _, e := f.Seek(0, io.SeekStart); e != nil {
			return nil
		}
		return scanFileStream(displayPath, f, m)
	default:
		return nil
	}
}

// ScanBytes returns the lines in data matched by m, labelled with displayPath.
// It is the in-memory counterpart to ScanFile: a caller that has already read
// (and size-bounded) a file can feed the same buffer to several matchers and do
// its own frontmatter-fence detection without re-reading. Binary data (a NUL in
// the head) yields nil, exactly as ScanFile does.
func ScanBytes(displayPath string, data []byte, m *Matcher) []Match {
	return scanWholeBody(displayPath, data, m)
}

// scanWholeBody finds matches by sliding bytes.Index over data. Cheap when a
// file has no matches at all — one bytes.Index call returns -1 and we're done.
func scanWholeBody(path string, data []byte, m *Matcher) []Match {
	headLimit := len(data)
	if headLimit > peekSize {
		headLimit = peekSize
	}
	if bytes.IndexByte(data[:headLimit], 0) >= 0 {
		return nil
	}

	// Pure-regex (no extracted literal): one FindAllIndex over the whole body.
	if m.Re != nil && len(m.Literal) == 0 {
		return scanWholeRegex(path, data, m)
	}

	// Literal or literal pre-filter: slide bytes.Index, validate with re if present.
	lit := m.Literal
	var out []Match
	lineNum := 1
	cursor := 0
	for {
		idx := bytes.Index(data[cursor:], lit)
		if idx < 0 {
			break
		}
		matchPos := cursor + idx
		lineNum += bytes.Count(data[cursor:matchPos], []byte{'\n'})
		lineStart := 0
		if i := bytes.LastIndexByte(data[:matchPos], '\n'); i >= 0 {
			lineStart = i + 1
		}
		lineEnd := len(data)
		if i := bytes.IndexByte(data[matchPos:], '\n'); i >= 0 {
			lineEnd = matchPos + i
		}
		line := trimCR(data[lineStart:lineEnd])
		if m.Re == nil || m.Re.Match(line) {
			out = append(out, Match{Path: path, Line: lineNum, Text: string(line)})
		}
		// Advance past this line so we don't re-match on it.
		cursor = lineEnd
		if cursor < len(data) {
			cursor++ // skip the '\n'
			lineNum++
		}
	}
	return out
}

func scanWholeRegex(path string, data []byte, m *Matcher) []Match {
	hits := m.Re.FindAllIndex(data, -1)
	if len(hits) == 0 {
		return nil
	}
	var out []Match
	lineNum := 1
	cursor := 0
	prevLineEnd := -1
	for _, h := range hits {
		matchPos := h[0]
		lineNum += bytes.Count(data[cursor:matchPos], []byte{'\n'})
		lineStart := 0
		if i := bytes.LastIndexByte(data[:matchPos], '\n'); i >= 0 {
			lineStart = i + 1
		}
		lineEnd := len(data)
		if i := bytes.IndexByte(data[matchPos:], '\n'); i >= 0 {
			lineEnd = matchPos + i
		}
		// Multiple regex hits can land on the same line — emit the line once.
		if lineEnd != prevLineEnd {
			line := trimCR(data[lineStart:lineEnd])
			out = append(out, Match{Path: path, Line: lineNum, Text: string(line)})
			prevLineEnd = lineEnd
		}
		cursor = matchPos
	}
	return out
}

// scanFileStream is the bufio fallback for files larger than scanBufSize.
func scanFileStream(path string, f io.Reader, m *Matcher) []Match {
	br := readerPool.Get().(*bufio.Reader)
	defer readerPool.Put(br)
	br.Reset(f)

	head, _ := br.Peek(peekSize)
	if bytes.IndexByte(head, 0) >= 0 {
		return nil
	}

	var out []Match
	lineNum := 0
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			return out
		}
		if len(line) > 0 || err == nil {
			lineNum++
			if n := len(line); n > 0 && line[n-1] == '\n' {
				line = line[:n-1]
			}
			line = trimCR(line)
			if m.Match(line) {
				out = append(out, Match{Path: path, Line: lineNum, Text: string(line)})
			}
		}
		if err != nil {
			break
		}
	}
	return out
}
