package lean

import (
	"regexp"
	"strings"
)

// Pos is a position reported by Lean. Line is 1-based, Column is a 0-based
// codepoint offset, both relative to the command text sent to the REPL.
type Pos struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Message is one Lean diagnostic as returned by the REPL.
type Message struct {
	Severity string `json:"severity"` // "error" | "warning" | "info"
	Pos      Pos    `json:"pos"`
	EndPos   *Pos   `json:"endPos,omitempty"`
	Data     string `json:"data"`
}

// Sorry is a `sorry` hole the REPL found, with the goal at that point.
// ProofState is only valid inside the REPL process that produced it.
type Sorry struct {
	Pos        Pos    `json:"pos"`
	EndPos     *Pos   `json:"endPos,omitempty"`
	Goal       string `json:"goal"`
	ProofState int    `json:"proofState"`
}

// TacticInfo is one tactic invocation with the goals *before* it ran
// (returned when a command is run with allTactics).
type TacticInfo struct {
	Tactic     string `json:"tactic"`
	Pos        Pos    `json:"pos"`
	EndPos     *Pos   `json:"endPos,omitempty"`
	Goals      string `json:"goals"`
	ProofState int    `json:"proofState"`
}

// ErrorKind is a coarse classification of a diagnostic, used to choose a
// repair strategy and to give the LLM targeted hints.
type ErrorKind string

const (
	KindParse         ErrorKind = "parse"
	KindUnknownIdent  ErrorKind = "unknown_identifier"
	KindUnsolvedGoals ErrorKind = "unsolved_goals"
	KindTacticFailed  ErrorKind = "tactic_failed"
	KindTypeMismatch  ErrorKind = "type_mismatch"
	KindTimeout       ErrorKind = "timeout"
	KindSorry         ErrorKind = "sorry"
	KindAxiom         ErrorKind = "forbidden_axiom"
	KindForbidden     ErrorKind = "forbidden_syntax"
	KindNoAxiomCheck  ErrorKind = "axiom_check_missing"
	KindOther         ErrorKind = "other"
)

var (
	reTimeout = regexp.MustCompile(`(?i)\(deterministic\) timeout|maximum recursion depth|heartbeats`)
	reUnknown = regexp.MustCompile(`(?i)^unknown (identifier|constant|declaration|namespace)|^unknown tactic`)
	reTactic  = regexp.MustCompile(`(?i)\bfailed\b|could not prove|made no progress|did not close|no goals to be proved|not supported by`)
	reSorry   = regexp.MustCompile("(?i)declaration uses ['`]?sorry")
)

// Classify assigns an ErrorKind to a diagnostic. Order matters: more
// specific patterns are tested first.
func Classify(m Message) ErrorKind {
	d := strings.TrimSpace(m.Data)
	low := strings.ToLower(d)
	switch {
	case strings.HasPrefix(d, axiomsBadTag):
		return KindAxiom
	case reSorry.MatchString(d):
		return KindSorry
	case reTimeout.MatchString(d):
		return KindTimeout
	case strings.HasPrefix(low, "unsolved goals"):
		return KindUnsolvedGoals
	case reUnknown.MatchString(d):
		return KindUnknownIdent
	case strings.Contains(low, "type mismatch"):
		return KindTypeMismatch
	case strings.HasPrefix(low, "unexpected") || strings.Contains(low, "expected token"):
		return KindParse
	case reTactic.MatchString(d):
		return KindTacticFailed
	}
	return KindOther
}

// Diagnostic is a Message plus its classification.
type Diagnostic struct {
	Message
	Kind ErrorKind `json:"kind"`
}

func diagnose(m Message) Diagnostic { return Diagnostic{Message: m, Kind: Classify(m)} }
