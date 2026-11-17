package llm

import (
	"context"
	"os"
	"testing"
	"time"

	"mathprover/internal/dotenv"
)

// Live test against the real API (costs a fraction of a cent):
//
//	DEEPSEEK_LIVE=1 go test -v -run TestLive ./internal/llm/
//
// Reads DEEPSEEK_API_KEY from the environment or ../../.env.
func TestLive(t *testing.T) {
	if os.Getenv("DEEPSEEK_LIVE") != "1" {
		t.Skip("set DEEPSEEK_LIVE=1 to call the real API")
	}
	if err := dotenv.Load("../../.env"); err != nil {
		t.Fatal(err)
	}
	c, err := NewClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	a := NewAgent(c, Options{LeanVersion: "Lean v4.36.0, Mathlib v4.36.0", BudgetUSD: 0.05})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	f, err := a.Formalize(ctx, FormalizeInput{Name: "stmt_1", Statement: "For every natural number n, n^2 + n is even."})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("signature: %s\nnotes: %s\ntokens: %+v reasoning=%d cost=$%.5f latency=%s",
		f.Signature, f.Notes, f.Response.Usage, f.Response.Usage.ReasoningTokens(), f.Response.CostUSD, f.Response.Latency)

	back, err := a.BackTranslate(ctx, "stmt_1", f.Signature, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("back-translation: %s", back)

	s := a.NewProofSession(ProveInput{Name: "stmt_1", Signature: f.Signature, Text: "n^2 + n is even"})
	body, resp, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("proof (%s, %d reasoning tokens):\n%s", resp.Latency.Round(time.Millisecond), resp.Usage.ReasoningTokens(), body)
	usd, calls := a.Spent()
	t.Logf("total: $%.5f over %d calls", usd, calls)
}
