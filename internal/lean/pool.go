package lean

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// ErrPoolClosed is returned by Acquire after Close.
var ErrPoolClosed = errors.New("lean: pool closed")

// PoolConfig configures a Pool of warm REPL workers.
type PoolConfig struct {
	Config
	// Size is the number of REPL processes. Each holds Mathlib in memory.
	Size int
	// MaxCommands recycles a worker after this many commands (0 = never).
	// Environments accumulate inside a process, so memory grows over time.
	MaxCommands int
	// MaxRSS recycles a worker whose resident memory exceeds this many
	// bytes (0 = never).
	MaxRSS int64
	Logger *slog.Logger
}

// Pool keeps Size REPLs with the imports already loaded. Dead or worn-out
// workers are replaced in the background.
type Pool struct {
	cfg    PoolConfig
	log    *slog.Logger
	idle   chan *REPL
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	live map[*REPL]struct{}
}

// NewPool starts the workers one at a time (parallel imports of Mathlib can
// exhaust RAM) and returns once all are ready.
func NewPool(ctx context.Context, cfg PoolConfig) (*Pool, error) {
	if cfg.Size <= 0 {
		cfg.Size = 1
	}
	cfg.Config = cfg.Config.withDefaults()
	if cfg.Env == nil {
		env, err := ResolveEnv(ctx, cfg.ProjectDir)
		if err != nil {
			return nil, err
		}
		cfg.Env = env
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	pctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		cfg:    cfg,
		log:    log,
		idle:   make(chan *REPL, cfg.Size),
		ctx:    pctx,
		cancel: cancel,
		live:   map[*REPL]struct{}{},
	}
	for i := range cfg.Size {
		t0 := time.Now()
		r, err := Start(ctx, cfg.Config)
		if err != nil {
			p.Close()
			return nil, err
		}
		p.log.Info("lean worker ready", "worker", i, "startup", time.Since(t0).Round(time.Millisecond))
		p.track(r)
		p.idle <- r
	}
	return p, nil
}

// Acquire takes an idle worker, waiting if all are busy. Always Release it.
func (p *Pool) Acquire(ctx context.Context) (*REPL, error) {
	select {
	case r := <-p.idle:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.ctx.Done():
		return nil, ErrPoolClosed
	}
}

// Release returns a worker. Dead or over-limit workers are closed and a
// replacement is started in the background.
func (p *Pool) Release(r *REPL) {
	if p.ctx.Err() != nil {
		p.retire(r)
		return
	}
	if reason := p.wornOut(r); reason != "" {
		p.log.Info("recycling lean worker", "reason", reason, "commands", r.Commands(), "age", r.Age().Round(time.Second))
		p.retire(r)
		p.wg.Add(1)
		go p.replace()
		return
	}
	p.idle <- r // buffered to Size; never blocks
}

// With runs fn on an acquired worker and releases it afterwards.
func (p *Pool) With(ctx context.Context, fn func(*REPL) error) error {
	r, err := p.Acquire(ctx)
	if err != nil {
		return err
	}
	defer p.Release(r)
	return fn(r)
}

// Close stops all workers and waits for pending replacements to finish.
func (p *Pool) Close() {
	p.cancel()
	p.wg.Wait()
	p.mu.Lock()
	rs := make([]*REPL, 0, len(p.live))
	for r := range p.live {
		rs = append(rs, r)
	}
	p.live = map[*REPL]struct{}{}
	p.mu.Unlock()
	for _, r := range rs {
		r.Close()
	}
}

func (p *Pool) wornOut(r *REPL) string {
	switch {
	case r.Dead():
		return "dead"
	case p.cfg.MaxCommands > 0 && r.Commands() >= p.cfg.MaxCommands:
		return "max commands"
	case p.cfg.MaxRSS > 0 && r.RSS() > p.cfg.MaxRSS:
		return "max rss"
	}
	return ""
}

func (p *Pool) replace() {
	defer p.wg.Done()
	backoff := time.Second
	for {
		t0 := time.Now()
		r, err := Start(p.ctx, p.cfg.Config)
		if err == nil {
			if p.ctx.Err() != nil {
				r.Close()
				return
			}
			p.log.Info("lean worker replaced", "startup", time.Since(t0).Round(time.Millisecond))
			p.track(r)
			p.idle <- r
			return
		}
		if p.ctx.Err() != nil {
			return
		}
		p.log.Error("starting lean worker failed", "err", err, "retry_in", backoff)
		select {
		case <-time.After(backoff):
		case <-p.ctx.Done():
			return
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (p *Pool) track(r *REPL) {
	p.mu.Lock()
	p.live[r] = struct{}{}
	p.mu.Unlock()
}

func (p *Pool) retire(r *REPL) {
	p.mu.Lock()
	delete(p.live, r)
	p.mu.Unlock()
	r.Close()
}
