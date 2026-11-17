# quod server

Natural-language math statements in, machine-checked Lean 4 proofs out.
An LLM formalizes each statement and writes proofs; the server
compiles them against Mathlib and feeds compiler errors back until a proof
verifies. Clients only see results.

```
statement(s) ─► split ─► formalize ─► Lean checks statement ─► back-translate ─► (confirm)
                                                                                     │
          explanation ◄─ verified ◄─ Lean checks proof ◄─ LLM writes proof ◄─────────┘
                                          │  errors (structured, with goals)
                                          └──────────► LLM fixes proof (repeat)
```

## Setup

```sh
# 1. Lean toolchain, Mathlib (+ prebuilt cache), REPL, axiom checker, smoke tests
scripts/bootstrap.sh

# 2. API key
cp .env.example .env    # then set DEEPSEEK_API_KEY=...

# 3. Build
go build -o bin/prove ./cmd/prove
```

`bootstrap.sh` is idempotent; rerun it after changing versions. Lean,
Mathlib and REPL are pinned to the same release in `lean_env/lakefile.toml`
and `lean_env/lean-toolchain`. Keep them in sync.

## Usage

```sh
./bin/prove "For all positive reals a, b: a/b + b/a >= 2."
./bin/prove -i -v                 # interactive: Lean stays loaded between inputs
./bin/prove -confirm "..."        # approve or correct each formalization first
echo "..." | ./bin/prove -json    # machine-readable result
```

| Flag | Default | |
|---|---|---|
| `-i` | off | prompt repeatedly (`\` continues a line, `:q` quits, Ctrl-C cancels a job) |
| `-v` | off | show Lean feedback for failed attempts |
| `-model` | `deepseek-flash` | prover model (`deepseek-v4-pro` for harder problems) |
| `-effort` | `high` | reasoning effort: `low`, `high`, `max` |
| `-attempts` | 6 | proof attempts per session |
| `-parallel` | 1 | proof sessions racing per statement |
| `-budget` | 0.50 | USD cap for the run/session |
| `-workers` | 1 | Lean REPL processes |
| `-confirm`, `-no-split`, `-no-explain`, `-json`, `-timeout`, `-lean-dir`, `-env` | | see `-h` |

## Tests

```sh
go test ./internal/...                                          # unit tests (no Lean, no API)
LEAN_INTEGRATION=1 go test -v ./internal/lean/                  # real Lean REPL (~5 s)
DEEPSEEK_LIVE=1 go test -v -run TestLive ./internal/llm/        # real API (< $0.01)
```

## Project structure

```
cmd/prove/            CLI driver for the pipeline
internal/
  lean/               Lean source
    repl.go             one REPL process: JSON protocol, timeouts, env cache
    pool.go             warm worker pool, recycling and replacement
    snapshot.go         content-addressed environments (incremental checking)
    check.go            proof/statement layout, verdict (axiom check)
    forbidden.go        rejects sorry/axiom/#eval/... in model output
    messages.go         diagnostics and error classification
    feedback.go         error rendering for the LLM (carets, goals)
  llm/                LLM source
    client.go           HTTP client, retries, keep-alive handling
    pricing.go          cost estimation (peak/off-peak)
    prompts.go          system prompts
    tasks.go            split, formalize, back-translate, prove sessions, explain
    extract.go          code/signature extraction from replies
  pipeline/           orchestration: statement chaining, repair loop, events, confirm hook
  dotenv/             .env loader
lean_env/             Lake project: Mathlib + REPL + Harness
  Harness/AxiomCheck.lean   #harness_axioms command
scripts/bootstrap.sh  environment setup
```

## How it works

- **Incremental checking.** Each worker imports Mathlib once at startup
  (seconds). Every check after that runs against the loaded environment
  (milliseconds). Environments are named by a hash of the code that built
  them, so any worker can rebuild a missing one, and proved statements are
  never re-elaborated for later ones.
- **Verification.** The model writes only the tactic body. The server
  assembles `theorem <name> <signature> := by <body>` and appends
  `#harness_axioms <name>`. A proof counts only if there are no errors and no
  `sorry`, and the axiom check passes at that exact line, using only
  `propext`, `Classical.choice` and `Quot.sound`.
- **Faithfulness.** The Lean statement is back-translated to English by a
  call that never sees the original text. With `-confirm` you check the two
  match before any proving happens.
- **Feedback.** Lean errors are classified (unknown identifier, unsolved
  goals, tactic failure, ...) and rendered under the offending proof line,
  with the goal state before the failing tactic.

## Security

Lean can run arbitrary code at elaboration time, so LLM output is untrusted.
`forbidden.go` blocks the obvious vectors, but it is **not a sandbox**.
Before exposing this to other users, run Lean workers in an isolated
container: no network, read-only filesystem, CPU and memory limits.
