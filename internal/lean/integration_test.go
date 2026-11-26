package lean

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Integration tests against the real REPL. They import Mathlib once (slow,
// several GB of RAM), so they only run when LEAN_INTEGRATION=1:
//
//	LEAN_INTEGRATION=1 go test -v -timeout 30m ./internal/lean/
func TestIntegration(t *testing.T) {
	if os.Getenv("LEAN_INTEGRATION") != "1" {
		t.Skip("set LEAN_INTEGRATION=1 to run (needs scripts/bootstrap.sh)")
	}
	_, file, _, _ := runtime.Caller(0)
	project := filepath.Join(filepath.Dir(file), "..", "..", "lean_env")

	ctx := context.Background()
	t0 := time.Now()
	r, err := Start(ctx, Config{ProjectDir: project, CmdTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	t.Logf("startup (import Mathlib) took %s, RSS %d MB", time.Since(t0).Round(time.Millisecond), r.RSS()>>20)

	s1 := Statement{Name: "stmt_1", Signature: "(x : ℝ) (h : 0 < x) : 0 < x ^ 2 + x"}
	var snap1 *Snapshot

	t.Run("statement elaborates", func(t *testing.T) {
		sc, err := CheckStatement(ctx, r, nil, s1)
		if err != nil || !sc.OK {
			t.Fatalf("err=%v check=%+v", err, sc)
		}
	})

	t.Run("ill-typed statement rejected", func(t *testing.T) {
		sc, err := CheckStatement(ctx, r, nil, Statement{Name: "bad_stmt", Signature: "(x : ℝ) : x + \"a\" = 1"})
		if err != nil || sc.OK {
			t.Fatalf("err=%v check=%+v", err, sc)
		}
		t.Logf("statement error: %s", sc.Errors[0].Data)
	})

	t.Run("valid proof verified", func(t *testing.T) {
		t1 := time.Now()
		o, err := CheckProof(ctx, r, nil, s1, "positivity")
		if err != nil {
			t.Fatal(err)
		}
		if !o.Verdict.Verified {
			t.Fatalf("not verified:\n%s", RenderFeedback(o, FeedbackOptions{}))
		}
		t.Logf("checked in %s: %s", time.Since(t1).Round(time.Millisecond), o.Verdict.Axioms)
		snap1 = o.Snapshot()
	})

	s2 := Statement{Name: "stmt_2", Signature: "(y : ℝ) (h : 0 < y) : 0 < (y ^ 2 + y) * 2"}
	body2 := "have := stmt_1 y h\npositivity"

	t.Run("later statement uses earlier one", func(t *testing.T) {
		o, err := CheckProof(ctx, r, snap1, s2, body2)
		if err != nil {
			t.Fatal(err)
		}
		if !o.Verdict.Verified {
			t.Fatalf("not verified:\n%s", RenderFeedback(o, FeedbackOptions{}))
		}
	})

	t.Run("snapshot replays on a fresh env cache", func(t *testing.T) {
		r.mu.Lock()
		r.envs = map[string]int{} // simulate a different worker
		r.mu.Unlock()
		o, err := CheckProof(ctx, r, snap1, s2, body2)
		if err != nil || !o.Verdict.Verified {
			t.Fatalf("err=%v verdict=%+v", err, o.Verdict)
		}
	})

	t.Run("wrong proof gives structured feedback", func(t *testing.T) {
		s := Statement{Name: "stmt_3", Signature: "(a b : ℕ) (h : a < b) : a + 1 ≤ b"}
		o, err := CheckProof(ctx, r, nil, s, "have := Nat.no_such_lemma h\nlinarith")
		if err != nil {
			t.Fatal(err)
		}
		if o.Verdict.Verified {
			t.Fatal("bogus proof verified")
		}
		fb := RenderFeedback(o, FeedbackOptions{})
		t.Logf("feedback:\n%s", fb)
		if !strings.Contains(fb, string(KindUnknownIdent)) {
			t.Errorf("expected %s in feedback", KindUnknownIdent)
		}
	})

	t.Run("forbidden syntax never reaches lean", func(t *testing.T) {
		before := r.Commands()
		o, err := CheckProof(ctx, r, nil, s1, "sorry")
		if err != nil || o.Verdict.Verified || o.Verdict.Errors[0].Kind != KindForbidden || r.Commands() != before {
			t.Fatalf("err=%v verdict=%+v", err, o.Verdict)
		}
	})

	t.Run("tactic mode", func(t *testing.T) {
		sc, err := CheckStatement(ctx, r, nil, Statement{Name: "stmt_4", Signature: "(n : ℕ) : n + 0 = n"})
		if err != nil || !sc.OK {
			t.Fatalf("err=%v check=%+v", err, sc)
		}
		res, err := r.Exec(ctx, nil, sc.Code, ExecOptions{})
		if err != nil || len(res.Sorries) != 1 {
			t.Fatalf("err=%v sorries=%+v", err, res)
		}
		tr, err := r.RunTactic(ctx, res.Sorries[0].ProofState, "simp", 0)
		if err != nil || len(tr.Goals) != 0 {
			t.Fatalf("err=%v result=%+v", err, tr)
		}
	})

	t.Run("explore proof reports sorry goals, never verifies", func(t *testing.T) {
		s := Statement{Name: "stmt_x", Signature: "(n : ℕ) : n ^ 2 + 0 = n * n"}
		o, err := ExploreProof(ctx, r, nil, s, "rw [Nat.add_zero]\nsorry")
		if err != nil {
			t.Fatal(err)
		}
		if o.Verdict.Verified || o.Result == nil || len(o.Result.Sorries) != 1 || !strings.Contains(o.Result.Sorries[0].Goal, "n ^ 2 = n * n") {
			t.Fatalf("verdict=%+v sorries=%+v", o.Verdict, o.Result)
		}
		if o2, _ := CheckProof(ctx, r, nil, s, "sorry"); o2.Verdict.Errors[0].Kind != KindForbidden {
			t.Errorf("CheckProof must still reject sorry up front: %+v", o2.Verdict)
		}
		if o3, _ := ExploreProof(ctx, r, nil, s, "sorry\ntheorem evil : False := sorry"); o3.Verdict.Errors[0].Kind != KindForbidden {
			t.Errorf("explore must still reject commands: %+v", o3.Verdict)
		}
	})

	// Must be last: a timeout kills the process.
	t.Run("wall-clock timeout kills the process", func(t *testing.T) {
		_, err := r.Exec(ctx, nil, "example : True := by\n  sleep 10000\n  trivial", ExecOptions{Timeout: time.Second})
		if !errors.Is(err, ErrTimeout) || !r.Dead() {
			t.Fatalf("err=%v dead=%v", err, r.Dead())
		}
		if _, err := r.Exec(ctx, nil, "example : True := trivial", ExecOptions{}); !errors.Is(err, ErrDead) {
			t.Fatalf("after timeout: %v", err)
		}
	})
}
