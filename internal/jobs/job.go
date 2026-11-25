// Package jobs runs pipeline jobs in the background: queueing and limits,
// a replayable event log per job, and the confirmation hand-off between a
// running pipeline and an HTTP client.
package jobs

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"mathprover/internal/pipeline"
)

// State of a job.
type State string

const (
	StateQueued   State = "queued"
	StateRunning  State = "running"
	StateAwaiting State = "awaiting_confirmation"
	StateDone     State = "done"
	StateFailed   State = "failed"
	StateCanceled State = "canceled"
)

// Terminal reports whether the job has finished.
func (s State) Terminal() bool {
	return s == StateDone || s == StateFailed || s == StateCanceled
}

// Job-level event types, in addition to pipeline.EventType values.
const (
	EvState           pipeline.EventType = "state"            // State set
	EvConfirmRequired pipeline.EventType = "confirm_required" // Proposal set
)

// Event is a pipeline or job event with a sequence number (1-based), used
// as the SSE event id for resuming.
type Event struct {
	Seq int `json:"seq"`
	pipeline.Event
	State    State              `json:"state,omitempty"`
	Proposal *pipeline.Proposal `json:"proposal,omitempty"`
}

// WithoutFeedback returns a copy with the Lean feedback text removed
// (it is large and mostly useful for debugging).
func (e Event) WithoutFeedback() Event {
	if e.Attempt != nil && e.Attempt.Feedback != "" {
		a := *e.Attempt
		a.Feedback = ""
		e.Attempt = &a
	}
	if e.Statement != nil {
		e.Statement = stripStatement(e.Statement)
	}
	return e
}

// Options are per-job settings supplied by the client. Zero values take
// the manager's defaults; values above the manager's limits are clamped.
type Options struct {
	Confirm   bool    `json:"confirm,omitempty"`    // wait for approval of each formalization
	BudgetUSD float64 `json:"budget_usd,omitempty"` // LLM spending cap for this job
	NoSplit   bool    `json:"no_split,omitempty"`
	NoExplain bool    `json:"no_explain,omitempty"`
	Attempts  int     `json:"attempts,omitempty"` // proof attempts per session
	Parallel  int     `json:"parallel,omitempty"` // proof sessions per statement
}

var (
	ErrNoPending      = errors.New("no confirmation pending for that statement")
	ErrConfirmTimeout = errors.New("confirmation timed out")
)

// Job is one submitted input. Fields above mu are immutable.
type Job struct {
	ID        string
	Input     string
	Options   Options
	BudgetUSD float64 // effective budget after clamping
	Created   time.Time

	mu        sync.Mutex
	state     State
	started   time.Time
	finished  time.Time
	result    *pipeline.Result
	errMsg    string
	pending   *pipeline.Proposal
	stmts     map[int]*pipeline.StatementResult // live view built from events
	events    []Event
	changed   chan struct{} // closed and replaced on every new event
	confirmCh chan pipeline.Decision
	cancel    context.CancelFunc
	canceled  bool // cancel requested by a client
}

func newJob(id, input string, opts Options, budget float64) *Job {
	j := &Job{
		ID: id, Input: input, Options: opts, BudgetUSD: budget, Created: time.Now(),
		state:     StateQueued,
		stmts:     map[int]*pipeline.StatementResult{},
		changed:   make(chan struct{}),
		confirmCh: make(chan pipeline.Decision, 1),
	}
	j.appendLocked(Event{Event: pipeline.Event{Type: EvState}, State: StateQueued})
	return j
}

// State returns the current state.
func (j *Job) State() State {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state
}

// Since returns the events after seq, a channel closed when more arrive,
// and whether the job is finished (no more events will come).
func (j *Job) Since(seq int) ([]Event, <-chan struct{}, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if seq < 0 {
		seq = 0
	}
	var out []Event
	if seq < len(j.events) {
		out = append(out, j.events[seq:]...)
	}
	return out, j.changed, j.state.Terminal()
}

// appendLocked adds an event and wakes subscribers. Caller holds j.mu.
func (j *Job) appendLocked(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Seq = len(j.events) + 1
	j.events = append(j.events, e)
	close(j.changed)
	j.changed = make(chan struct{})
}

func (j *Job) setStateLocked(s State) {
	if j.state == s {
		return
	}
	j.state = s
	j.appendLocked(Event{Event: pipeline.Event{Type: EvState}, State: s})
}

// onPipelineEvent records a pipeline event and updates the live view.
func (j *Job) onPipelineEvent(e pipeline.Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if e.Statement != nil {
		c := *e.Statement
		j.stmts[e.Stmt] = &c
	}
	if e.Attempt != nil {
		s := j.stmts[e.Stmt]
		if s == nil {
			s = &pipeline.StatementResult{Index: e.Stmt}
			j.stmts[e.Stmt] = s
		}
		s.Attempts = append(s.Attempts, *e.Attempt)
	}
	j.appendLocked(Event{Event: e})
}

// confirmFunc blocks the pipeline until the client answers via Confirm.
func (j *Job) confirmFunc(timeout time.Duration) pipeline.ConfirmFunc {
	return func(ctx context.Context, p pipeline.Proposal) (pipeline.Decision, error) {
		j.mu.Lock()
		// Drop any stale answer before publishing the new proposal.
		select {
		case <-j.confirmCh:
		default:
		}
		j.pending = &p
		j.setStateLocked(StateAwaiting)
		j.appendLocked(Event{Event: pipeline.Event{Type: EvConfirmRequired, Stmt: p.Index}, Proposal: &p})
		j.mu.Unlock()

		defer func() {
			j.mu.Lock()
			j.pending = nil
			if !j.state.Terminal() {
				j.setStateLocked(StateRunning)
			}
			j.mu.Unlock()
		}()

		t := time.NewTimer(timeout)
		defer t.Stop()
		select {
		case d := <-j.confirmCh:
			return d, nil
		case <-ctx.Done():
			return pipeline.Decision{}, ctx.Err()
		case <-t.C:
			return pipeline.Decision{}, ErrConfirmTimeout
		}
	}
}

// confirm delivers the client's decision for statement stmt.
func (j *Job) confirm(stmt int, d pipeline.Decision) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.pending == nil || j.pending.Index != stmt {
		return ErrNoPending
	}
	j.pending = nil  // a second answer gets ErrNoPending
	j.confirmCh <- d // buffered(1) and drained before each proposal
	return nil
}

func (j *Job) requestCancel() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.Terminal() {
		return
	}
	j.canceled = true
	if j.cancel != nil {
		j.cancel()
	}
}

func (j *Job) finish(res *pipeline.Result, err error, shuttingDown bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.result = res
	j.finished = time.Now()
	j.pending = nil
	switch {
	case err == nil:
		j.setStateLocked(StateDone)
	case j.canceled:
		j.errMsg = "canceled by client"
		j.setStateLocked(StateCanceled)
	case shuttingDown:
		j.errMsg = "server shutting down"
		j.setStateLocked(StateCanceled)
	case errors.Is(err, context.DeadlineExceeded):
		j.errMsg = "job timed out"
		j.setStateLocked(StateFailed)
	default:
		j.errMsg = err.Error()
		j.setStateLocked(StateFailed)
	}
}

// View is the JSON representation of a job.
type View struct {
	ID         string                      `json:"id"`
	State      State                       `json:"state"`
	Input      string                      `json:"input"`
	Options    Options                     `json:"options"`
	BudgetUSD  float64                     `json:"budget_usd"`
	Created    time.Time                   `json:"created"`
	Started    *time.Time                  `json:"started,omitempty"`
	Finished   *time.Time                  `json:"finished,omitempty"`
	Pending    *pipeline.Proposal          `json:"pending_confirmation,omitempty"`
	Statements []*pipeline.StatementResult `json:"statements"`
	LeanFile   string                      `json:"lean_file,omitempty"`
	CostUSD    float64                     `json:"cost_usd"`
	LLMCalls   int                         `json:"llm_calls"`
	Error      string                      `json:"error,omitempty"`
	Events     int                         `json:"events"` // last event seq
}

// View snapshots the job. full keeps Lean feedback text in attempts.
func (j *Job) View(full bool) View {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := View{
		ID: j.ID, State: j.state, Input: j.Input, Options: j.Options, BudgetUSD: j.BudgetUSD,
		Created: j.Created, Error: j.errMsg, Events: len(j.events),
	}
	if !j.started.IsZero() {
		t := j.started
		v.Started = &t
	}
	if !j.finished.IsZero() {
		t := j.finished
		v.Finished = &t
	}
	if j.pending != nil {
		p := *j.pending
		v.Pending = &p
	}
	var stmts []*pipeline.StatementResult
	if j.result != nil {
		stmts = j.result.Statements
		v.LeanFile, v.CostUSD, v.LLMCalls = j.result.LeanFile, j.result.CostUSD, j.result.LLMCalls
	} else {
		for _, s := range j.stmts {
			stmts = append(stmts, s)
			for _, a := range s.Attempts {
				v.CostUSD += a.CostUSD // partial: proof attempts only
			}
		}
		sort.Slice(stmts, func(a, b int) bool { return stmts[a].Index < stmts[b].Index })
	}
	v.Statements = make([]*pipeline.StatementResult, 0, len(stmts))
	for _, s := range stmts {
		c := *s
		c.Attempts = append([]pipeline.AttemptRecord(nil), s.Attempts...)
		if !full {
			stripAttempts(c.Attempts)
		}
		v.Statements = append(v.Statements, &c)
	}
	return v
}

func stripStatement(s *pipeline.StatementResult) *pipeline.StatementResult {
	c := *s
	c.Attempts = append([]pipeline.AttemptRecord(nil), s.Attempts...)
	stripAttempts(c.Attempts)
	return &c
}

func stripAttempts(as []pipeline.AttemptRecord) {
	for i := range as {
		as[i].Feedback = ""
	}
}
