package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"mathprover/internal/lean"
	"mathprover/internal/llm"
)

// fakeLLM routes requests by system prompt to per-task reply scripts.
type fakeLLM struct {
	mu      sync.Mutex
	split   string
	formal  []string // consumed in order
	proofs  []string
	reqs    []llm.Request
	costPer float64
	failAll error
}

func (f *fakeLLM) Chat(_ context.Context, r llm.Request) (*llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	if f.failAll != nil {
		return nil, f.failAll
	}
	sys := r.Messages[0].Content
	pop := func(q *[]string) string {
		if len(*q) == 0 {
			return "```lean\nnothing_left\n```"
		}
		s := (*q)[0]
		*q = (*q)[1:]
		return s
	}
	var c string
	switch {
	case strings.Contains(sys, "split a user's"):
		c = f.split
	case strings.Contains(sys, "translate natural-language"):
		c = pop(&f.formal)
	case strings.Contains(sys, "say in plain mathematical English"):
		c = "back-translated"
	case strings.Contains(sys, "write tactic proofs"):
		c = pop(&f.proofs)
	case strings.Contains(sys, "explain a formally verified"):
		c = "explanation"
	default:
		return nil, fmt.Errorf("unrouted request: %.60s", sys)
	}
	return &llm.Response{Content: c, CostUSD: f.costPer}, nil
}

func formal(name, sig string) string {
	return fmt.Sprintf("```lean\ntheorem %s %s := by sorry\n```\nNone.", name, sig)
}

func proof(body string) string { return "```lean\n" + body + "\n```" }

// fakeLean: signatures containing BAD fail to elaborate; proofs equal to
// "good" (or "use_<name>" when <name> is in the base snapshot) verify.
type fakeLean struct {
	mu     sync.Mutex
	proofs []string
	bases  []string // base Code() seen per CheckProof
}

func (f *fakeLean) CheckStatement(_ context.Context, base *lean.Snapshot, s lean.Statement) (*lean.StatementCheck, error) {
	code := lean.StatementCode(s)
	if strings.Contains(s.Signature, "BAD") {
		return &lean.StatementCheck{Code: code, Errors: []lean.Diagnostic{{Message: lean.Message{Severity: "error", Pos: lean.Pos{Line: 1}, Data: "unknown identifier 'BAD'"}, Kind: lean.KindUnknownIdent}}}, nil
	}
	return &lean.StatementCheck{OK: true, Code: code, Snapshot: base.Extend(code)}, nil
}

func (f *fakeLean) CheckProof(_ context.Context, base *lean.Snapshot, s lean.Statement, body string) (*lean.Outcome, error) {
	f.mu.Lock()
	f.proofs = append(f.proofs, body)
	f.bases = append(f.bases, base.Code())
	f.mu.Unlock()
	a := lean.NewAttempt(s, body)
	ok := a.Body == "good"
	if dep, found := strings.CutPrefix(a.Body, "use_"); found {
		ok = strings.Contains(base.Code(), "theorem "+dep+" ")
	}
	res := &lean.Result{Snapshot: base.Extend(a.Code)}
	if ok {
		res.Messages = []lean.Message{{Severity: "info", Pos: lean.Pos{Line: a.AxiomsLine}, Data: "HARNESS_AXIOMS_OK " + s.Name + ": []"}}
	} else {
		res.Messages = []lean.Message{{Severity: "error", Pos: lean.Pos{Line: a.BodyLine}, Data: "linarith failed"}}
	}
	return &lean.Outcome{Attempt: a, Result: res, Verdict: lean.Judge(a, res)}, nil
}

func run(t *testing.T, fl *fakeLLM, fk *fakeLean, opts Options, hooks Hooks, input string) (*Result, error, []Event) {
	t.Helper()
	var events []Event
	user := hooks.OnEvent
	hooks.OnEvent = func(e Event) {
		events = append(events, e)
		if user != nil {
			user(e)
		}
	}
	p := New(llm.NewAgent(fl, llm.Options{}), fk, legacy(opts))
	res, err := p.Run(context.Background(), input, hooks)
	return res, err, events
}

// legacy makes older tests deterministic: one stage, 6 attempts, no
// automation, unless the test configures those itself.
func legacy(o Options) Options {
	if o.Stages == nil {
		o.Stages = []Stage{{Name: "test", Sessions: 1, Attempts: 6}}
	}
	if o.Automation == nil {
		o.NoAutomation = true
	}
	return o
}

func TestHappyPathWithChaining(t *testing.T) {
	fl := &fakeLLM{
		split:  `{"statements": ["first", "second"]}`,
		formal: []string{formal("stmt_1", ": 1 = 1"), formal("stmt_2", ": 2 = 2")},
		proofs: []string{proof("wrong"), proof("good"), proof("use_stmt_1")},
	}
	fk := &fakeLean{}
	res, err, events := run(t, fl, fk, Options{}, Hooks{}, "first. second.")
	if err != nil {
		t.Fatal(err)
	}
	s1, s2 := res.Statements[0], res.Statements[1]
	if s1.Status != StatusProved || s1.Proof != "good" || len(s1.Attempts) != 2 || s1.Explanation != "explanation" {
		t.Errorf("stmt_1 = %+v", s1)
	}
	if s2.Status != StatusProved || s2.Proof != "use_stmt_1" {
		t.Errorf("stmt_2 should prove using stmt_1 in its base: %+v", s2)
	}
	// second feedback round must contain the Lean error for the first attempt
	if !strings.Contains(s1.Attempts[0].Feedback, "linarith failed") {
		t.Errorf("feedback = %q", s1.Attempts[0].Feedback)
	}
	if !strings.Contains(res.LeanFile, "import Mathlib") || !strings.Contains(res.LeanFile, "theorem stmt_1 : 1 = 1 := by\n  good") ||
		!strings.Contains(res.LeanFile, "theorem stmt_2") {
		t.Errorf("lean file:\n%s", res.LeanFile)
	}
	if last := events[len(events)-1]; last.Type != EvDone || last.Message != "2/2 proved" {
		t.Errorf("last event = %+v", last)
	}
	// the prover for stmt_2 was told stmt_1 is available
	var stmt2Intro string
	for _, r := range fl.reqs {
		if strings.Contains(r.Messages[0].Content, "write tactic proofs") && strings.Contains(r.Messages[1].Content, "stmt_2") {
			stmt2Intro = r.Messages[1].Content
		}
	}
	if !strings.Contains(stmt2Intro, "theorem stmt_1 : 1 = 1") {
		t.Errorf("stmt_2 prompt lacks stmt_1:\n%s", stmt2Intro)
	}
}

func TestFormalizationRepairAndGiveUp(t *testing.T) {
	fl := &fakeLLM{
		split:  `{"statements": ["a", "b"]}`,
		formal: []string{formal("stmt_1", ": BAD"), formal("stmt_1", ": 1 = 1"), formal("stmt_2", ": BAD"), formal("stmt_2", ": BAD"), formal("stmt_2", ": BAD")},
		proofs: []string{proof("good")},
	}
	res, err, _ := run(t, fl, &fakeLean{}, Options{NoExplain: true}, Hooks{}, "a b")
	if err != nil {
		t.Fatal(err)
	}
	if res.Statements[0].Status != StatusProved {
		t.Errorf("stmt_1 should be repaired and proved: %+v", res.Statements[0])
	}
	if s := res.Statements[1]; s.Status != StatusUnformal || !strings.Contains(s.Error, "unknown identifier") {
		t.Errorf("stmt_2 = %+v", s)
	}
	// the repair request carried the Lean error
	found := false
	for _, r := range fl.reqs {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, "rejected") && strings.Contains(m.Content, "unknown identifier 'BAD'") {
				found = true
			}
		}
	}
	if !found {
		t.Error("formalization retry did not include the Lean error")
	}
}

func TestProofGivesUpAndDetectsRepeats(t *testing.T) {
	fl := &fakeLLM{
		formal: []string{formal("stmt_1", ": 1 = 1")},
		proofs: []string{proof("bad1"), proof("bad1"), proof("bad2")},
	}
	fk := &fakeLean{}
	res, err, _ := run(t, fl, fk, Options{SkipSplit: true, MaxProofAttempts: 3}, Hooks{}, "x")
	if err != nil {
		t.Fatal(err)
	}
	s := res.Statements[0]
	if s.Status != StatusFailed || len(s.Attempts) != 3 {
		t.Fatalf("stmt = %+v", s)
	}
	if s.Attempts[1].ErrorKinds[0] != "repeated" || len(fk.proofs) != 2 {
		t.Errorf("repeat should skip Lean: kinds=%v lean calls=%d", s.Attempts[1].ErrorKinds, len(fk.proofs))
	}
	if !strings.Contains(res.LeanFile, "/- Not proved (failed):\ntheorem stmt_1 : 1 = 1") {
		t.Errorf("lean file:\n%s", res.LeanFile)
	}
}

func TestParallelSessionsFirstWins(t *testing.T) {
	fl := &fakeLLM{
		formal: []string{formal("stmt_1", ": 1 = 1")},
		proofs: []string{proof("good"), proof("bad"), proof("bad"), proof("bad")},
	}
	res, err, _ := run(t, fl, &fakeLean{}, Options{SkipSplit: true, ParallelSessions: 3, NoExplain: true}, Hooks{}, "x")
	if err != nil || res.Statements[0].Status != StatusProved {
		t.Fatalf("err=%v stmt=%+v", err, res.Statements[0])
	}
}

func TestConfirmRevision(t *testing.T) {
	fl := &fakeLLM{
		formal: []string{formal("stmt_1", "(x : ℕ) : x ≥ 0"), formal("stmt_1", "(x : ℝ) : x ^ 2 ≥ 0")},
		proofs: []string{proof("good")},
	}
	var proposals []Proposal
	confirm := func(_ context.Context, p Proposal) (Decision, error) {
		proposals = append(proposals, p)
		if len(proposals) == 1 {
			return Decision{Correction: "x is real and it is x squared"}, nil
		}
		return Decision{Accept: true}, nil
	}
	res, err, _ := run(t, fl, &fakeLean{}, Options{SkipSplit: true, NoExplain: true}, Hooks{Confirm: confirm}, "x^2 >= 0")
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 2 || proposals[0].BackTranslation != "back-translated" || res.Statements[0].Signature != "(x : ℝ) : x ^ 2 ≥ 0" {
		t.Fatalf("proposals=%+v stmt=%+v", proposals, res.Statements[0])
	}
	// the correction reached the formalizer
	found := false
	for _, r := range fl.reqs {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, "x is real and it is x squared") {
				found = true
			}
		}
	}
	if !found {
		t.Error("user correction not sent to formalizer")
	}

	// rejecting without a correction skips the statement
	fl2 := &fakeLLM{formal: []string{formal("stmt_1", ": 1 = 1")}}
	res2, _, _ := run(t, fl2, &fakeLean{}, Options{SkipSplit: true}, Hooks{Confirm: func(context.Context, Proposal) (Decision, error) {
		return Decision{}, nil
	}}, "x")
	if res2.Statements[0].Status != StatusUnformal {
		t.Errorf("status = %s", res2.Statements[0].Status)
	}
}

func TestFatalErrorsStopJob(t *testing.T) {
	fl := &fakeLLM{split: `{"statements": ["a", "b"]}`}
	fl.failAll = &llm.APIError{Status: 402, Message: "Insufficient Balance"}
	res, err, _ := run(t, fl, &fakeLean{}, Options{}, Hooks{}, "a b")
	var ae *llm.APIError
	if !errors.As(err, &ae) || ae.Status != 402 {
		t.Fatalf("err = %v", err)
	}
	_ = res

	// budget exhausted mid-job: later statements are canceled
	fl2 := &fakeLLM{
		split:   `{"statements": ["a", "b"]}`,
		formal:  []string{formal("stmt_1", ": 1 = 1"), formal("stmt_2", ": 2 = 2")},
		proofs:  []string{proof("bad"), proof("bad"), proof("bad")},
		costPer: 1,
	}
	p := New(llm.NewAgent(fl2, llm.Options{BudgetUSD: 2.5}), &fakeLean{}, legacy(Options{NoExplain: true}))
	res2, err := p.Run(context.Background(), "a b", Hooks{})
	if !errors.Is(err, llm.ErrBudget) {
		t.Fatalf("err = %v", err)
	}
	if res2.Statements[1].Status != StatusCanceled || res2.CostUSD < 2.5 {
		t.Errorf("stmts=%+v cost=%g", res2.Statements, res2.CostUSD)
	}
}

func TestContextCanceled(t *testing.T) {
	fl := &fakeLLM{formal: []string{formal("stmt_1", ": 1 = 1")}, proofs: []string{proof("good")}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := New(llm.NewAgent(fl, llm.Options{}), &fakeLean{}, legacy(Options{SkipSplit: true}))
	if _, err := p.Run(ctx, "x", Hooks{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

// fakeVerifier rejects the names in reject, or fails entirely with err.
type fakeVerifier struct {
	reject map[string]bool
	err    error
	code   string
	items  []lean.VerifyItem
}

func (f *fakeVerifier) Verify(_ context.Context, code string, items []lean.VerifyItem) (*lean.VerifyReport, error) {
	f.code, f.items = code, items
	if f.err != nil {
		return nil, f.err
	}
	rep := &lean.VerifyReport{KernelOK: true, OK: true}
	for _, it := range items {
		v := lean.VerifiedItem{Name: it.Name, OK: !f.reject[it.Name], Axioms: []string{"propext"}}
		if !v.OK {
			v.Error = "type differs from the confirmed statement"
			rep.OK = false
		}
		rep.Items = append(rep.Items, v)
	}
	return rep, nil
}

func TestIndependentVerification(t *testing.T) {
	script := func() *fakeLLM {
		return &fakeLLM{
			split:  `{"statements": ["a", "b"]}`,
			formal: []string{formal("stmt_1", ": 1 = 1"), formal("stmt_2", ": 2 = 2")},
			proofs: []string{proof("good"), proof("good")},
		}
	}

	// accepted: statuses stay proved, verdicts attached, file passed through
	fv := &fakeVerifier{}
	res, err, events := run(t, script(), &fakeLean{}, Options{NoExplain: true, Verifier: fv}, Hooks{}, "a b")
	if err != nil {
		t.Fatal(err)
	}
	if len(fv.items) != 2 || fv.items[1].Signature != ": 2 = 2" || !strings.HasPrefix(fv.code, "import Mathlib\nimport Harness\n\ntheorem stmt_1") {
		t.Errorf("verifier input: items=%+v code=%q", fv.items, fv.code)
	}
	for _, s := range res.Statements {
		if s.Status != StatusProved || s.Verification == nil || !s.Verification.OK {
			t.Errorf("%s = %+v", s.Name, s)
		}
	}
	var sawVerified bool
	for _, e := range events {
		sawVerified = sawVerified || (e.Type == EvVerified && strings.Contains(e.Message, "2/2"))
	}
	if !sawVerified || res.Verification == nil {
		t.Error("missing verified event or report")
	}

	// one rejected: it becomes failed, the other stays proved
	res, _, events = run(t, script(), &fakeLean{}, Options{NoExplain: true, Verifier: &fakeVerifier{reject: map[string]bool{"stmt_2": true}}}, Hooks{}, "a b")
	if res.Statements[0].Status != StatusProved || res.Statements[1].Status != StatusFailed ||
		!strings.Contains(res.Statements[1].Error, "independent verification rejected") {
		t.Errorf("statements: %+v / %+v", res.Statements[0], res.Statements[1])
	}
	if last := events[len(events)-1]; last.Message != "1/2 proved" {
		t.Errorf("summary = %q", last.Message)
	}

	// verifier cannot run: fail closed as unverified, never proved
	res, err, _ = run(t, script(), &fakeLean{}, Options{NoExplain: true, Verifier: &fakeVerifier{err: errors.New("bwrap missing")}}, Hooks{}, "a b")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Statements {
		if s.Status != StatusUnverified || !strings.Contains(s.Error, "bwrap missing") {
			t.Errorf("%s = %+v", s.Name, s)
		}
	}
}

func TestAutomationFirst(t *testing.T) {
	fl := &fakeLLM{formal: []string{formal("stmt_1", ": 1 = 1")}, proofs: []string{proof("good")}}
	fk := &fakeLean{}
	// "good" is the only body fakeLean accepts, so automation list ["omega", "good"]
	// stands in for a tactic that closes the goal.
	res, err, events := run(t, fl, fk, Options{SkipSplit: true, NoExplain: true, Automation: []string{"omega", "good"}}, Hooks{}, "x")
	if err != nil {
		t.Fatal(err)
	}
	s := res.Statements[0]
	if s.Status != StatusProved || s.Proof != "good" || len(s.Attempts) != 1 || s.Attempts[0].Stage != "auto" {
		t.Fatalf("stmt = %+v", s)
	}
	for _, r := range fl.reqs {
		if strings.Contains(r.Messages[0].Content, "write tactic proofs") {
			t.Fatal("LLM prover called although automation succeeded")
		}
	}
	var saw bool
	for _, e := range events {
		saw = saw || (e.Type == EvAutomation && strings.Contains(e.Message, "good"))
	}
	if !saw {
		t.Error("no automation event")
	}

	// nothing closes it: falls through to the LLM
	fl2 := &fakeLLM{formal: []string{formal("stmt_1", ": 1 = 1")}, proofs: []string{proof("good")}}
	res, _, _ = run(t, fl2, &fakeLean{}, Options{SkipSplit: true, NoExplain: true, Automation: []string{"omega", "simp"}}, Hooks{}, "x")
	if s := res.Statements[0]; s.Status != StatusProved || s.Attempts[len(s.Attempts)-1].Stage != "test" {
		t.Fatalf("stmt = %+v", s)
	}
}

// tryThisLean accepts "exact?" (reporting a suggestion) and the suggested term.
type tryThisLean struct{ fakeLean }

func (f *tryThisLean) CheckProof(ctx context.Context, base *lean.Snapshot, s lean.Statement, body string) (*lean.Outcome, error) {
	a := lean.NewAttempt(s, body)
	res := &lean.Result{Snapshot: base.Extend(a.Code)}
	switch a.Body {
	case "exact?", "exact Nat.le_refl 1":
		res.Messages = []lean.Message{{Severity: "info", Pos: lean.Pos{Line: a.AxiomsLine}, Data: "HARNESS_AXIOMS_OK " + s.Name + ": []"}}
		if a.Body == "exact?" {
			res.Messages = append(res.Messages, lean.Message{Severity: "info", Pos: lean.Pos{Line: a.BodyLine}, Data: "Try this: exact Nat.le_refl 1"})
		}
	default:
		res.Messages = []lean.Message{{Severity: "error", Pos: lean.Pos{Line: a.BodyLine}, Data: "nope"}}
	}
	return &lean.Outcome{Attempt: a, Result: res, Verdict: lean.Judge(a, res)}, nil
}

func TestAutomationKeepsExactSuggestion(t *testing.T) {
	fl := &fakeLLM{formal: []string{formal("stmt_1", ": 1 ≤ 1")}}
	p := New(llm.NewAgent(fl, llm.Options{}), &tryThisLean{}, Options{SkipSplit: true, NoExplain: true, Automation: []string{"omega", "exact?"}})
	res, err := p.Run(context.Background(), "x", Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if s := res.Statements[0]; s.Status != StatusProved || s.Proof != "exact Nat.le_refl 1" {
		t.Fatalf("want the suggested term as proof, got %+v", s)
	}
}

func TestStageLadder(t *testing.T) {
	// Stage 1 (1 session x 2 attempts) fails, stage 2 succeeds on its first try.
	fl := &fakeLLM{
		formal: []string{formal("stmt_1", ": 1 = 1")},
		proofs: []string{proof("bad1"), proof("bad2"), proof("good")},
	}
	stages := []Stage{
		{Name: "cheap", Spec: llm.ModelSpec{Model: "m", Effort: llm.EffortNone}, Sessions: 1, Attempts: 2},
		{Name: "deep", Spec: llm.ModelSpec{Model: "m", Effort: llm.EffortHigh}, Sessions: 1, Attempts: 2},
	}
	res, err, events := run(t, fl, &fakeLean{}, Options{SkipSplit: true, NoExplain: true, Stages: stages}, Hooks{}, "x")
	if err != nil {
		t.Fatal(err)
	}
	s := res.Statements[0]
	if s.Status != StatusProved || len(s.Attempts) != 3 {
		t.Fatalf("stmt = %+v", s)
	}
	if a := s.Attempts[2]; a.Stage != "deep" || a.Effort != "high" || a.Session != 2 || a.N != 1 {
		t.Errorf("winning attempt = %+v", a)
	}
	// each stage's requests used that stage's effort
	var efforts []llm.Effort
	for _, r := range fl.reqs {
		if strings.Contains(r.Messages[0].Content, "write tactic proofs") {
			efforts = append(efforts, r.Effort)
		}
	}
	if len(efforts) != 3 || efforts[0] != llm.EffortNone || efforts[2] != llm.EffortHigh {
		t.Errorf("efforts = %v", efforts)
	}
	var stageEvents int
	for _, e := range events {
		if e.Type == EvStage {
			stageEvents++
		}
	}
	if stageEvents != 2 {
		t.Errorf("stage events = %d", stageEvents)
	}
}

func TestRepeatsSharedAcrossSessions(t *testing.T) {
	// Two stages; the second stage repeats a body that failed in the first.
	fl := &fakeLLM{
		formal: []string{formal("stmt_1", ": 1 = 1")},
		proofs: []string{proof("bad"), proof("bad"), proof("good")},
	}
	fk := &fakeLean{}
	stages := []Stage{{Name: "a", Sessions: 1, Attempts: 1}, {Name: "b", Sessions: 1, Attempts: 2}}
	res, _, _ := run(t, fl, fk, Options{SkipSplit: true, NoExplain: true, Stages: stages}, Hooks{}, "x")
	s := res.Statements[0]
	if s.Status != StatusProved || s.Attempts[1].ErrorKinds[0] != "repeated" || !strings.Contains(s.Attempts[1].Feedback, "linarith failed") {
		t.Fatalf("stmt = %+v", s)
	}
	if len(fk.proofs) != 2 { // "bad" once, "good" once
		t.Errorf("lean checks = %d", len(fk.proofs))
	}
}
