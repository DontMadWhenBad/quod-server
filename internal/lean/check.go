package lean

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	axiomsOKTag  = "HARNESS_AXIOMS_OK"
	axiomsBadTag = "HARNESS_AXIOMS_BAD"
	bodyIndent   = "  "
)

var reIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_']*(\.[A-Za-z_][A-Za-z0-9_']*)*$`)

// Statement is a formalized theorem. The server owns Name; Signature comes
// from the formalization step and is what the user confirms.
type Statement struct {
	Name      string // Lean identifier, e.g. "stmt_1"
	Signature string // binders and type, e.g. "(a b : ℕ) : a + b = b + a"
}

// Validate checks the name and scans the signature for forbidden syntax.
func (s Statement) Validate() error {
	if !reIdent.MatchString(s.Name) {
		return fmt.Errorf("lean: invalid theorem name %q", s.Name)
	}
	if strings.TrimSpace(s.Signature) == "" {
		return errors.New("lean: empty signature")
	}
	if err := CheckForbidden(s.Signature); err != nil {
		return fmt.Errorf("lean: signature: %w", err)
	}
	return nil
}

func (s Statement) header() string {
	return "theorem " + s.Name + " " + strings.TrimSpace(s.Signature) + " := by\n"
}

// StatementCode is the Lean code used to check that a statement elaborates.
func StatementCode(s Statement) string {
	return s.header() + bodyIndent + "sorry\n"
}

// Attempt is one proof attempt laid out as Lean code. The model writes only
// Body; the statement and the axiom check are added by the server, so the
// model cannot change what is being proved.
type Attempt struct {
	Statement  Statement
	Body       string // normalized tactic script, unindented
	Code       string // full command text sent to the REPL
	BodyLine   int    // 1-based line in Code of Body's first line
	BodyLines  int    // number of lines in Body
	AxiomsLine int    // 1-based line in Code of #harness_axioms
}

// NewAttempt lays out a proof body under the statement.
func NewAttempt(s Statement, body string) *Attempt {
	body = NormalizeBody(body)
	lines := strings.Split(body, "\n")
	var b strings.Builder
	header := s.header()
	b.WriteString(header)
	for _, l := range lines {
		if l != "" {
			b.WriteString(bodyIndent)
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString("\n#harness_axioms " + s.Name + "\n")
	bodyLine := strings.Count(header, "\n") + 1
	return &Attempt{
		Statement:  s,
		Body:       body,
		Code:       b.String(),
		BodyLine:   bodyLine,
		BodyLines:  len(lines),
		AxiomsLine: bodyLine + len(lines) + 1,
	}
}

// NormalizeBody strips code fences, a leading `by`, CRLFs, blank edge
// lines, trailing whitespace and common indentation (computed over all
// lines, including the first) from a model-written tactic script.
func NormalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\t", "  ")
	if t := strings.TrimSpace(body); strings.HasPrefix(t, "```") {
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			body = strings.TrimSuffix(strings.TrimRight(t[i+1:], " \n"), "```")
		} else {
			body = ""
		}
	}
	lines := strings.Split(body, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	// Leading `by`: alone on its line (drop the line) or `by tac` (keep tac
	// at the same column).
	first := strings.TrimLeft(lines[0], " ")
	if first == "by" {
		lines = lines[1:]
		if len(lines) == 0 {
			return ""
		}
	} else if rest, ok := strings.CutPrefix(first, "by "); ok {
		lines[0] = strings.Repeat(" ", len(lines[0])-len(first)) + strings.TrimLeft(rest, " ")
	}
	minIndent := -1
	for _, l := range lines {
		if l == "" {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, " ")); minIndent < 0 || n < minIndent {
			minIndent = n
		}
	}
	for i, l := range lines {
		if len(l) >= minIndent {
			lines[i] = l[minIndent:]
		}
	}
	return strings.Join(lines, "\n")
}

// Verdict is the server's judgement of one attempt.
type Verdict struct {
	Verified bool
	Errors   []Diagnostic // reasons it failed; empty iff Verified
	Warnings []Diagnostic // non-fatal (linter noise etc.)
	Axioms   string       // the HARNESS_AXIOMS_OK line when verified
}

// Judge decides whether an attempt proved its statement. Verified requires:
// no error diagnostics, no sorry warnings, and the HARNESS_AXIOMS_OK line for
// this theorem emitted at the server-appended #harness_axioms line (so a
// `trace` in the proof cannot fake it).
func Judge(a *Attempt, res *Result) Verdict {
	var v Verdict
	okPrefix := axiomsOKTag + " " + a.Statement.Name + ":"
	for _, m := range res.Messages {
		d := diagnose(m)
		switch {
		case m.Severity == "error", d.Kind == KindSorry:
			v.Errors = append(v.Errors, d)
		case m.Severity == "info" && m.Pos.Line == a.AxiomsLine && strings.HasPrefix(m.Data, okPrefix):
			v.Axioms = m.Data
		case m.Severity == "warning":
			v.Warnings = append(v.Warnings, d)
		}
	}
	// A failed proof is still added (via sorryAx) so #harness_axioms also
	// fails; that is noise when the real error is already reported.
	if len(v.Errors) > 1 {
		kept := v.Errors[:0]
		for _, d := range v.Errors {
			if d.Kind != KindAxiom {
				kept = append(kept, d)
			}
		}
		if len(kept) > 0 {
			v.Errors = kept
		}
	}
	if len(v.Errors) == 0 && v.Axioms == "" {
		v.Errors = append(v.Errors, synthetic(a.AxiomsLine, KindNoAxiomCheck,
			"axiom check did not run; the proof must not end the theorem early or add commands"))
	}
	v.Verified = len(v.Errors) == 0
	return v
}

func synthetic(line int, kind ErrorKind, text string) Diagnostic {
	return Diagnostic{Message: Message{Severity: "error", Pos: Pos{Line: line}, Data: text}, Kind: kind}
}

// Outcome is the result of checking one proof attempt.
type Outcome struct {
	Attempt *Attempt
	Result  *Result // nil if the attempt was rejected before reaching Lean
	Verdict Verdict
}

// Snapshot returns the environment containing the proved theorem, for
// building later statements on. Nil unless verified.
func (o *Outcome) Snapshot() *Snapshot {
	if !o.Verdict.Verified || o.Result == nil {
		return nil
	}
	return o.Result.Snapshot
}

// CheckProof lays out body under s, runs it on top of base and judges it.
// Model mistakes (forbidden syntax, Lean errors, timeouts) are reported in
// the Verdict, not as a Go error, so the caller can feed them back. A
// timeout kills the REPL; the caller must Release it to the pool.
func CheckProof(ctx context.Context, r *REPL, base *Snapshot, s Statement, body string) (*Outcome, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	a := NewAttempt(s, body)
	if a.Body == "" {
		return rejected(a, synthetic(a.BodyLine, KindParse, "empty proof")), nil
	}
	if err := CheckForbidden(a.Body); err != nil {
		var fe *ForbiddenError
		line := a.BodyLine
		if errors.As(err, &fe) {
			line = a.BodyLine + fe.Line - 1
		}
		return rejected(a, synthetic(line, KindForbidden, err.Error())), nil
	}
	res, err := r.Exec(ctx, base, a.Code, ExecOptions{AllTactics: true})
	if errors.Is(err, ErrTimeout) {
		return rejected(a, synthetic(a.BodyLine, KindTimeout, err.Error())), nil
	}
	if err != nil {
		return nil, err
	}
	return &Outcome{Attempt: a, Result: res, Verdict: Judge(a, res)}, nil
}

func rejected(a *Attempt, d Diagnostic) *Outcome {
	return &Outcome{Attempt: a, Verdict: Verdict{Errors: []Diagnostic{d}}}
}

// StatementCheck is the result of elaborating a statement with `sorry`.
type StatementCheck struct {
	OK     bool
	Errors []Diagnostic
	Code   string
	// Snapshot holds the statement as a sorried theorem: useful for
	// formalizing later statements that mention it, never for proving.
	Snapshot *Snapshot
}

// CheckStatement verifies that a formalized statement type-checks.
func CheckStatement(ctx context.Context, r *REPL, base *Snapshot, s Statement) (*StatementCheck, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	code := StatementCode(s)
	res, err := r.Exec(ctx, base, code, ExecOptions{})
	if errors.Is(err, ErrTimeout) {
		return &StatementCheck{Code: code, Errors: []Diagnostic{synthetic(1, KindTimeout, err.Error())}}, nil
	}
	if err != nil {
		return nil, err
	}
	sc := &StatementCheck{Code: code, Snapshot: res.Snapshot}
	for _, m := range res.Messages {
		if m.Severity == "error" {
			sc.Errors = append(sc.Errors, diagnose(m))
		}
	}
	sc.OK = len(sc.Errors) == 0
	return sc, nil
}
