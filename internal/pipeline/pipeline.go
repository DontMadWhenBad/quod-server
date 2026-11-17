package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"mathprover/internal/lean"
	"mathprover/internal/llm"
)

// Options tunes a Pipeline. Zero values get defaults.
type Options struct {
	MaxFormalizeAttempts int  // Lean-rejected formalizations before giving up; default 3
	MaxRevisions         int  // user corrections per statement; default 3
	MaxProofAttempts     int  // per proof session; default 6
	ParallelSessions     int  // independent proof sessions racing; default 1
	SkipSplit            bool // treat the whole input as one statement
	NoExplain            bool
	// LeanHeader starts LeanFile; default "import Mathlib\nimport Harness".
	LeanHeader string
	Feedback   lean.FeedbackOptions
}

func (o Options) withDefaults() Options {
	if o.MaxFormalizeAttempts <= 0 {
		o.MaxFormalizeAttempts = 3
	}
	if o.MaxRevisions <= 0 {
		o.MaxRevisions = 3
	}
	if o.MaxProofAttempts <= 0 {
		o.MaxProofAttempts = 6
	}
	if o.ParallelSessions <= 0 {
		o.ParallelSessions = 1
	}
	if o.LeanHeader == "" {
		o.LeanHeader = "import Mathlib\nimport Harness"
	}
	return o
}

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

// prove races ParallelSessions proof sessions; the first verified outcome
// wins and cancels the rest. Returns (nil, nil) if none succeeded.
func (j *job) prove(ctx context.Context, sr *StatementResult, stmt lean.Statement, base *lean.Snapshot, lemmas []llm.Lemma) (*lean.Outcome, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	n := j.p.opts.ParallelSessions
	type res struct {
		o   *lean.Outcome
		err error
	}
	ch := make(chan res, n)
	in := llm.ProveInput{Name: stmt.Name, Signature: stmt.Signature, Text: sr.Text, Context: lemmas}
	for s := 1; s <= n; s++ {
		go func() {
			o, err := j.session(ctx, s, sr, stmt, base, in)
			ch <- res{o, err}
		}()
	}
	var win *lean.Outcome
	var firstErr error
	for range n {
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

// session runs one propose → check → feedback loop.
func (j *job) session(ctx context.Context, sn int, sr *StatementResult, stmt lean.Statement, base *lean.Snapshot, in llm.ProveInput) (*lean.Outcome, error) {
	sess := j.p.agent.NewProofSession(in)
	failed := map[string]bool{} // normalized bodies already rejected
	for k := 1; k <= j.p.opts.MaxProofAttempts; k++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rec := AttemptRecord{Session: sn, N: k}
		t0 := time.Now()
		body, resp, err := sess.Next(ctx)
		rec.LLMTime = time.Since(t0)
		if resp != nil {
			rec.CostUSD = resp.CostUSD
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
		if failed[norm] {
			rec.ErrorKinds = []string{"repeated"}
			rec.Feedback = "This proof is identical to an earlier attempt that already failed. Use a different approach."
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
		failed[norm] = true
		sess.Feedback(rec.Feedback)
	}
	return nil, nil
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
