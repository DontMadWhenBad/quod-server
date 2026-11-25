package pipeline

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"mathprover/internal/lean"
	"mathprover/internal/llm"
)

// Options tunes a Pipeline. Zero values get defaults.
type Options struct {
	MaxFormalizeAttempts int // Lean-rejected formalizations before giving up; default 3
	MaxRevisions         int // user corrections per statement; default 3
	// Stages is the escalation ladder for proof search; nil means
	// DefaultStages(the agent's prover model).
	Stages []Stage
	// MaxProofAttempts and ParallelSessions, if > 0, override every
	// stage's Attempts and Sessions.
	MaxProofAttempts int
	ParallelSessions int
	// Automation tactics are tried directly in Lean before any LLM call;
	// nil means DefaultAutomation. NoAutomation skips this step.
	Automation    []string
	NoAutomation  bool
	SkipSplit     bool // treat the whole input as one statement
	NoExplain     bool
	MaxStatements int // reject inputs that split into more; 0 = unlimited
	// LeanHeader starts LeanFile; default "import Mathlib\nimport Harness".
	LeanHeader string
	Feedback   lean.FeedbackOptions
	// Verifier, if set, re-checks every proved statement independently at
	// the end of the job; a statement only stays "proved" if it passes.
	Verifier Verifier
}

func (o Options) withDefaults() Options {
	if o.MaxFormalizeAttempts <= 0 {
		o.MaxFormalizeAttempts = 3
	}
	if o.MaxRevisions <= 0 {
		o.MaxRevisions = 3
	}
	if o.LeanHeader == "" {
		o.LeanHeader = "import Mathlib\nimport Harness"
	}
	return o
}

// Stage is one rung of the proof-search ladder.
type Stage struct {
	Name     string        `json:"name"`
	Spec     llm.ModelSpec `json:"spec"`
	Sessions int           `json:"sessions"` // parallel sessions racing
	Attempts int           `json:"attempts"` // propose→check rounds per session
}

// DefaultStages escalates from many cheap, non-thinking attempts (Lean's
// fast feedback does the checking) to fewer, deliberate ones. Measured on
// easy Mathlib goals, thinking time is dominated by the model second-guessing
// lemma names it cannot look up, not by the mathematics.
func DefaultStages(model string) []Stage {
	if model == "" {
		model = llm.ModelFlash
	}
	return []Stage{
		{Name: "fast", Spec: llm.ModelSpec{Model: model, Effort: llm.EffortNone, MaxTokens: 4096}, Sessions: 4, Attempts: 3},
		{Name: "think", Spec: llm.ModelSpec{Model: model, Effort: llm.EffortLow}, Sessions: 2, Attempts: 3},
		{Name: "deep", Spec: llm.ModelSpec{Model: model, Effort: llm.EffortHigh}, Sessions: 2, Attempts: 4},
	}
}

func (p *Pipeline) stages() []Stage {
	st := p.opts.Stages
	if st == nil {
		st = DefaultStages(p.agent.Models().Prove.Model)
	}
	out := make([]Stage, len(st))
	for i, s := range st {
		if p.opts.ParallelSessions > 0 {
			s.Sessions = p.opts.ParallelSessions
		}
		if p.opts.MaxProofAttempts > 0 {
			s.Attempts = p.opts.MaxProofAttempts
		}
		s.Sessions, s.Attempts = max(s.Sessions, 1), max(s.Attempts, 1)
		if s.Name == "" {
			s.Name = fmt.Sprintf("stage%d", i+1)
		}
		out[i] = s
	}
	return out
}

// ErrTooManyStatements is returned when the input exceeds MaxStatements.
var ErrTooManyStatements = errors.New("too many statements")

// Pipeline is reusable and safe for concurrent jobs.
type Pipeline struct {
	agent *llm.Agent
	check Checker
	opts  Options
}

// New builds a pipeline.
func New(agent *llm.Agent, check Checker, opts Options) *Pipeline {
	return &Pipeline{agent: agent, check: check, opts: opts.withDefaults()}
}

// Job hooks. Both optional.
type Hooks struct {
	OnEvent func(Event) // called synchronously; keep it fast
	Confirm ConfirmFunc // nil = accept every formalization
}

// job is the state of one Run.
type job struct {
	p     *Pipeline
	hooks Hooks
	mu    sync.Mutex // guards statement results and event emission
	res   *Result
}

// Run processes the input. It returns a Result even on error (with what
// was done so far); err is set for fatal problems such as an exhausted
// budget, cancellation or a broken Lean worker pool.
func (p *Pipeline) Run(ctx context.Context, input string, hooks Hooks) (*Result, error) {
	start := time.Now()
	usd0, calls0 := p.agent.Spent()
	j := &job{p: p, hooks: hooks, res: &Result{}}
	defer func() {
		usd, calls := p.agent.Spent()
		j.res.CostUSD, j.res.LLMCalls = usd-usd0, calls-calls0
		j.res.Duration = time.Since(start)
	}()

	texts, err := j.split(ctx, input)
	if err != nil {
		j.emit(Event{Type: EvError, Message: err.Error()})
		return j.res, err
	}
	if limit := p.opts.MaxStatements; limit > 0 && len(texts) > limit {
		err := fmt.Errorf("%w: input has %d statements, limit is %d", ErrTooManyStatements, len(texts), limit)
		j.emit(Event{Type: EvError, Message: err.Error()})
		return j.res, err
	}
	for i, t := range texts {
		j.res.Statements = append(j.res.Statements, &StatementResult{
			Index: i + 1, Name: fmt.Sprintf("stmt_%d", i+1), Text: t, Status: StatusPending,
		})
	}
	j.emit(Event{Type: EvSplit, Message: fmt.Sprintf("%d statement(s)", len(texts))})

	var base *lean.Snapshot // environment with every proved statement so far
	var lemmas []llm.Lemma  // earlier statements, for the model's context
	var fatal error
	for _, sr := range j.res.Statements {
		if fatal != nil {
			j.setStatus(sr, StatusCanceled, fatal.Error())
			continue
		}
		snap, err := j.statement(ctx, sr, base, lemmas)
		if err != nil {
			fatal = err
			j.setStatus(sr, StatusCanceled, err.Error())
			j.emit(Event{Type: EvError, Stmt: sr.Index, Message: err.Error()})
			continue
		}
		if snap != nil {
			base = snap
		}
		if sr.Signature != "" {
			lemmas = append(lemmas, llm.Lemma{Name: sr.Name, Signature: sr.Signature, Text: sr.Text, Proved: sr.Status == StatusProved})
		}
	}
	if p.opts.Verifier != nil && fatal == nil {
		if err := j.verify(ctx, base); err != nil {
			fatal = err
		}
	}
	j.res.LeanFile = j.leanFile(base)
	j.emit(Event{Type: EvDone, Message: j.summary()})
	return j.res, fatal
}

func (j *job) split(ctx context.Context, input string) ([]string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, errors.New("empty input")
	}
	if j.p.opts.SkipSplit {
		return []string{input}, nil
	}
	texts, err := j.p.agent.SplitStatements(ctx, input)
	if err != nil {
		if fatalErr(ctx, err) {
			return nil, err
		}
		// Splitting is a convenience; fall back to one statement.
		j.emit(Event{Type: EvError, Message: "split failed, treating input as one statement: " + err.Error()})
		return []string{input}, nil
	}
	return texts, nil
}

// statement formalizes and proves one statement. It returns the new base
// snapshot if proved, and an error only for fatal conditions.
func (j *job) statement(ctx context.Context, sr *StatementResult, base *lean.Snapshot, lemmas []llm.Lemma) (*lean.Snapshot, error) {
	j.setStatus(sr, StatusFormalizing, "")
	stmt, ok, err := j.formalize(ctx, sr, base, lemmas)
	if err != nil || !ok {
		return nil, err
	}

	j.setStatus(sr, StatusProving, "")
	o, err := j.prove(ctx, sr, stmt, base, lemmas)
	if err != nil {
		return nil, err
	}
	if o == nil {
		j.setStatus(sr, StatusFailed, fmt.Sprintf("no proof found in %d attempt(s)", len(sr.Attempts)))
		j.emit(Event{Type: EvFailed, Stmt: sr.Index, Statement: j.copyOf(sr)})
		return nil, nil
	}

	j.mu.Lock()
	sr.Status = StatusProved
	sr.Proof = o.Attempt.Body
	sr.LeanCode = o.Attempt.Code
	sr.Axioms = o.Verdict.Axioms
	j.mu.Unlock()
	j.emit(Event{Type: EvProved, Stmt: sr.Index, Statement: j.copyOf(sr)})

	if !j.p.opts.NoExplain {
		exp, err := j.p.agent.Explain(ctx, sr.Text, sr.Name, sr.Signature, sr.Proof)
		if err != nil {
			if fatalErr(ctx, err) {
				return o.Snapshot(), err
			}
			j.emit(Event{Type: EvError, Stmt: sr.Index, Message: "explanation failed: " + err.Error()})
		} else {
			j.mu.Lock()
			sr.Explanation = exp
			j.mu.Unlock()
			j.emit(Event{Type: EvExplained, Stmt: sr.Index, Message: exp})
		}
	}
	return o.Snapshot(), nil
}

// formalize loops: LLM proposes a signature, Lean checks it, the user
// optionally confirms or corrects it.
func (j *job) formalize(ctx context.Context, sr *StatementResult, base *lean.Snapshot, lemmas []llm.Lemma) (lean.Statement, bool, error) {
	var prev *llm.FormalizeRetry
	leanFails, revisions := 0, 0
	for {
		j.emit(Event{Type: EvFormalizing, Stmt: sr.Index, Message: fmt.Sprintf("attempt %d", leanFails+revisions+1)})
		f, err := j.p.agent.Formalize(ctx, llm.FormalizeInput{Name: sr.Name, Statement: sr.Text, Context: lemmas, Previous: prev})
		if err != nil {
			if fatalErr(ctx, err) {
				return lean.Statement{}, false, err
			}
			// Unparseable reply: count it like a Lean rejection.
			leanFails++
			if leanFails >= j.p.opts.MaxFormalizeAttempts {
				j.setStatus(sr, StatusUnformal, err.Error())
				return lean.Statement{}, false, nil
			}
			continue
		}
		stmt := lean.Statement{Name: sr.Name, Signature: f.Signature}

		problem := ""
		if err := stmt.Validate(); err != nil {
			problem = err.Error()
		} else {
			sc, err := j.p.check.CheckStatement(ctx, base, stmt)
			if err != nil {
				return lean.Statement{}, false, fmt.Errorf("lean: %w", err)
			}
			if !sc.OK {
				problem = renderStatementErrors(sc)
			}
		}
		if problem != "" {
			leanFails++
			j.emit(Event{Type: EvStatementError, Stmt: sr.Index, Message: problem})
			if leanFails >= j.p.opts.MaxFormalizeAttempts {
				j.setStatus(sr, StatusUnformal, "Lean rejected every formalization; last error:\n"+problem)
				return lean.Statement{}, false, nil
			}
			prev = &llm.FormalizeRetry{Signature: f.Signature, Problem: "Lean error:\n" + problem}
			continue
		}

		back, err := j.p.agent.BackTranslate(ctx, sr.Name, f.Signature, lemmas)
		if err != nil {
			if fatalErr(ctx, err) {
				return lean.Statement{}, false, err
			}
			back = "(back-translation unavailable: " + err.Error() + ")"
		}
		j.mu.Lock()
		sr.Signature, sr.FormalizationNotes, sr.BackTranslation = f.Signature, f.Notes, back
		j.mu.Unlock()
		j.emit(Event{Type: EvFormalized, Stmt: sr.Index, Statement: j.copyOf(sr)})

		if j.hooks.Confirm == nil {
			return stmt, true, nil
		}
		d, err := j.hooks.Confirm(ctx, Proposal{
			Index: sr.Index, Text: sr.Text, Name: sr.Name, Signature: f.Signature,
			Notes: f.Notes, BackTranslation: back,
		})
		if err != nil {
			return lean.Statement{}, false, fmt.Errorf("confirm: %w", err)
		}
		if d.Accept {
			return stmt, true, nil
		}
		if strings.TrimSpace(d.Correction) == "" || revisions >= j.p.opts.MaxRevisions {
			j.setStatus(sr, StatusUnformal, "formalization rejected by user")
			return lean.Statement{}, false, nil
		}
		revisions++
		j.emit(Event{Type: EvRevising, Stmt: sr.Index, Message: d.Correction})
		prev = &llm.FormalizeRetry{Signature: f.Signature, Problem: "The user says this does not match the intended statement:\n" + d.Correction}
	}
}

func renderStatementErrors(sc *lean.StatementCheck) string {
	var b strings.Builder
	for _, d := range sc.Errors {
		fmt.Fprintf(&b, "line %d (%s): %s\n", d.Pos.Line, d.Kind, d.Data)
	}
	return strings.TrimSpace(b.String())
}

// prove first tries automation tactics directly in Lean, then climbs the
// stage ladder: each stage races its sessions, the first verified outcome
// wins and cancels the rest, and the next (more expensive) stage only runs
// if the current one found nothing. Returns (nil, nil) if all failed.
func (j *job) prove(ctx context.Context, sr *StatementResult, stmt lean.Statement, base *lean.Snapshot, lemmas []llm.Lemma) (*lean.Outcome, error) {
	if !j.p.opts.NoAutomation {
		o, err := j.automate(ctx, sr, stmt, base)
		if err != nil || o != nil {
			return o, err
		}
	}
	in := llm.ProveInput{Name: stmt.Name, Signature: stmt.Signature, Text: sr.Text, Context: lemmas}
	failed := &failedSet{m: map[string]string{}}
	sessionNo := 0
	stages := j.p.stages()
	for i, st := range stages {
		j.emit(Event{Type: EvStage, Stmt: sr.Index, Message: fmt.Sprintf("stage %d/%d %q: %s effort %s, %d session(s) × %d attempt(s)",
			i+1, len(stages), st.Name, st.Spec.Model, effortName(st.Spec.Effort), st.Sessions, st.Attempts)})
		o, err := j.runStage(ctx, sr, stmt, base, in, st, &sessionNo, failed)
		if err != nil || o != nil {
			return o, err
		}
	}
	return nil, nil
}

// DefaultAutomation is tried, in order, before any LLM call. Each runs as
// its own check, so one tactic's failure (or timeout) never hides another.
var DefaultAutomation = []string{
	"omega", "decide", "norm_num", "simp", "positivity",
	"linarith", "nlinarith", "aesop", "exact?",
}

var reTryThis = regexp.MustCompile(`(?s)^Try this:\s*(?:\[apply\]\s*)?(.+)$`)

// automate tries each automation tactic as a complete proof.
func (j *job) automate(ctx context.Context, sr *StatementResult, stmt lean.Statement, base *lean.Snapshot) (*lean.Outcome, error) {
	tactics := j.p.opts.Automation
	if tactics == nil {
		tactics = DefaultAutomation
	}
	t0 := time.Now()
	for i, tac := range tactics {
		t1 := time.Now()
		o, err := j.p.check.CheckProof(ctx, base, stmt, tac)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("lean: %w", err)
		}
		if !o.Verdict.Verified {
			continue
		}
		// `exact?` closes the goal by search; keep the term it found instead,
		// so the final file is fast to re-check and readable.
		if o.Result != nil {
			for _, m := range o.Result.Messages {
				if mm := reTryThis.FindStringSubmatch(strings.TrimSpace(m.Data)); mm != nil && m.Severity == "info" {
					if o2, err := j.p.check.CheckProof(ctx, base, stmt, mm[1]); err == nil && o2.Verdict.Verified {
						o, tac = o2, mm[1]
					}
					break
				}
			}
		}
		j.record(sr, AttemptRecord{Stage: "auto", N: i + 1, Body: tac, Verified: true, LeanTime: time.Since(t1)})
		j.emit(Event{Type: EvAutomation, Stmt: sr.Index, Message: fmt.Sprintf("closed by automation: %s", tac)})
		return o, nil
	}
	j.emit(Event{Type: EvAutomation, Stmt: sr.Index, Message: fmt.Sprintf("automation: none of %d tactics closed the goal (%s)",
		len(tactics), time.Since(t0).Round(10*time.Millisecond))})
	return nil, nil
}

// failedSet holds proofs already rejected for one statement, shared by all
// sessions and stages, so repeats are not re-checked.
type failedSet struct {
	mu sync.Mutex
	m  map[string]string // normalized body -> feedback given
}

func (f *failedSet) get(body string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fb, ok := f.m[body]
	return fb, ok
}

func (f *failedSet) add(body, feedback string) {
	f.mu.Lock()
	f.m[body] = feedback
	f.mu.Unlock()
}

// runStage races st.Sessions sessions; the first verified outcome wins.
func (j *job) runStage(ctx context.Context, sr *StatementResult, stmt lean.Statement, base *lean.Snapshot,
	in llm.ProveInput, st Stage, sessionNo *int, failed *failedSet) (*lean.Outcome, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type res struct {
		o   *lean.Outcome
		err error
	}
	ch := make(chan res, st.Sessions)
	for range st.Sessions {
		*sessionNo++
		sn := *sessionNo
		go func() {
			o, err := j.session(ctx, sn, st, sr, stmt, base, in, failed)
			ch <- res{o, err}
		}()
	}
	var win *lean.Outcome
	var firstErr error
	for range st.Sessions {
		r := <-ch
		switch {
		case r.o != nil && win == nil:
			win = r.o
			cancel()
		case r.err != nil && firstErr == nil && !errors.Is(r.err, context.Canceled):
			firstErr = r.err
		}
	}
	if win != nil {
		return win, nil
	}
	if err := ctx.Err(); err != nil && firstErr == nil {
		// parent canceled (our own cancel only fires after a win)
		return nil, err
	}
	return nil, firstErr
}

// thinkingEvery throttles "thinking…" progress events per attempt.
const thinkingEvery = 10 * time.Second

// session runs one propose → check → feedback loop.
func (j *job) session(ctx context.Context, sn int, st Stage, sr *StatementResult, stmt lean.Statement,
	base *lean.Snapshot, in llm.ProveInput, failed *failedSet) (*lean.Outcome, error) {
	sess := j.p.agent.NewProofSessionWith(in, st.Spec)
	for k := 1; k <= st.Attempts; k++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rec := AttemptRecord{Stage: st.Name, Session: sn, N: k, Model: st.Spec.Model, Effort: effortName(st.Spec.Effort)}
		t0 := time.Now()
		lastEmit := t0
		var progMu sync.Mutex
		body, resp, err := sess.NextProgress(ctx, func(p llm.Progress) {
			progMu.Lock()
			defer progMu.Unlock()
			if time.Since(lastEmit) < thinkingEvery {
				return
			}
			lastEmit = time.Now()
			j.emit(Event{Type: EvThinking, Stmt: sr.Index, Message: fmt.Sprintf("session %d attempt %d (%s): thinking… %d tokens after %s",
				sn, k, st.Name, p.ReasoningChunks+p.ContentChunks, time.Since(t0).Round(time.Second))})
		})
		rec.LLMTime = time.Since(t0)
		if resp != nil {
			rec.CostUSD = resp.CostUSD
			rec.CompletionTokens = resp.Usage.CompletionTokens
			rec.ReasoningTokens = resp.Usage.ReasoningTokens()
		}
		if err != nil {
			if fatalErr(ctx, err) {
				return nil, err
			}
			if errors.Is(err, llm.ErrTruncated) {
				rec.ErrorKinds = []string{"truncated"}
				j.record(sr, rec)
				continue // the session has no pending reply; just ask again
			}
			return nil, err // non-transient API error after client retries
		}
		rec.Body = body

		norm := lean.NormalizeBody(body)
		if fb, seen := failed.get(norm); seen {
			rec.ErrorKinds = []string{"repeated"}
			rec.Feedback = "This proof is identical to an earlier attempt that already failed with:\n\n" + fb +
				"\n\nUse a different approach."
			j.record(sr, rec)
			sess.Feedback(rec.Feedback)
			continue
		}

		t1 := time.Now()
		o, err := j.p.check.CheckProof(ctx, base, stmt, body)
		rec.LeanTime = time.Since(t1)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("lean: %w", err)
		}
		rec.Verified = o.Verdict.Verified
		for _, d := range o.Verdict.Errors {
			rec.ErrorKinds = append(rec.ErrorKinds, string(d.Kind))
		}
		if !rec.Verified {
			rec.Feedback = lean.RenderFeedback(o, j.p.opts.Feedback)
		}
		j.record(sr, rec)
		if rec.Verified {
			return o, nil
		}
		failed.add(norm, rec.Feedback)
		sess.Feedback(rec.Feedback)
	}
	return nil, nil
}

func effortName(e llm.Effort) string {
	if e == llm.EffortDefault {
		return "default"
	}
	return string(e)
}

// fatalErr reports errors that should stop the whole job.
func fatalErr(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, llm.ErrBudget) {
		return true
	}
	var ae *llm.APIError
	// 401 bad key, 402 no balance: nothing will work until fixed.
	return errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 402)
}

func (j *job) record(sr *StatementResult, rec AttemptRecord) {
	j.mu.Lock()
	sr.Attempts = append(sr.Attempts, rec)
	j.mu.Unlock()
	r := rec
	j.emit(Event{Type: EvAttempt, Stmt: sr.Index, Attempt: &r})
}

func (j *job) setStatus(sr *StatementResult, s Status, msg string) {
	j.mu.Lock()
	sr.Status = s
	if msg != "" {
		sr.Error = msg
	}
	j.mu.Unlock()
}

func (j *job) copyOf(sr *StatementResult) *StatementResult {
	j.mu.Lock()
	defer j.mu.Unlock()
	c := *sr
	c.Attempts = append([]AttemptRecord(nil), sr.Attempts...)
	return &c
}

func (j *job) emit(e Event) {
	if j.hooks.OnEvent == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.hooks.OnEvent(e)
}

// verify re-checks all proved statements in fresh processes. Statements
// it rejects become failed; if it cannot run, they become unverified.
// Only cancellation is returned as an error.
func (j *job) verify(ctx context.Context, base *lean.Snapshot) error {
	var proved []*StatementResult
	var items []lean.VerifyItem
	for _, sr := range j.res.Statements {
		if sr.Status == StatusProved {
			proved = append(proved, sr)
			items = append(items, lean.VerifyItem{Name: sr.Name, Signature: sr.Signature})
		}
	}
	if len(items) == 0 {
		return nil
	}
	j.emit(Event{Type: EvVerifying, Message: fmt.Sprintf("independently re-checking %d proof(s)", len(items))})
	code := j.p.opts.LeanHeader + "\n\n" + base.Code() + "\n"
	rep, err := j.p.opts.Verifier.Verify(ctx, code, items)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, sr := range proved {
			j.setStatus(sr, StatusUnverified, "independent verification could not run: "+err.Error())
		}
		j.emit(Event{Type: EvError, Message: "independent verification could not run: " + err.Error()})
		return nil
	}
	j.mu.Lock()
	j.res.Verification = rep
	rejected := 0
	for _, sr := range proved {
		it, ok := rep.Item(sr.Name)
		if !ok {
			it = lean.VerifiedItem{Name: sr.Name, Error: "missing from verification report"}
		}
		sr.Verification = &it
		if !it.OK {
			rejected++
			sr.Status = StatusFailed
			sr.Error = "independent verification rejected the proof: " + it.Error
			if rep.Log != "" {
				sr.Error += "\n" + rep.Log
			}
		}
	}
	j.mu.Unlock()
	msg := fmt.Sprintf("%d/%d proof(s) confirmed by kernel re-check and axiom audit (%s)",
		len(proved)-rejected, len(proved), rep.Duration.Round(100*time.Millisecond))
	j.emit(Event{Type: EvVerified, Message: msg})
	return nil
}

func (j *job) leanFile(base *lean.Snapshot) string {
	var b strings.Builder
	b.WriteString(j.p.opts.LeanHeader + "\n\n")
	if code := base.Code(); code != "" {
		b.WriteString(code + "\n")
	}
	for _, sr := range j.res.Statements {
		if sr.Status == StatusProved || sr.Signature == "" {
			continue
		}
		fmt.Fprintf(&b, "\n/- Not proved (%s):\ntheorem %s %s := by\n  sorry\n-/\n", sr.Status, sr.Name, sr.Signature)
	}
	return b.String()
}

func (j *job) summary() string {
	counts := map[Status]int{}
	for _, sr := range j.res.Statements {
		counts[sr.Status]++
	}
	return fmt.Sprintf("%d/%d proved", counts[StatusProved], len(j.res.Statements))
}
