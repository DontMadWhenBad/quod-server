package lean

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestScrubEnv(t *testing.T) {
	in := []string{
		"DEEPSEEK_API_KEY=sk-secret", "API_TOKEN=tok", "SSH_AUTH_SOCK=/run/agent", "HOME=/home/u",
		"LEAN_SYSROOT=/tc", "LEAN_PATH=/p/a:/p/b", "LEAN_SRC_PATH=/s",
		"PATH=/home/u/bin:/usr/bin", "LD_LIBRARY_PATH=/tc/lib:/home/u/project",
	}
	out := ScrubEnv(in)
	joined := strings.Join(out, "\n")
	for _, bad := range []string{"sk-secret", "API_TOKEN", "SSH_AUTH_SOCK", "/home/u"} {
		if strings.Contains(joined, bad) {
			t.Errorf("scrubbed env still contains %q:\n%s", bad, joined)
		}
	}
	for _, want := range []string{"LEAN_PATH=/p/a:/p/b", "LEAN_SYSROOT=/tc", "HOME=/tmp", "PATH=/tc/bin:/usr/bin", "LD_LIBRARY_PATH=/tc/lib/lean:/tc/lib"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q:\n%s", want, joined)
		}
	}
	if again := ScrubEnv(out); strings.Join(again, "\n") != joined {
		t.Error("ScrubEnv must be idempotent")
	}
}

func TestSandboxCommand(t *testing.T) {
	cfg := Config{ProjectDir: "/proj", Env: []string{"LEAN_SYSROOT=/tc"}}
	t.Setenv("DEEPSEEK_API_KEY", "sk-secret")
	argv, env, err := DefaultSandbox().command(cfg, "/proj/repl")
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k != "PATH" && k != "XDG_RUNTIME_DIR" && k != "DBUS_SESSION_BUS_ADDRESS" {
			t.Errorf("launcher env has %q", kv)
		}
	}
	a := strings.Join(argv, " ")
	for _, want := range []string{
		"systemd-run --user --scope", "MemoryMax=10G", "MemorySwapMax=0", "TasksMax=256",
		"bwrap --unshare-all --die-with-parent --new-session",
		"--ro-bind /tc /tc", "--ro-bind /proj /proj", "--tmpfs /tmp", "--clearenv --setenv LEAN_SYSROOT /tc",
		"--chdir /proj -- /proj/repl",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("argv missing %q:\n%s", want, a)
		}
	}
	if strings.Contains(a, "--bind ") || strings.Contains(a, "--share-net") {
		t.Errorf("argv has a writable bind or network:\n%s", a)
	}
	if strings.Contains(a, "sk-secret") {
		t.Errorf("secret in argv:\n%s", a)
	}
	if argv, _, _ := (Sandbox{Kind: SandboxNone}).command(cfg, "/proj/repl"); len(argv) != 1 {
		t.Errorf("none sandbox argv = %v", argv)
	}
}

// TestSandboxEscapes runs hostile Lean code directly (bypassing the
// forbidden-syntax filter, as a real escape would) inside the bwrap sandbox
// and checks every channel is closed.
//
//	LEAN_INTEGRATION=1 go test -v -run TestSandbox ./internal/lean/
//	LEAN_SANDBOX_MEMTEST=1 ...   also runs the memory-bomb test (allocates up to MemoryMax)
func TestSandboxEscapes(t *testing.T) {
	if os.Getenv("LEAN_INTEGRATION") != "1" {
		t.Skip("set LEAN_INTEGRATION=1 to run (needs scripts/bootstrap.sh, bwrap, systemd user session)")
	}
	_, file, _, _ := runtime.Caller(0)
	repo, _ := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	project := filepath.Join(repo, "lean_env")
	sb := Sandbox{Kind: SandboxBwrap, MemoryMax: "10G", TasksMax: 128}
	if err := sb.Check(); err != nil {
		t.Fatal(err)
	}

	// A secret in the server's environment must not reach Lean.
	const secret = "sk-test-must-not-leak"
	t.Setenv("DEEPSEEK_API_KEY", secret)

	ctx := context.Background()
	cfg := Config{ProjectDir: project, Sandbox: sb, CmdTimeout: time.Minute}
	start := func(t *testing.T) *REPL {
		t.Helper()
		t0 := time.Now()
		r, err := Start(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("sandboxed worker up in %s (tree RSS %d MB)", time.Since(t0).Round(time.Millisecond), r.RSS()>>20)
		return r
	}
	r := start(t)
	defer r.Close()

	// eval runs code and returns all message texts joined, plus whether any
	// message was an error.
	eval := func(t *testing.T, code string) (string, bool) {
		t.Helper()
		res, err := r.Exec(ctx, nil, code, ExecOptions{})
		if err != nil {
			t.Fatalf("exec %q: %v", code, err)
		}
		var b strings.Builder
		hasErr := false
		for _, m := range res.Messages {
			b.WriteString(m.Severity + ": " + m.Data + "\n")
			hasErr = hasErr || m.Severity == "error"
		}
		return b.String(), hasErr
	}
	mustFail := func(t *testing.T, code string) string {
		t.Helper()
		out, hasErr := eval(t, code)
		if !hasErr {
			t.Errorf("expected failure, got:\n%s", out)
		}
		return out
	}

	t.Run("proofs still work", func(t *testing.T) {
		o, err := CheckProof(ctx, r, nil, Statement{Name: "sb_ok", Signature: "(n : ℕ) : n + 0 = n"}, "simp")
		if err != nil || !o.Verdict.Verified {
			t.Fatalf("err=%v verdict=%+v", err, o.Verdict)
		}
	})

	t.Run("no secrets in environment", func(t *testing.T) {
		out, _ := eval(t, `#eval IO.getEnv "DEEPSEEK_API_KEY"`)
		if strings.Contains(out, secret) || !strings.Contains(out, "none") {
			t.Errorf("env leak: %s", out)
		}
		// HOME is the private /tmp. (PATH legitimately names the read-only
		// toolchain, which may live under the host home directory.)
		out, _ = eval(t, `#eval IO.getEnv "HOME"`)
		if !strings.Contains(out, `some "/tmp"`) {
			t.Errorf("HOME should be /tmp: %s", out)
		}
		// Every process visible in the sandbox (pid 1 is bwrap's init) has a
		// clean environment too.
		out, _ = eval(t, `#eval IO.FS.readFile "/proc/1/environ"`)
		if strings.Contains(out, secret) {
			t.Errorf("secret visible via /proc/1/environ")
		}
		// The server's own environment via /proc is not reachable either.
		mustFail(t, fmt.Sprintf(`#eval IO.FS.readFile "/proc/%d/environ"`, os.Getpid()))
	})

	t.Run("host files invisible", func(t *testing.T) {
		mustFail(t, fmt.Sprintf(`#eval IO.FS.readFile %q`, filepath.Join(repo, "go.mod")))
		mustFail(t, fmt.Sprintf(`#eval IO.FS.readFile %q`, filepath.Join(repo, ".env")))
		if home, err := os.UserHomeDir(); err == nil {
			mustFail(t, fmt.Sprintf(`#eval System.FilePath.readDir %q`, filepath.Join(home, ".ssh")))
		}
	})

	t.Run("project is read-only", func(t *testing.T) {
		target := filepath.Join(project, ".lake", "build", "lib", "lean", "pwned.txt")
		out := mustFail(t, fmt.Sprintf(`#eval IO.FS.writeFile %q "x"`, target))
		if _, err := os.Stat(target); err == nil {
			os.Remove(target)
			t.Fatalf("write reached the host: %s", out)
		}
		mustFail(t, fmt.Sprintf(`#eval IO.FS.writeFile %q "x"`, filepath.Join(project, ".lake", "build", "lib", "lean", "Harness.olean")))
	})

	t.Run("no network", func(t *testing.T) {
		if _, err := exec.LookPath("curl"); err != nil {
			t.Skip("curl not installed")
		}
		out, hasErr := eval(t, `#eval do
  let o ← IO.Process.output {cmd := "curl", args := #["-sS", "-m", "5", "http://1.1.1.1"]}
  return o.exitCode`)
		if hasErr {
			return // spawning itself failed: also no network
		}
		code := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "info:"))
		if code == "0" {
			t.Errorf("network reachable: %s", out)
		}
	})

	t.Run("task limit", func(t *testing.T) {
		out, _ := eval(t, `#eval do
  let mut n := 0
  for _ in [0:400] do
    try
      let _ ← IO.Process.spawn {cmd := "sleep", args := #["60"]}
      n := n + 1
    catch _ => break
  return n`)
		f := strings.Fields(out)
		n, err := strconv.Atoi(f[len(f)-1])
		if err != nil || n >= 400 {
			t.Errorf("spawned %v processes, limit not enforced: %s", n, out)
		} else {
			t.Logf("task limit stopped spawning after %d processes", n)
		}
	})

	t.Run("no state shared between workers", func(t *testing.T) {
		if _, hasErr := eval(t, `#eval IO.FS.writeFile "/tmp/job-a-marker" "secret from job A"`); hasErr {
			t.Fatal("private /tmp should be writable")
		}
		if _, err := os.Stat("/tmp/job-a-marker"); err == nil {
			t.Error("sandbox /tmp leaked to host /tmp")
		}
		r2 := start(t)
		defer r2.Close()
		res, err := r2.Exec(ctx, nil, `#eval IO.FS.readFile "/tmp/job-a-marker"`, ExecOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range res.Messages {
			if strings.Contains(m.Data, "secret from job A") {
				t.Fatalf("job B read job A's file: %s", m.Data)
			}
		}
	})

	t.Run("lease discards workers between jobs", func(t *testing.T) {
		pool, err := NewPool(ctx, PoolConfig{Config: cfg, Size: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		l1 := pool.Lease()
		w1, err := l1.REPL(ctx)
		if err != nil {
			t.Fatal(err)
		}
		l1.Close()
		if !w1.Dead() {
			t.Fatal("job A's worker still alive after its lease closed")
		}
		l2 := pool.Lease()
		defer l2.Close()
		w2, err := l2.REPL(ctx) // waits for the replacement
		if err != nil {
			t.Fatal(err)
		}
		if w2 == w1 || w2.cmd.Process.Pid == w1.cmd.Process.Pid {
			t.Fatal("job B got job A's worker")
		}
	})

	t.Run("memory bomb kills only the worker", func(t *testing.T) {
		if os.Getenv("LEAN_SANDBOX_MEMTEST") != "1" {
			t.Skip("set LEAN_SANDBOX_MEMTEST=1 (allocates up to the 10G limit)")
		}
		rb := start(t)
		defer rb.Close()
		_, err := rb.Exec(ctx, nil, `#eval (Array.replicate 3000000000 (7 : UInt64)).size`, ExecOptions{Timeout: 3 * time.Minute})
		if err == nil || !rb.Dead() {
			t.Fatalf("expected the worker to be killed, err=%v dead=%v", err, rb.Dead())
		}
		if !errors.Is(err, ErrDead) && !errors.Is(err, ErrTimeout) {
			t.Fatalf("unexpected error: %v", err)
		}
		t.Logf("worker killed: %v", err)
	})
}
