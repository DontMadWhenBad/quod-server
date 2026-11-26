package lean

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		data string
		want ErrorKind
	}{
		{"unsolved goals\n⊢ False", KindUnsolvedGoals},
		{"unknown identifier 'Nat.foo_bar'", KindUnknownIdent},
		{"Unknown constant `Real.sqrt_foo`", KindUnknownIdent},
		{"type mismatch\n  rfl\nhas type ...", KindTypeMismatch},
		{"application type mismatch\n  f x", KindTypeMismatch},
		{"unexpected token 'at'; expected term", KindParse},
		{"linarith failed to find a contradiction", KindTacticFailed},
		{"omega could not prove the goal", KindTacticFailed},
		{"simp made no progress", KindTacticFailed},
		{"(deterministic) timeout at `whnf`, maximum number of heartbeats (200000) has been reached", KindTimeout},
		{"declaration uses 'sorry'", KindSorry},
		{"declaration uses `sorry`", KindSorry},
		{"HARNESS_AXIOMS_BAD t: [sorryAx]", KindAxiom},
		{"something else entirely", KindOther},
	}
	for _, c := range cases {
		if got := Classify(Message{Data: c.data}); got != c.want {
			t.Errorf("Classify(%q) = %s, want %s", c.data, got, c.want)
		}
	}
}

func TestCheckForbidden(t *testing.T) {
	bad := map[string]string{
		"sorry":                                  "sorry",
		"intro x\n  exact sorry":                 "sorry",
		"admit":                                  "admit",
		"native_decide":                          "native_decide",
		"rfl\ntheorem other : 1 = 1 := rfl":      "theorem",
		"rfl\n#eval IO.println \"hi\"":           "#eval",
		"run_tac do pure ()":                     "run_tac",
		"set_option maxHeartbeats 0 in simp":     "set_option",
		"rfl\naxiom cheat : False":               "axiom",
		"exact (by_elab pure (Lean.mkConst `x))": "by_elab",
	}
	for body, tok := range bad {
		err := CheckForbidden(body)
		fe, ok := err.(*ForbiddenError)
		if !ok || fe.Token != tok {
			t.Errorf("CheckForbidden(%q) = %v, want token %q", body, err, tok)
		}
	}
	good := []string{
		"intro x\nsimp [Nat.add_comm]",
		"exact Classical.choice ⟨0⟩",
		"infer_instance",                // contains "instance" inside an identifier
		"simp [Function.Injective.def]", // field access .def
		"exact axiom_of_choice h",       // identifier containing axiom
		"-- sorry, this is a comment\nrfl",
		"/- nested /- sorry -/ still comment -/ rfl",
		"have h : (#s : ℕ) ≤ 3 := by omega\nexact h", // Mathlib card notation
		"exact \"sorry\".length_pos",                 // inside string literal
		"decide",
	}
	for _, body := range good {
		if err := CheckForbidden(body); err != nil {
			t.Errorf("CheckForbidden(%q) = %v, want nil", body, err)
		}
	}
	if fe, _ := CheckForbidden("rfl\nrfl\nsorry").(*ForbiddenError); fe == nil || fe.Line != 3 {
		t.Errorf("want line 3, got %+v", fe)
	}
}

func TestNormalizeBody(t *testing.T) {
	cases := map[string]string{
		"  intro x\n  simp  \n":            "intro x\nsimp",
		"by\n  intro x\n  simp":            "intro x\nsimp",
		"by simp":                          "simp",
		"```lean\nintro x\nsimp\n```":      "intro x\nsimp",
		"constructor\n· simp\n· ring":      "constructor\n· simp\n· ring",
		"induction n with\n| zero => simp": "induction n with\n| zero => simp",
		// first line indented like the rest (as extracted from a code block)
		"  rcases h with ⟨k, rfl⟩ | ⟨k, rfl⟩\n  · refine ⟨k, ?_⟩\n    ring": "rcases h with ⟨k, rfl⟩ | ⟨k, rfl⟩\n· refine ⟨k, ?_⟩\n  ring",
		"\n\n  by\n    simp\n\n": "simp",
		"  by simp\n  ring":      "simp\nring",
		"\tsimp":                 "simp",
		"bye":                    "bye",
		"":                       "",
	}
	for in, want := range cases {
		if got := NormalizeBody(in); got != want {
			t.Errorf("NormalizeBody(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewAttemptLayout(t *testing.T) {
	s := Statement{Name: "stmt_1", Signature: "(a b : ℕ) : a + b = b + a"}
	a := NewAttempt(s, "intro\nomega")
	want := "theorem stmt_1 (a b : ℕ) : a + b = b + a := by\n  intro\n  omega\n\n#harness_axioms stmt_1\n"
	if a.Code != want {
		t.Fatalf("code:\n%s\nwant:\n%s", a.Code, want)
	}
	lines := strings.Split(a.Code, "\n")
	if lines[a.BodyLine-1] != "  intro" || a.BodyLines != 2 {
		t.Errorf("BodyLine %d points at %q", a.BodyLine, lines[a.BodyLine-1])
	}
	if lines[a.AxiomsLine-1] != "#harness_axioms stmt_1" {
		t.Errorf("AxiomsLine %d points at %q", a.AxiomsLine, lines[a.AxiomsLine-1])
	}

	// multi-line signature shifts the body
	s2 := Statement{Name: "t", Signature: "(x : ℝ)\n    (h : 0 < x) : 0 < x"}
	a2 := NewAttempt(s2, "exact h")
	if l := strings.Split(a2.Code, "\n"); a2.BodyLine != 3 || l[a2.AxiomsLine-1] != "#harness_axioms t" {
		t.Errorf("multi-line signature: BodyLine=%d AxiomsLine=%d\n%s", a2.BodyLine, a2.AxiomsLine, a2.Code)
	}
}

func TestStatementValidate(t *testing.T) {
	if err := (Statement{Name: "stmt_1", Signature: ": 1 = 1"}).Validate(); err != nil {
		t.Error(err)
	}
	for _, s := range []Statement{
		{Name: "1bad", Signature: ": True"},
		{Name: "a b", Signature: ": True"},
		{Name: "ok", Signature: "  "},
		{Name: "ok", Signature: ": True := by sorry\naxiom x : False\ntheorem y"},
	} {
		if s.Validate() == nil {
			t.Errorf("Validate(%+v) = nil, want error", s)
		}
	}
}

func TestJudge(t *testing.T) {
	s := Statement{Name: "t", Signature: ": 1 = 1"}
	a := NewAttempt(s, "rfl") // body line 2, axioms line 4
	ok := Message{Severity: "info", Pos: Pos{Line: a.AxiomsLine}, Data: "HARNESS_AXIOMS_OK t: []"}

	if v := Judge(a, &Result{Messages: []Message{ok}}); !v.Verified {
		t.Errorf("clean proof not verified: %+v", v)
	}
	// info at the wrong line (e.g. produced by `trace` inside the proof)
	fake := ok
	fake.Pos.Line = a.BodyLine
	if v := Judge(a, &Result{Messages: []Message{fake}}); v.Verified || v.Errors[0].Kind != KindNoAxiomCheck {
		t.Errorf("faked OK accepted: %+v", v)
	}
	// OK for a different theorem name
	other := ok
	other.Data = "HARNESS_AXIOMS_OK t2: []"
	if v := Judge(a, &Result{Messages: []Message{other}}); v.Verified {
		t.Error("OK for other theorem accepted")
	}
	// sorry warning is fatal
	sorryW := Message{Severity: "warning", Pos: Pos{Line: 1}, Data: "declaration uses 'sorry'"}
	if v := Judge(a, &Result{Messages: []Message{sorryW, ok}}); v.Verified {
		t.Error("sorry accepted")
	}
	// unrelated warning is fine
	lint := Message{Severity: "warning", Pos: Pos{Line: 2}, Data: "unused variable `h`"}
	if v := Judge(a, &Result{Messages: []Message{lint, ok}}); !v.Verified || len(v.Warnings) != 1 {
		t.Errorf("lint warning broke verdict: %+v", v)
	}
	// real error + axiom noise -> only the real error is reported
	e := Message{Severity: "error", Pos: Pos{Line: 2}, Data: "linarith failed"}
	bad := Message{Severity: "error", Pos: Pos{Line: a.AxiomsLine}, Data: "HARNESS_AXIOMS_BAD t: [sorryAx]"}
	if v := Judge(a, &Result{Messages: []Message{e, bad}}); len(v.Errors) != 1 || v.Errors[0].Kind != KindTacticFailed {
		t.Errorf("axiom noise not filtered: %+v", v.Errors)
	}
	// axiom error alone is kept
	if v := Judge(a, &Result{Messages: []Message{bad}}); v.Verified || v.Errors[0].Kind != KindAxiom {
		t.Errorf("bad axioms accepted: %+v", v)
	}
}

func TestSnapshot(t *testing.T) {
	var base *Snapshot
	a := base.Extend("theorem a : True := trivial")
	b := a.Extend("theorem b : True := a")
	if a.Key() == "" || a.Key() == b.Key() || b.Depth() != 2 || b.Parent() != a {
		t.Fatal("bad snapshot chain")
	}
	if base.Extend("theorem a : True := trivial").Key() != a.Key() {
		t.Error("keys must be content-addressed")
	}
	if b.Code() != "theorem a : True := trivial\n\ntheorem b : True := a" {
		t.Errorf("Code() = %q", b.Code())
	}
}

func TestRenderFeedback(t *testing.T) {
	s := Statement{Name: "stmt_1", Signature: "(x : ℝ) (h : 0 < x) : 0 < x ^ 2 + x"}
	a := NewAttempt(s, "have h2 : 0 < x ^ 2 := by positivity\nlinarith [foo]")
	// body line 2 = "  have ...", line 3 = "  linarith [foo]"
	res := &Result{
		Messages: []Message{
			{Severity: "error", Pos: Pos{Line: 3, Column: 12}, EndPos: &Pos{Line: 3, Column: 15}, Data: "unknown identifier 'foo'"},
			{Severity: "error", Pos: Pos{Line: 1, Column: 52}, EndPos: &Pos{Line: 3, Column: 16}, Data: "unsolved goals\nx : ℝ\n⊢ 0 < x ^ 2 + x"},
		},
		Tactics: []TacticInfo{
			{Tactic: "linarith [foo]", Pos: Pos{Line: 3, Column: 2}, EndPos: &Pos{Line: 3, Column: 16}, Goals: "x : ℝ\nh2 : 0 < x ^ 2\n⊢ 0 < x ^ 2 + x"},
		},
	}
	o := &Outcome{Attempt: a, Result: res, Verdict: Judge(a, res)}
	fb := RenderFeedback(o, FeedbackOptions{})
	for _, want := range []string{
		"failed with 2 error(s)",
		"error for the proof as a whole (unsolved_goals)",
		" 2 | linarith [foo]",
		"          ^^^ error (unknown_identifier):", // gutter "   | " + 10 spaces
		"goal before `linarith [foo]`:",
		"h2 : 0 < x ^ 2",
	} {
		if !strings.Contains(fb, want) {
			t.Errorf("feedback missing %q:\n%s", want, fb)
		}
	}
	if small := RenderFeedback(o, FeedbackOptions{MaxBytes: 80}); !strings.HasSuffix(small, "(feedback truncated)\n") {
		t.Errorf("not truncated:\n%s", small)
	}
}

func TestCheckForbiddenAllow(t *testing.T) {
	if err := CheckForbiddenAllow("have h : x := by sorry\nexact sorry", "sorry"); err != nil {
		t.Errorf("sorry should be allowed: %v", err)
	}
	for _, body := range []string{
		"sorry theorem foo : False := sorry",
		"sorry axiom a : False",
		"exact sorry\n#eval 1",
		"sorry sorry def x := 1",
	} {
		if err := CheckForbiddenAllow(body, "sorry"); err == nil {
			t.Errorf("%q passed with only sorry allowed", body)
		}
	}
	if fe, _ := CheckForbidden("sorry theorem").(*ForbiddenError); fe == nil || fe.Token != "sorry" {
		t.Errorf("without allow list the first word is reported: %+v", fe)
	}
}
