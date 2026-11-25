package jobs

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"mathprover/internal/llm"
	"mathprover/internal/pipeline"
)

var (
	ErrNotFound    = errors.New("job not found")
	ErrQueueFull   = errors.New("too many jobs queued")
	ErrClosed      = errors.New("server is shutting down")
	ErrInvalid     = errors.New("invalid request")
	ErrDailyBudget = errors.New("daily LLM budget exhausted")
)

// Deps are the shared resources every job uses.
type Deps struct {
	Chatter     llm.Chatter      // one HTTP client shared by all jobs
	Checker     pipeline.Checker // usually PoolChecker over one lean.Pool
	Models      llm.Models
	LeanVersion string
}

// Config bounds what clients can ask for. Zero values take defaults.
type Config struct {
	MaxRunning       int           // concurrent jobs; default 2
	MaxQueued        int           // waiting jobs before ErrQueueFull; default 32
	MaxInputBytes    int           // default 8192
	MaxStatements    int           // per job; default 10
	DefaultBudgetUSD float64       // default 0.25
	MaxBudgetUSD     float64       // default 1.00
	DailyBudgetUSD   float64       // across all jobs, per UTC day; default 5.00; <0 = unlimited
	DefaultAttempts  int           // default 6
	MaxAttempts      int           // default 12
	MaxParallel      int           // default 4
	JobTimeout       time.Duration // default 30m
	ConfirmTimeout   time.Duration // default 30m
	Retention        time.Duration // finished jobs kept; default 24h
	MaxJobs          int           // finished jobs kept; default 1000
}

func (c Config) withDefaults() Config {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.MaxRunning, 2)
	def(&c.MaxQueued, 32)
	def(&c.MaxInputBytes, 8192)
	def(&c.MaxStatements, 10)
	def(&c.DefaultAttempts, 6)
	def(&c.MaxAttempts, 12)
	def(&c.MaxParallel, 4)
	def(&c.MaxJobs, 1000)
	if c.DefaultBudgetUSD <= 0 {
		c.DefaultBudgetUSD = 0.25
	}
	if c.MaxBudgetUSD <= 0 {
		c.MaxBudgetUSD = 1.00
	}
	if c.DailyBudgetUSD == 0 {
		c.DailyBudgetUSD = 5.00
	}
	if c.JobTimeout <= 0 {
		c.JobTimeout = 30 * time.Minute
	}
	if c.ConfirmTimeout <= 0 {
		c.ConfirmTimeout = 30 * time.Minute
	}
	if c.Retention <= 0 {
		c.Retention = 24 * time.Hour
	}
	return c
}

// Manager owns all jobs. Jobs live in memory and are lost on restart.
type Manager struct {
	deps Deps
	cfg  Config

	base       context.Context // parent of every job; canceled on forced shutdown
	baseCancel context.CancelFunc
	sem        chan struct{} // running slots
	wg         sync.WaitGroup
	stop       chan struct{}

	mu       sync.Mutex
	jobs     map[string]*Job
	active   int // queued + running
	closed   bool
	day      string
	daySpent float64
}

// NewManager starts a manager (and its janitor goroutine).
func NewManager(deps Deps, cfg Config) *Manager {
	cfg = cfg.withDefaults()
	base, cancel := context.WithCancel(context.Background())
	m := &Manager{
		deps: deps, cfg: cfg,
		base: base, baseCancel: cancel,
		sem:  make(chan struct{}, cfg.MaxRunning),
		stop: make(chan struct{}),
		jobs: map[string]*Job{},
	}
	go m.janitor()
	return m
}

// Config returns the effective configuration.
func (m *Manager) Config() Config { return m.cfg }

// Submit validates and enqueues a job.
func (m *Manager) Submit(input string, opts Options) (*Job, error) {
	input = strings.TrimSpace(input)
	switch {
	case input == "":
		return nil, fmt.Errorf("%w: input is empty", ErrInvalid)
	case len(input) > m.cfg.MaxInputBytes:
		return nil, fmt.Errorf("%w: input is %d bytes, limit is %d", ErrInvalid, len(input), m.cfg.MaxInputBytes)
	case opts.BudgetUSD < 0 || opts.Attempts < 0 || opts.Parallel < 0:
		return nil, fmt.Errorf("%w: negative option", ErrInvalid)
	}
	if opts.Attempts == 0 {
		opts.Attempts = m.cfg.DefaultAttempts
	}
	opts.Attempts = min(opts.Attempts, m.cfg.MaxAttempts)
	opts.Parallel = min(max(opts.Parallel, 1), m.cfg.MaxParallel)
	if opts.BudgetUSD == 0 {
		opts.BudgetUSD = m.cfg.DefaultBudgetUSD
	}
	opts.BudgetUSD = min(opts.BudgetUSD, m.cfg.MaxBudgetUSD)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if m.active >= m.cfg.MaxRunning+m.cfg.MaxQueued {
		return nil, ErrQueueFull
	}
	budget := opts.BudgetUSD
	if m.cfg.DailyBudgetUSD > 0 {
		m.rollDayLocked()
		left := m.cfg.DailyBudgetUSD - m.daySpent
		if left <= 0 {
			return nil, ErrDailyBudget
		}
		// Approximate: concurrent jobs may together overshoot by one job's budget.
		budget = min(budget, left)
	}

	j := newJob(newID(), input, opts, budget)
	ctx, cancel := context.WithTimeout(m.base, m.cfg.JobTimeout)
	j.cancel = cancel
	m.jobs[j.ID] = j
	m.active++
	m.wg.Add(1)
	go m.run(ctx, cancel, j)
	return j, nil
}

func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, j *Job) {
	defer m.wg.Done()
	defer cancel()
	defer func() {
		m.mu.Lock()
		m.active--
		m.mu.Unlock()
	}()

	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		j.finish(nil, ctx.Err(), m.base.Err() != nil)
		return
	}

	j.mu.Lock()
	j.started = time.Now()
	j.setStateLocked(StateRunning)
	j.mu.Unlock()

	agent := llm.NewAgent(m.deps.Chatter, llm.Options{
		Models: m.deps.Models, LeanVersion: m.deps.LeanVersion, BudgetUSD: j.BudgetUSD,
	})
	p := pipeline.New(agent, m.deps.Checker, pipeline.Options{
		MaxProofAttempts: j.Options.Attempts,
		ParallelSessions: j.Options.Parallel,
		SkipSplit:        j.Options.NoSplit,
		NoExplain:        j.Options.NoExplain,
		MaxStatements:    m.cfg.MaxStatements,
	})
	hooks := pipeline.Hooks{OnEvent: j.onPipelineEvent}
	if j.Options.Confirm {
		hooks.Confirm = j.confirmFunc(m.cfg.ConfirmTimeout)
	}
	res, err := p.Run(ctx, j.Input, hooks)

	usd, _ := agent.Spent()
	m.mu.Lock()
	m.rollDayLocked()
	m.daySpent += usd
	m.mu.Unlock()

	j.finish(res, err, m.base.Err() != nil)
}

// Get returns a job by id.
func (m *Manager) Get(id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return j, nil
}

// List returns jobs, newest first.
func (m *Manager) List() []*Job {
	m.mu.Lock()
	out := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j)
	}
	m.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].Created.After(out[b].Created) })
	return out
}

// Confirm answers a pending confirmation for statement stmt (1-based).
func (m *Manager) Confirm(id string, stmt int, d pipeline.Decision) error {
	j, err := m.Get(id)
	if err != nil {
		return err
	}
	return j.confirm(stmt, d)
}

// Cancel stops a queued or running job. Canceling a finished job is a no-op.
func (m *Manager) Cancel(id string) error {
	j, err := m.Get(id)
	if err != nil {
		return err
	}
	j.requestCancel()
	return nil
}

// Stats is a snapshot for health endpoints.
type Stats struct {
	Active        int     `json:"active"` // queued + running
	Jobs          int     `json:"jobs"`   // kept in memory
	DailySpentUSD float64 `json:"daily_spent_usd"`
}

func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollDayLocked()
	return Stats{Active: m.active, Jobs: len(m.jobs), DailySpentUSD: m.daySpent}
}

// Shutdown stops accepting jobs and waits for running ones until ctx is
// done; then it cancels the rest and waits for them to wind down.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	close(m.stop)

	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		m.baseCancel()
		<-done
		return ctx.Err()
	}
}

func (m *Manager) rollDayLocked() {
	if d := time.Now().UTC().Format("2006-01-02"); d != m.day {
		m.day, m.daySpent = d, 0
	}
}

func (m *Manager) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.prune(time.Now())
		case <-m.stop:
			return
		}
	}
}

// prune drops finished jobs past Retention, then the oldest finished ones
// beyond MaxJobs.
func (m *Manager) prune(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	type fin struct {
		id string
		at time.Time
	}
	var done []fin
	for id, j := range m.jobs {
		j.mu.Lock()
		terminal, at := j.state.Terminal(), j.finished
		j.mu.Unlock()
		if !terminal {
			continue
		}
		if now.Sub(at) > m.cfg.Retention {
			delete(m.jobs, id)
			continue
		}
		done = append(done, fin{id, at})
	}
	if over := len(m.jobs) - m.cfg.MaxJobs; over > 0 {
		sort.Slice(done, func(a, b int) bool { return done[a].at.Before(done[b].at) })
		for _, f := range done[:min(over, len(done))] {
			delete(m.jobs, f.id)
		}
	}
}

var idEnc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func newID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return idEnc.EncodeToString(b) // 16 chars, unguessable
}
