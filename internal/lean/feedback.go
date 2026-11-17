package lean

import (
	"fmt"
	"sort"
	"strings"
)

// FeedbackOptions bounds the size of the rendered feedback.
type FeedbackOptions struct {
	MaxBytes       int // total; default 6000
	MaxMessageRune int // per diagnostic text; default 1500
}

// RenderFeedback formats a failed attempt for the LLM: the proof body with
// line numbers, each error placed under the line it points at with a caret
// underline, and the goal state before the failing tactic when known.
// Line numbers refer to the proof body, not the generated file.
func RenderFeedback(o *Outcome, opts FeedbackOptions) string {
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 6000
	}
	if opts.MaxMessageRune == 0 {
		opts.MaxMessageRune = 1500
	}
	a := o.Attempt
	errs := append([]Diagnostic(nil), o.Verdict.Errors...)
	sort.SliceStable(errs, func(i, j int) bool {
		if errs[i].Pos.Line != errs[j].Pos.Line {
			return errs[i].Pos.Line < errs[j].Pos.Line
		}
		return errs[i].Pos.Column < errs[j].Pos.Column
	})

	var tactics []TacticInfo
	if o.Result != nil {
		tactics = o.Result.Tactics
	}

	byLine := map[int][]Diagnostic{} // body line index (0-based) -> diagnostics
	var outside []Diagnostic
	for _, d := range errs {
		idx := d.Pos.Line - a.BodyLine
		if idx >= 0 && idx < a.BodyLines {
			byLine[idx] = append(byLine[idx], d)
		} else {
			outside = append(outside, d)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "The proof of `%s` failed with %d error(s).\n\n", a.Statement.Name, len(errs))

	for _, d := range outside {
		where := "after the proof"
		if d.Pos.Line < a.BodyLine {
			where = "for the proof as a whole"
		}
		fmt.Fprintf(&b, "error %s (%s):\n%s\n\n", where, d.Kind, indent(truncRunes(d.Data, opts.MaxMessageRune), "  "))
	}

	b.WriteString("Proof (line numbers refer to the proof body):\n")
	lines := strings.Split(a.Body, "\n")
	width := len(fmt.Sprint(len(lines)))
	gutter := strings.Repeat(" ", width+2) + "| " // aligns with "%*d | "
	for i, l := range lines {
		fmt.Fprintf(&b, "%*d | %s\n", width+1, i+1, l)
		for _, d := range byLine[i] {
			col := max(d.Pos.Column-len(bodyIndent), 0)
			lr := []rune(l)
			col = min(col, len(lr))
			span := len(lr) - col
			if d.EndPos != nil && d.EndPos.Line == d.Pos.Line {
				span = d.EndPos.Column - d.Pos.Column
			}
			span = max(span, 1)
			fmt.Fprintf(&b, "%s%s%s error (%s):\n", gutter, strings.Repeat(" ", col), strings.Repeat("^", span), d.Kind)
			b.WriteString(indent(truncRunes(d.Data, opts.MaxMessageRune), gutter+"  "))
			b.WriteByte('\n')
			if d.Kind != KindUnsolvedGoals {
				if t, ok := tacticAt(tactics, d.Pos); ok && strings.TrimSpace(t.Goals) != "" {
					b.WriteString(gutter + "  goal before `" + firstLine(t.Tactic) + "`:\n")
					b.WriteString(indent(truncRunes(t.Goals, opts.MaxMessageRune), gutter+"    "))
					b.WriteByte('\n')
				}
			}
		}
	}

	out := b.String()
	if len(out) > opts.MaxBytes {
		cut := opts.MaxBytes
		for cut > 0 && (out[cut]&0xC0) == 0x80 { // don't split a UTF-8 sequence
			cut--
		}
		out = out[:cut] + "\n... (feedback truncated)\n"
	}
	return out
}

// tacticAt returns the innermost tactic whose span contains p.
func tacticAt(ts []TacticInfo, p Pos) (TacticInfo, bool) {
	var best TacticInfo
	found := false
	for _, t := range ts {
		if !posLE(t.Pos, p) || (t.EndPos != nil && !posLE(p, *t.EndPos)) {
			continue
		}
		if !found || posLE(best.Pos, t.Pos) {
			best, found = t, true
		}
	}
	return best, found
}

func posLE(a, b Pos) bool {
	return a.Line < b.Line || (a.Line == b.Line && a.Column <= b.Column)
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " ...(truncated)"
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
