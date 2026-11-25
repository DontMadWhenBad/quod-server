package lean

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSpecCode(t *testing.T) {
	got := SpecCode([]VerifyItem{{Name: "stmt_1", Signature: " (n : ℕ) : n = n "}, {Name: "stmt_2", Signature: ": True"}})
	want := "import Mathlib\n\nnamespace QuodSpec\n\ntheorem stmt_1 (n : ℕ) : n = n := sorry\n\ntheorem stmt_2 : True := sorry\n\nend QuodSpec\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestVerifier runs the independent verifier on honest and cheating jobs.
// The cheats bypass the forbidden-syntax filter on purpose: the verifier
// must catch them on its own.
//
//	LEAN_INTEGRATION=1 go test -v -run TestVerifier ./internal/lean/
func TestVerifier(t *testing.T) {
	if os.Getenv("LEAN_INTEGRATION") != "1" {
		t.Skip("set LEAN_INTEGRATION=1 to run (needs scripts/bootstrap.sh)")
	}
	_, file, _, _ := runtime.Caller(0)
	project, _ := filepath.Abs(filepath.Join(filepath.Dir(file), "..", "..", "lean_env"))
	ctx := context.Background()
	v, err := NewVerifier(ctx, VerifierConfig{
		Config:        Config{ProjectDir: project, Sandbox: Sandbox{Kind: SandboxBwrap, MemoryMax: "10G", TasksMax: 256}},
		MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	items := []VerifyItem{
		{Name: "stmt_1", Signature: "(n : ℕ) : n + 0 = n"},
		{Name: "stmt_2", Signature: "(x : ℝ) : 0 ≤ x ^ 2"},
	}
	const header = "import Mathlib\nimport Harness\n\n"
	const stmt2 = "\n\ntheorem stmt_2 (x : ℝ) : 0 ≤ x ^ 2 := by\n  positivity\n"

	cases := []struct {
		name     string
		code     string
		wantOK   bool
		badStmt1 string // substring expected in stmt_1's error
	}{
		{"honest", header + "theorem stmt_1 (n : ℕ) : n + 0 = n := by\n  simp\n\n#harness_axioms stmt_1" + stmt2, true, ""},
		{"sorry", header + "theorem stmt_1 (n : ℕ) : n + 0 = n := by\n  sorry" + stmt2, false, "sorryAx"},
		{"axiom", header + "axiom cheat : False\n\ntheorem stmt_1 (n : ℕ) : n + 0 = n := cheat.elim" + stmt2, false, "cheat"},
		{"wrong statement", header + "theorem stmt_1 (n : ℕ) : n = n := rfl" + stmt2, false, "type differs"},
		// A fake `+` makes the statement trivially true; same text, different meaning.
		{"redefined meaning", header + "instance (priority := high) fakeAdd : Add ℕ := ⟨fun a _ => a⟩\n\ntheorem stmt_1 (n : ℕ) : n + 0 = n := rfl" + stmt2, false, "type differs"},
		{"shadowed in-job checker", header +
			"macro \"#harness_axioms \" i:ident : command => `(#eval IO.println (\"HARNESS_AXIOMS_OK \" ++ toString $(Lean.quote i.getId) ++ \": []\"))\n\n" +
			"theorem stmt_1 (n : ℕ) : n + 0 = n := by\n  sorry\n\n#harness_axioms stmt_1" + stmt2, false, "sorryAx"},
		{"clashes with spec names", header + "theorem QuodSpec.stmt_1 (n : ℕ) : n + 0 = n := by\n  sorry\n\ntheorem stmt_1 (n : ℕ) : n + 0 = n := by\n  simp" + stmt2, false, ""},
		{"does not compile", header + "theorem stmt_1 (n : ℕ) : n + 0 = n := by\n  exact foo" + stmt2, false, "does not compile"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t0 := time.Now()
			rep, err := v.Verify(ctx, c.code, items)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s in %s: ok=%v kernel=%v items=%+v", c.name, time.Since(t0).Round(100*time.Millisecond), rep.OK, rep.KernelOK, rep.Items)
			if rep.OK != c.wantOK {
				t.Fatalf("OK = %v, want %v; log:\n%s", rep.OK, c.wantOK, rep.Log)
			}
			s1, _ := rep.Item("stmt_1")
			s2, _ := rep.Item("stmt_2")
			if c.wantOK && (!s1.OK || !s2.OK) {
				t.Errorf("items: %+v", rep.Items)
			}
			if !c.wantOK {
				if s1.OK {
					t.Errorf("stmt_1 accepted: %+v", s1)
				}
				if c.badStmt1 != "" && !strings.Contains(s1.Error+strings.Join(s1.Axioms, ","), c.badStmt1) {
					t.Errorf("stmt_1 error %q (axioms %v), want mention of %q", s1.Error, s1.Axioms, c.badStmt1)
				}
				if c.name != "does not compile" && c.name != "clashes with spec names" && !s2.OK {
					t.Errorf("honest stmt_2 rejected: %+v", s2)
				}
			}
		})
	}
}
