package lean

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Snapshot names a Lean environment by the chain of commands that produced
// it on top of the base imports. A nil *Snapshot is the base environment.
//
// Snapshots are immutable and process-independent: REPL env ids are only
// valid inside one process, so each REPL keeps a cache from snapshot key to
// its own env id and rebuilds missing links by replaying their code. A job
// can therefore use any pool worker and still never re-elaborate a command a
// worker has already run.
type Snapshot struct {
	parent *Snapshot
	code   string
	key    string
	depth  int
}

// Extend returns the snapshot obtained by running code on top of s.
func (s *Snapshot) Extend(code string) *Snapshot {
	h := sha256.New()
	h.Write([]byte(s.Key()))
	h.Write([]byte{0})
	h.Write([]byte(code))
	return &Snapshot{parent: s, code: code, key: hex.EncodeToString(h.Sum(nil)), depth: s.Depth() + 1}
}

// Key is a content hash identifying the snapshot ("" for the base).
func (s *Snapshot) Key() string {
	if s == nil {
		return ""
	}
	return s.key
}

// Depth is the number of commands on top of the base imports.
func (s *Snapshot) Depth() int {
	if s == nil {
		return 0
	}
	return s.depth
}

// Parent returns the snapshot this one extends (nil for depth 1 or base).
func (s *Snapshot) Parent() *Snapshot {
	if s == nil {
		return nil
	}
	return s.parent
}

// Code returns all commands from the base to s, in order, joined by blank
// lines. Prepend the imports to get a standalone Lean file.
func (s *Snapshot) Code() string {
	var parts []string
	for c := s; c != nil; c = c.parent {
		parts = append(parts, c.code)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "\n\n")
}
