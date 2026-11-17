package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ModelSpec selects a model and its settings for one kind of task.
type ModelSpec struct {
	Model     string
	Effort    Effort
	MaxTokens int
}

// Models assigns a model to each task.
type Models struct {
	Split     ModelSpec
	Formalize ModelSpec
	Back      ModelSpec // back-translation
	Prove     ModelSpec
	Explain   ModelSpec
}

// DefaultModels uses deepseek-flash everywhere (cheap); switch Prove to
// ModelPro for harder problems.
func DefaultModels() Models {
	return Models{
		Split:     ModelSpec{Model: ModelFlash, Effort: EffortNone, MaxTokens: 4096},
		Formalize: ModelSpec{Model: ModelFlash, Effort: EffortHigh},
		Back:      ModelSpec{Model: ModelFlash, Effort: EffortNone, MaxTokens: 1024},
		Prove:     ModelSpec{Model: ModelFlash, Effort: EffortHigh},
		Explain:   ModelSpec{Model: ModelFlash, Effort: EffortNone, MaxTokens: 2048},
	}
}

// ErrBudget is returned when the spending limit has been reached.
var ErrBudget = errors.New("llm: budget exhausted")

// Options configures an Agent.
type Options struct {
	Models      Models
	LeanVersion string  // shown to the model, e.g. "Lean v4.36.0, Mathlib v4.36.0"
	BudgetUSD   float64 // 0 = unlimited; checked before each call
	// MaxFeedbackRounds bounds how many (proof, feedback) pairs a proof
	// session keeps in context; older rounds are summarized away. Default 3.
	MaxFeedbackRounds int
}

// Agent runs the prover's LLM tasks and tracks total spend. Safe for
// concurrent use.
type Agent struct {
	c    Chatter
	opts Options

	mu    sync.Mutex
	spent float64
	calls int
}

// NewAgent wraps a Chatter (usually *Client).
func NewAgent(c Chatter, opts Options) *Agent {
	if opts.Models == (Models{}) {
		opts.Models = DefaultModels()
	}
	if opts.LeanVersion == "" {
		opts.LeanVersion = "Lean 4 with a recent Mathlib"
	}
	if opts.MaxFeedbackRounds <= 0 {
		opts.MaxFeedbackRounds = 3
	}
	return &Agent{c: c, opts: opts}
}

// Spent returns the estimated USD spent and the number of calls.
func (a *Agent) Spent() (usd float64, calls int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.spent, a.calls
}

func (a *Agent) call(ctx context.Context, spec ModelSpec, asJSON bool, msgs ...Message) (*Response, error) {
	a.mu.Lock()
	if a.opts.BudgetUSD > 0 && a.spent >= a.opts.BudgetUSD {
		a.mu.Unlock()
		return nil, fmt.Errorf("%w ($%.4f of $%.2f)", ErrBudget, a.spent, a.opts.BudgetUSD)
	}
	a.mu.Unlock()

	resp, err := a.c.Chat(ctx, Request{Model: spec.Model, Effort: spec.Effort, MaxTokens: spec.MaxTokens, JSON: asJSON, Messages: msgs})
	if resp != nil {
		a.mu.Lock()
		a.spent += resp.CostUSD
		a.calls++
		a.mu.Unlock()
	}
	return resp, err
}

func (a *Agent) system(tmpl string) Message {
	return System(strings.ReplaceAll(tmpl, "{{LEAN_VERSION}}", a.opts.LeanVersion))
}

// Lemma is a previously formalized (and possibly proved) statement that
// later statements may reference.
type Lemma struct {
	Name      string
	Signature string
	Text      string // original natural-language statement
	Proved    bool
}

func renderLemmas(header string, ls []Lemma, onlyProved bool) string {
	var b strings.Builder
	for _, l := range ls {
		if onlyProved && !l.Proved {
			continue
		}
		if b.Len() == 0 {
			b.WriteString(header + "\n")
		}
		fmt.Fprintf(&b, "theorem %s %s\n", l.Name, l.Signature)
		if l.Text != "" {
			fmt.Fprintf(&b, "  -- %s\n", oneLine(l.Text))
		}
	}
	return b.String()
}

// SplitStatements splits free-form input into individual statements.
func (a *Agent) SplitStatements(ctx context.Context, input string) ([]string, error) {
	resp, err := a.call(ctx, a.opts.Models.Split, true, System(splitSystem), User(input))
	if err != nil {
		return nil, err
	}
	var out struct {
		Statements []string `json:"statements"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Content)), &out); err != nil {
		return nil, fmt.Errorf("llm: split: bad json %q: %w", truncate(resp.Content, 200), err)
	}
	var stmts []string
	for _, s := range out.Statements {
		if s = strings.TrimSpace(s); s != "" {
			stmts = append(stmts, s)
		}
	}
	if len(stmts) == 0 {
		return nil, errors.New("llm: split: no statements found")
	}
	return stmts, nil
}

// FormalizeInput describes one formalization request.
type FormalizeInput struct {
	Name      string  // theorem name to use (server-chosen)
	Statement string  // natural language
	Context   []Lemma // earlier statements it may refer to
	// Previous, if set, is a failed earlier formalization to repair.
	Previous *FormalizeRetry
}

// FormalizeRetry carries a rejected formalization and why it was rejected
// (Lean errors, or the user's correction).
type FormalizeRetry struct {
	Signature string
	Problem   string
}

// Formalization is a proposed Lean statement.
type Formalization struct {
	Signature string // "<binders> : <prop>", for lean.Statement
	Notes     string // the model's interpretation choices
	Response  *Response
}

// Formalize translates a natural-language statement into a Lean signature.
func (a *Agent) Formalize(ctx context.Context, in FormalizeInput) (*Formalization, error) {
	var b strings.Builder
	if c := renderLemmas("Previously formalized statements (you may refer to them by name):", in.Context, false); c != "" {
		b.WriteString(c + "\n")
	}
	fmt.Fprintf(&b, "Theorem name: %s\n\nStatement:\n%s\n", in.Name, in.Statement)
	msgs := []Message{a.system(formalizeSystem), User(b.String())}
	if p := in.Previous; p != nil {
		msgs = append(msgs,
			Assistant(fmt.Sprintf("```lean\ntheorem %s %s := by sorry\n```", in.Name, p.Signature)),
			User("That formalization was rejected:\n"+p.Problem+"\n\nWrite a corrected formalization in the same format."))
	}
	resp, err := a.call(ctx, a.opts.Models.Formalize, false, msgs...)
	if err != nil {
		return nil, err
	}
	sig, ok := ExtractSignature(resp.Content)
	if !ok || sig == "" {
		return nil, fmt.Errorf("llm: formalize: no theorem found in reply %q", truncate(resp.Content, 300))
	}
	return &Formalization{Signature: sig, Notes: Notes(resp.Content), Response: resp}, nil
}

// BackTranslate states in plain language what a Lean statement asserts,
// without seeing the original text, so the user can check faithfulness.
func (a *Agent) BackTranslate(ctx context.Context, name, signature string, lemmas []Lemma) (string, error) {
	var b strings.Builder
	if c := renderLemmas("Earlier statements referenced:", lemmas, false); c != "" {
		b.WriteString(c + "\n")
	}
	fmt.Fprintf(&b, "```lean\ntheorem %s %s\n```", name, signature)
	resp, err := a.call(ctx, a.opts.Models.Back, false, System(backTranslateSystem), User(b.String()))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

// ProveInput describes the theorem to prove.
type ProveInput struct {
	Name      string
	Signature string
	Text      string  // natural-language statement (helps the model)
	Context   []Lemma // only Proved ones are offered for use
}

// ProofSession is a multi-turn conversation: propose a proof, receive
// compiler feedback, propose a fix. Not safe for concurrent use; run
// several sessions for parallel attempts.
type ProofSession struct {
	a       *Agent
	intro   Message
	rounds  []round // completed (proof, feedback) pairs
	pending string  // last proposed reply awaiting feedback
	dropped int     // rounds summarized away
}

type round struct{ reply, feedback string }

// NewProofSession starts a proof conversation.
func (a *Agent) NewProofSession(in ProveInput) *ProofSession {
	var b strings.Builder
	if c := renderLemmas("Previously proved theorems you may use:", in.Context, true); c != "" {
		b.WriteString(c + "\n")
	}
	if in.Text != "" {
		fmt.Fprintf(&b, "Informal statement: %s\n\n", oneLine(in.Text))
	}
	fmt.Fprintf(&b, "Prove:\n```lean\ntheorem %s %s := by\n  ...\n```\nReply with the tactic script only.", in.Name, in.Signature)
	return &ProofSession{a: a, intro: User(b.String())}
}

// Next asks for a proof (the first one, or a fix after Feedback).
func (s *ProofSession) Next(ctx context.Context) (body string, resp *Response, err error) {
	msgs := []Message{s.a.system(proveSystem), s.intro}
	if s.dropped > 0 {
		msgs = append(msgs, Assistant("(earlier attempts omitted)"),
			User(fmt.Sprintf("%d earlier attempt(s) failed and are omitted. Continue.", s.dropped)))
	}
	for _, r := range s.rounds {
		msgs = append(msgs, Assistant(r.reply), User(r.feedback))
	}
	resp, err = s.a.call(ctx, s.a.opts.Models.Prove, false, msgs...)
	if err != nil {
		return "", resp, err
	}
	body = ExtractProofBody(resp.Content)
	// Keep only the code in history: shorter context, and prose from a
	// failed attempt tends to mislead later ones.
	s.pending = "```lean\n" + body + "\n```"
	return body, resp, nil
}

// Feedback records why the last proof failed; call Next for a fix.
func (s *ProofSession) Feedback(feedback string) {
	if s.pending == "" {
		return
	}
	s.rounds = append(s.rounds, round{reply: s.pending, feedback: feedback +
		"\n\nFix the proof. Reply with the complete corrected tactic script in one lean code block."})
	s.pending = ""
	if over := len(s.rounds) - s.a.opts.MaxFeedbackRounds; over > 0 {
		s.rounds = s.rounds[over:]
		s.dropped += over
	}
}

// Rounds is the number of failed attempts recorded so far.
func (s *ProofSession) Rounds() int { return s.dropped + len(s.rounds) }

// Explain describes a verified proof in plain language.
func (a *Agent) Explain(ctx context.Context, text, name, signature, proof string) (string, error) {
	u := fmt.Sprintf("Statement: %s\n\nVerified Lean proof:\n```lean\ntheorem %s %s := by\n%s\n```", text, name, signature, indentLines(proof, "  "))
	resp, err := a.call(ctx, a.opts.Models.Explain, false, System(explainSystem), User(u))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func indentLines(s, p string) string {
	ls := strings.Split(s, "\n")
	for i := range ls {
		ls[i] = p + ls[i]
	}
	return strings.Join(ls, "\n")
}
