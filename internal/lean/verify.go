package lean

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Independent final verification.
//
// The in-job check (#harness_axioms inside the REPL) runs in the same Lean
// environment as the job's own code, which could in principle shadow the
// checker or change what a statement means. The Verifier re-establishes the
// result from scratch, in fresh sandboxed processes that share nothing with
// the job:
//
//  1. compile the confirmed statements alone (`theorem n <sig> := sorry`,
//     importing only Mathlib) into QuodSpec.olean;
//  2. compile the job's final Lean file into QuodJob.olean (job code runs
//     here, sandboxed, and can only write into its own temp directory);
//  3. run the toolchain's `leanchecker QuodJob` (kernel re-check of every
//     declaration) and `harness_verify` (each statement is a theorem of the
//     job module whose type is identical to the spec's, with only standard
//     axioms, and every imported module comes from a trusted read-only
//     location). Neither runs any code from the job.
//
// Steps 1–2 and the two checks in step 3 each run in parallel.

// VerifyItem is a statement the job claims to have proved.
type VerifyItem struct {
	Name      string `json:"name"`
	Signature string `json:"signature"`
}

// VerifiedItem is the verdict for one statement.
type VerifiedItem struct {
	Name   string   `json:"name"`
	OK     bool     `json:"ok"`
	Axioms []string `json:"axioms,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// VerifyReport is the outcome of an independent verification.
type VerifyReport struct {
	OK       bool           `json:"ok"`        // every item passed and the kernel re-check passed
	KernelOK bool           `json:"kernel_ok"` // leanchecker accepted the job module
	Items    []VerifiedItem `json:"items"`
	Log      string         `json:"log,omitempty"` // diagnostics when something failed
	Duration time.Duration  `json:"duration"`
}

// Item returns the verdict for name.
func (r *VerifyReport) Item(name string) (VerifiedItem, bool) {
	for _, it := range r.Items {
		if it.Name == name {
			return it, true
		}
	}
	return VerifiedItem{}, false
}

// Verifier runs independent verifications. Safe for concurrent use; at
// most MaxConcurrent run at once (each loads Mathlib several times).
type Verifier struct {
	cfg     Config
	timeout time.Duration
	sem     chan struct{}
}

// VerifierConfig configures a Verifier.
type VerifierConfig struct {
	Config
	Timeout       time.Duration // per step; default 10m
	MaxConcurrent int           // default 1
}

// NewVerifier checks the tools exist and resolves the environment once.
func NewVerifier(ctx context.Context, vc VerifierConfig) (*Verifier, error) {
	cfg := vc.Config.withDefaults()
	if err := cfg.Sandbox.Check(); err != nil {
		return nil, err
	}
	if cfg.Env == nil {
		env, err := ResolveEnv(ctx, cfg.ProjectDir)
		if err != nil {
			return nil, err
		}
		cfg.Env = env
	}
	cfg.Env = ScrubEnv(cfg.Env)
	v := &Verifier{cfg: cfg, timeout: vc.Timeout, sem: make(chan struct{}, max(vc.MaxConcurrent, 1))}
	if v.timeout <= 0 {
		v.timeout = 10 * time.Minute
	}
	for _, bin := range []string{v.leanBin(), v.checkerBin(), v.verifyBin()} {
		if _, err := os.Stat(bin); err != nil {
			return nil, fmt.Errorf("lean: verifier binary missing (run scripts/bootstrap.sh): %w", err)
		}
	}
	return v, nil
}

func (v *Verifier) sysroot() string    { return envValue(v.cfg.Env, "LEAN_SYSROOT") }
func (v *Verifier) leanBin() string    { return filepath.Join(v.sysroot(), "bin", "lean") }
func (v *Verifier) checkerBin() string { return filepath.Join(v.sysroot(), "bin", "leanchecker") }
func (v *Verifier) verifyBin() string {
	return filepath.Join(v.cfg.ProjectDir, ".lake", "build", "bin", "harness_verify")
}

// trustedRoots are directories whose .olean files may be imported by a job.
func (v *Verifier) trustedRoots() string {
	return strings.Join([]string{
		filepath.Join(v.sysroot(), "lib", "lean"),
		filepath.Join(v.cfg.ProjectDir, ".lake", "packages"),
		filepath.Join(v.cfg.ProjectDir, ".lake", "build", "lib", "lean"),
	}, ":")
}

const (
	specModule = "QuodSpec"
	jobModule  = "QuodJob"
)

// SpecCode is the Lean file holding only the confirmed statements, in
// namespace QuodSpec so it can be loaded next to the job module.
func SpecCode(items []VerifyItem) string {
	var b strings.Builder
	b.WriteString("import Mathlib\n\nnamespace " + specModule + "\n")
	for _, it := range items {
		fmt.Fprintf(&b, "\ntheorem %s %s := sorry\n", it.Name, strings.TrimSpace(it.Signature))
	}
	b.WriteString("\nend " + specModule + "\n")
	return b.String()
}

// Verify checks that jobCode proves every item. A returned error means the
// verification could not be carried out (infrastructure), not that a proof
// was rejected; rejections are reported in the VerifyReport.
func (v *Verifier) Verify(ctx context.Context, jobCode string, items []VerifyItem) (*VerifyReport, error) {
	if len(items) == 0 {
		return nil, errors.New("lean: nothing to verify")
	}
	for _, it := range items {
		if err := (Statement{Name: it.Name, Signature: it.Signature}).Validate(); err != nil {
			return nil, err
		}
	}
	select {
	case v.sem <- struct{}{}:
		defer func() { <-v.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	start := time.Now()

	base, err := os.MkdirTemp("", "quod-verify-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(base)
	specDir, jobDir := filepath.Join(base, "spec"), filepath.Join(base, "job")
	for _, d := range []string{specDir, jobDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(specDir, specModule+".lean"), []byte(SpecCode(items)), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(jobDir, jobModule+".lean"), []byte(jobCode), 0o644); err != nil {
		return nil, err
	}

	// Steps 1 and 2: compile spec and job, each in its own sandbox that can
	// write only its own directory.
	var specRes, jobRes runResult
	parallel(
		func() { specRes = v.compile(ctx, specDir, specModule) },
		func() { jobRes = v.compile(ctx, jobDir, jobModule) },
	)
	if specRes.err != nil {
		return nil, fmt.Errorf("lean: compiling statements: %w", specRes.err)
	}
	if specRes.code != 0 {
		return nil, fmt.Errorf("lean: confirmed statements do not compile on their own:\n%s", tail(specRes.out, 2000))
	}
	rep := &VerifyReport{}
	if jobRes.err != nil {
		return nil, fmt.Errorf("lean: compiling job: %w", jobRes.err)
	}
	if jobRes.code != 0 {
		rep.Log = "the final Lean file does not compile on its own:\n" + tail(jobRes.out, 4000)
		for _, it := range items {
			rep.Items = append(rep.Items, VerifiedItem{Name: it.Name, Error: "final file does not compile"})
		}
		rep.Duration = time.Since(start)
		return rep, nil
	}

	// Step 3: kernel re-check, then statement/axiom check, read-only. Run
	// one after the other: each loads Mathlib, and together with the job's
	// worker they can exhaust RAM on a 16 GB machine.
	lp := envValue(v.cfg.Env, "LEAN_PATH")
	kern := v.run(ctx, withEnv(v.cfg.Env, "LEAN_PATH", lp+":"+jobDir), jobDir,
		[]mount{{path: jobDir}}, v.checkerBin(), jobModule)
	if err := crashed("kernel re-check (leanchecker)", kern); err != nil {
		return nil, err
	}
	args := append([]string{v.trustedRoots(), specModule, jobModule}, names(items)...)
	ver := v.run(ctx, withEnv(v.cfg.Env, "LEAN_PATH", lp+":"+specDir+":"+jobDir), base,
		[]mount{{path: specDir}, {path: jobDir}}, v.verifyBin(), args...)
	if err := crashed("statement/axiom check (harness_verify)", ver); err != nil {
		return nil, err
	}
	rep.KernelOK = kern.code == 0
	if !rep.KernelOK {
		rep.Log += "kernel re-check (leanchecker) rejected the job:\n" + tail(kern.out, 4000) + "\n"
	}

	got := map[string]VerifiedItem{}
	jobErr := "" // whole-job rejection (untrusted import, name clash with the spec)
	for _, line := range strings.Split(ver.stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var it VerifiedItem
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, fmt.Errorf("lean: bad checker output %q: %w", line, err)
		}
		if it.Name == "" { // whole-job failure (e.g. untrusted import)
			jobErr = it.Error
			rep.Log += "statement check rejected the job: " + it.Error + "\n"
			continue
		}
		got[it.Name] = it
	}
	if len(got) == 0 && jobErr == "" {
		// Exited without any verdict: a crash, not a rejection.
		return nil, fmt.Errorf("lean: harness_verify gave no verdict (exit %d): %s", ver.code, tail(ver.stderr, 500))
	}
	rep.OK = rep.KernelOK
	for _, it := range items {
		r, ok := got[it.Name]
		if !ok {
			r = VerifiedItem{Name: it.Name, Error: "no verdict from checker"}
			if jobErr != "" {
				r.Error = jobErr
			}
		}
		if !rep.KernelOK && r.OK {
			r.OK, r.Error = false, "kernel re-check failed"
		}
		rep.OK = rep.OK && r.OK
		rep.Items = append(rep.Items, r)
	}
	rep.Duration = time.Since(start)
	return rep, nil
}

// crashed reports infrastructure failures: the step could not run, timed
// out, or was killed by a signal (bwrap reports 128+signal), e.g. by the
// OOM killer. Those must never be mistaken for a rejected proof.
func crashed(step string, r runResult) error {
	if r.err != nil {
		return fmt.Errorf("lean: %s failed to run: %w", step, r.err)
	}
	if r.code >= 128 || r.code < 0 {
		return fmt.Errorf("lean: %s was killed (exit %d, likely out of memory): %s", step, r.code, tail(r.stderr, 300))
	}
	return nil
}

func (v *Verifier) compile(ctx context.Context, dir, module string) runResult {
	src := filepath.Join(dir, module+".lean")
	return v.run(ctx, v.cfg.Env, dir, []mount{{path: dir, writable: true}},
		v.leanBin(), "-R", dir, "-o", filepath.Join(dir, module+".olean"), src)
}

type runResult struct {
	code           int
	stdout, stderr string
	out            string // stdout + stderr
	err            error  // could not run / timed out
}

// run executes a binary in a fresh sandbox (or directly when sandboxing is
// off) and captures its output.
func (v *Verifier) run(ctx context.Context, env []string, cwd string, mounts []mount, bin string, args ...string) runResult {
	argv, outerEnv, err := v.cfg.Sandbox.wrap(v.cfg, env, cwd, mounts, append([]string{bin}, args...))
	if err != nil {
		return runResult{err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = outerEnv
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	r := runResult{stdout: stdout.String(), stderr: stderr.String()}
	r.out = strings.TrimSpace(r.stdout + "\n" + r.stderr)
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		r.err = fmt.Errorf("%s: %w", filepath.Base(bin), ctx.Err())
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	case err != nil:
		r.err = err
	}
	return r
}

func parallel(fns ...func()) {
	var wg sync.WaitGroup
	for _, f := range fns {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	wg.Wait()
}

func withEnv(env []string, key, val string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+val)
}

func names(items []VerifyItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Name
	}
	return out
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
