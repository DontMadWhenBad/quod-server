package lean

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Model-written text (proof bodies, formalized signatures) is scanned for
// constructs that could cheat the checker or run code at elaboration time.
//
// This is defense in depth, not a sandbox: the Lean process must still run
// with no network, limited filesystem and resource limits. The authoritative
// soundness check is the axiom check (#harness_axioms) at a known position.

// Words that must not appear as standalone identifiers. Command keywords are
// included because a tactic block can be terminated early and followed by
// new top-level commands.
var forbiddenWords = []string{
	// holes and cheats
	"sorry", "admit", "axiom", "native_decide",
	// meta-programming / code execution
	"run_cmd", "run_tac", "run_elab", "by_elab", "unsafe", "implemented_by", "extern",
	"macro", "macro_rules", "syntax", "elab", "elab_rules", "declare_syntax_cat",
	"notation", "infix", "infixl", "infixr", "prefix", "postfix",
	"initialize", "builtin_initialize",
	// top-level commands
	"import", "set_option", "theorem", "lemma", "def", "instance", "example",
	"abbrev", "opaque", "structure", "inductive", "class", "attribute",
	"mutual", "omit", "include",
}

// `#` commands. Listed explicitly because `#s` is Mathlib notation for card.
var forbiddenHash = []string{
	"eval", "exit", "print", "check", "reduce", "synth", "help", "time",
	"guard_msgs", "harness_axioms", "lint", "find", "where",
}

var (
	// Lean identifiers may contain letters, digits, _ ' ! ? and dots; a word
	// counts only if it is not part of a longer identifier or a field access.
	reForbiddenWord = regexp.MustCompile(`(?:^|[^\w.'!?])(` + strings.Join(forbiddenWords, "|") + `)(?:$|[^\w'!?])`)
	reForbiddenHash = regexp.MustCompile(`#(` + strings.Join(forbiddenHash, "|") + `)\b`)
)

// ForbiddenError reports a forbidden construct in model-written Lean text.
type ForbiddenError struct {
	Token string
	Line  int // 1-based line within the scanned text
}

func (e *ForbiddenError) Error() string {
	return fmt.Sprintf("forbidden construct %q on line %d", e.Token, e.Line)
}

// CheckForbidden scans Lean text (comments and string literals ignored).
func CheckForbidden(text string) error { return CheckForbiddenAllow(text) }

// CheckForbiddenAllow is CheckForbidden with some words permitted (e.g.
// "sorry" when exploring a proof; such a proof can never be verified).
func CheckForbiddenAllow(text string, allow ...string) error {
	clean := stripCommentsAndStrings(text)
	for i, line := range strings.Split(clean, "\n") {
		// Restart each search right after the previous word (not after the
		// boundary character the match consumed), so adjacent words like
		// "sorry theorem" are each seen.
		for at := 0; at < len(line); {
			loc := reForbiddenWord.FindStringSubmatchIndex(line[at:])
			if loc == nil {
				break
			}
			word := line[at+loc[2] : at+loc[3]]
			if !slices.Contains(allow, word) {
				return &ForbiddenError{Token: word, Line: i + 1}
			}
			at += loc[3]
		}
		if m := reForbiddenHash.FindStringSubmatch(line); m != nil {
			return &ForbiddenError{Token: "#" + m[1], Line: i + 1}
		}
	}
	return nil
}

// stripCommentsAndStrings blanks out `-- ...` comments, nested `/- ... -/`
// comments and "..." string literals, keeping newlines so line numbers hold.
func stripCommentsAndStrings(s string) string {
	rs := []rune(s)
	out := make([]rune, 0, len(rs))
	depth := 0 // block comment nesting
	inStr := false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		next := rune(0)
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		switch {
		case depth > 0:
			if c == '/' && next == '-' {
				depth++
				i++
			} else if c == '-' && next == '/' {
				depth--
				i++
			} else if c == '\n' {
				out = append(out, '\n')
			}
			continue
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			} else if c == '\n' {
				out = append(out, '\n')
			}
			continue
		case c == '/' && next == '-':
			depth = 1
			i++
			out = append(out, ' ')
			continue
		case c == '-' && next == '-':
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			if i < len(rs) {
				out = append(out, '\n')
			}
			continue
		case c == '"':
			inStr = true
			out = append(out, ' ')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
