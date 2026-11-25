package lean

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrDead means the REPL process exited or was killed; discard the REPL.
	ErrDead = errors.New("lean: repl process is dead")
	// ErrTimeout means a command exceeded its wall-clock limit. The process
	// has been killed (Lean cannot be interrupted mid-command).
	ErrTimeout = errors.New("lean: command timed out")
)

// ReplError is an error reported by the REPL protocol itself (unknown env
// id, malformed request, ...), as opposed to a Lean diagnostic.
type ReplError struct{ Msg string }

func (e *ReplError) Error() string { return "lean repl: " + e.Msg }

// Config describes how to launch a REPL process.
type Config struct {
	// ProjectDir is the Lake project with Mathlib (lean_env).
	ProjectDir string
	// REPLBin defaults to ProjectDir/.lake/packages/REPL/.lake/build/bin/repl.
	REPLBin string
	// Imports run once at startup; every command builds on them.
	// Defaults to Mathlib and Harness.
	Imports []string
	// StartTimeout bounds the initial import. Default 10m.
	StartTimeout time.Duration
	// CmdTimeout is the default wall-clock limit per command. Default 60s.
	CmdTimeout time.Duration
	// Env is the process environment. If nil it is resolved once via
	// `lake env` (see ResolveEnv), so the REPL runs without a lake wrapper.
	// It is always passed through ScrubEnv before use.
	Env []string
	// Sandbox isolates the process. The zero value means no sandbox.
	Sandbox Sandbox
}

func (c Config) withDefaults() Config {
	// exec resolves a relative binary path against cmd.Dir, so make both
	// absolute up front.
	if abs, err := filepath.Abs(c.ProjectDir); err == nil {
		c.ProjectDir = abs
	}
	if c.REPLBin != "" && !filepath.IsAbs(c.REPLBin) {
		if abs, err := filepath.Abs(c.REPLBin); err == nil {
			c.REPLBin = abs
		}
	}
	if c.REPLBin == "" {
		c.REPLBin = filepath.Join(c.ProjectDir, ".lake", "packages", "REPL", ".lake", "build", "bin", "repl")
	}
	if c.Imports == nil {
		c.Imports = []string{"Mathlib", "Harness"}
	}
	if c.StartTimeout == 0 {
		c.StartTimeout = 10 * time.Minute
	}
	if c.CmdTimeout == 0 {
		c.CmdTimeout = 60 * time.Second
	}
	return c
}

// ResolveEnv returns the environment `lake env` would set up for the
// project (LEAN_PATH, LEAN_SYSROOT, ...), scrubbed of everything else (see
// ScrubEnv). Running the REPL directly with this environment avoids keeping
// a `lake` process alive per worker.
func ResolveEnv(ctx context.Context, projectDir string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "lake", "env", "env", "-0")
	cmd.Dir = projectDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("lean: lake env in %s: %w: %s", projectDir, err, strings.TrimSpace(stderr.String()))
	}
	var env []string
	for _, kv := range bytes.Split(out, []byte{0}) {
		if len(kv) > 0 {
			env = append(env, string(kv))
		}
	}
	return ScrubEnv(env), nil
}

// REPL drives one leanprover-community/repl process. Methods are safe for
// concurrent use but are serialized; use a Pool for parallelism.
type REPL struct {
	cfg     Config
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	dec     *json.Decoder
	stderr  *tailBuffer
	exited  chan struct{}
	started time.Time

	mu      sync.Mutex
	baseEnv int
	envs    map[string]int // snapshot key -> env id in this process
	cmds    int

	dead atomic.Bool
}

// Start launches a REPL and runs the imports. This is the expensive step
// (seconds to minutes, several GB of RAM).
func Start(ctx context.Context, cfg Config) (*REPL, error) {
	cfg = cfg.withDefaults()
	if cfg.Env == nil {
		env, err := ResolveEnv(ctx, cfg.ProjectDir)
		if err != nil {
			return nil, err
		}
		cfg.Env = env
	}

	cfg.Env = ScrubEnv(cfg.Env) // idempotent; also covers caller-supplied Env
	argv, env, err := cfg.Sandbox.command(cfg, cfg.REPLBin)
	if err != nil {
		return nil, err
	}
	// Not CommandContext: the process outlives ctx, which only bounds startup.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cfg.ProjectDir
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &tailBuffer{max: 64 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lean: start %s: %w", cfg.REPLBin, err)
	}

	r := &REPL{
		cfg:     cfg,
		cmd:     cmd,
		stdin:   stdin,
		dec:     json.NewDecoder(stdout),
		stderr:  stderr,
		exited:  make(chan struct{}),
		started: time.Now(),
		envs:    map[string]int{},
	}
	go func() {
		_ = cmd.Wait()
		r.dead.Store(true)
		close(r.exited)
	}()

	var header strings.Builder
	for _, imp := range cfg.Imports {
		fmt.Fprintf(&header, "import %s\n", imp)
	}
	var resp cmdResponse
	if err := r.roundTrip(ctx, cfg.StartTimeout, map[string]any{"cmd": header.String()}, &resp); err != nil {
		r.Close()
		return nil, fmt.Errorf("lean: importing %v: %w", cfg.Imports, err)
	}
	if errs := errorsOf(resp.Messages); len(errs) > 0 {
		r.Close()
		return nil, fmt.Errorf("lean: importing %v: %s", cfg.Imports, errs[0].Data)
	}
	if resp.Env == nil {
		r.Close()
		return nil, &ReplError{Msg: "import response has no env"}
	}
	r.baseEnv = *resp.Env
	r.cmds = 0
	return r, nil
}

// Close kills the process. Safe to call more than once.
func (r *REPL) Close() {
	r.kill()
	<-r.exited
}

// Dead reports whether the process is gone (crashed, killed or timed out).
func (r *REPL) Dead() bool { return r.dead.Load() }

// Commands is the number of commands run since startup (imports excluded).
func (r *REPL) Commands() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmds
}

// Age is the time since the process started.
func (r *REPL) Age() time.Duration { return time.Since(r.started) }

// RSS returns the resident memory of the REPL process tree in bytes
// (Linux only; 0 elsewhere). Shared, file-backed pages (mapped .olean files)
// are counted in full, so this overstates the per-worker cost.
func (r *REPL) RSS() int64 {
	if r.cmd.Process == nil {
		return 0
	}
	return treeRSS(r.cmd.Process.Pid)
}

// ExecOptions tunes one Exec call.
type ExecOptions struct {
	// AllTactics asks the REPL for every tactic with its goals before it
	// ran; used to show the goal at a failing tactic.
	AllTactics bool
	// Timeout overrides Config.CmdTimeout.
	Timeout time.Duration
}

// Result is the outcome of one command.
type Result struct {
	Messages []Message
	Sorries  []Sorry
	Tactics  []TacticInfo
	// Snapshot is the environment after the command. Build on it only if the
	// command succeeded; extending a failed snapshot carries its errors.
	Snapshot *Snapshot
}

// Exec runs Lean commands on top of base (nil = just the imports).
// If this process has not seen base yet, its chain is replayed first.
func (r *REPL) Exec(ctx context.Context, base *Snapshot, code string, opts ExecOptions) (*Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	env, err := r.envFor(ctx, base)
	if err != nil {
		return nil, err
	}
	req := map[string]any{"cmd": code, "env": env}
	if opts.AllTactics {
		req["allTactics"] = true
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = r.cfg.CmdTimeout
	}
	var resp cmdResponse
	if err := r.roundTrip(ctx, timeout, req, &resp); err != nil {
		return nil, err
	}
	if resp.Env == nil {
		return nil, &ReplError{Msg: "command response has no env"}
	}
	snap := base.Extend(code)
	r.envs[snap.Key()] = *resp.Env
	return &Result{Messages: resp.Messages, Sorries: resp.Sorries, Tactics: resp.Tactics, Snapshot: snap}, nil
}

// envFor returns this process's env id for s, replaying missing links.
// Caller holds r.mu.
func (r *REPL) envFor(ctx context.Context, s *Snapshot) (int, error) {
	if s == nil {
		return r.baseEnv, nil
	}
	if id, ok := r.envs[s.Key()]; ok {
		return id, nil
	}
	parent, err := r.envFor(ctx, s.parent)
	if err != nil {
		return 0, err
	}
	var resp cmdResponse
	if err := r.roundTrip(ctx, r.cfg.CmdTimeout, map[string]any{"cmd": s.code, "env": parent}, &resp); err != nil {
		return 0, fmt.Errorf("lean: replaying snapshot: %w", err)
	}
	if errs := errorsOf(resp.Messages); len(errs) > 0 {
		return 0, fmt.Errorf("lean: replaying snapshot produced error: %s", errs[0].Data)
	}
	if resp.Env == nil {
		return 0, &ReplError{Msg: "replay response has no env"}
	}
	r.envs[s.Key()] = *resp.Env
	return *resp.Env, nil
}

// TacticResult is the outcome of one tactic-mode step.
type TacticResult struct {
	ProofState int       `json:"proofState"`
	Goals      []string  `json:"goals"`
	Messages   []Message `json:"messages"`
}

// RunTactic runs a tactic against a proof state (from Result.Sorries or a
// previous TacticResult). Proof states are local to this process, so the
// caller must hold this REPL (Pool.Acquire) for the whole tactic session.
func (r *REPL) RunTactic(ctx context.Context, proofState int, tactic string, timeout time.Duration) (*TacticResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if timeout == 0 {
		timeout = r.cfg.CmdTimeout
	}
	var resp TacticResult
	if err := r.roundTrip(ctx, timeout, map[string]any{"tactic": tactic, "proofState": proofState}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

type cmdResponse struct {
	Env      *int         `json:"env"`
	Messages []Message    `json:"messages"`
	Sorries  []Sorry      `json:"sorries"`
	Tactics  []TacticInfo `json:"tactics"`
}

// roundTrip sends one request and decodes one response. On timeout or I/O
// failure the process is killed, because the protocol cannot resync.
// Caller holds r.mu (or is Start, before r is shared).
func (r *REPL) roundTrip(ctx context.Context, timeout time.Duration, req, out any) error {
	if r.dead.Load() {
		return ErrDead
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	payload = append(payload, '\n', '\n') // the REPL separates requests by a blank line

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var raw json.RawMessage
	done := make(chan error, 1)
	go func() {
		if _, err := r.stdin.Write(payload); err != nil {
			done <- err
			return
		}
		done <- r.dec.Decode(&raw)
	}()

	select {
	case err := <-done:
		if err != nil {
			r.kill()
			return fmt.Errorf("%w: %v; stderr: %s", ErrDead, err, r.stderr.String())
		}
	case <-cctx.Done():
		r.kill()
		<-done // pipes close when the process dies, unblocking the goroutine
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w after %s", ErrTimeout, timeout)
	}
	r.cmds++

	var perr struct {
		Message *string `json:"message"`
	}
	if json.Unmarshal(raw, &perr) == nil && perr.Message != nil {
		return &ReplError{Msg: *perr.Message}
	}
	return json.Unmarshal(raw, out)
}

func (r *REPL) kill() {
	if r.dead.CompareAndSwap(false, true) && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
}

func errorsOf(ms []Message) []Message {
	var out []Message
	for _, m := range ms {
		if m.Severity == "error" {
			out = append(out, m)
		}
	}
	return out
}

// tailBuffer keeps the last max bytes written (for stderr in error reports).
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
