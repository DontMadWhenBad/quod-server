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
go build -o bin/server ./cmd/server
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
| `-effort` | (ladder) | run a single stage at this effort (`none`, `low`, `high`, `max`) instead of the ladder |
| `-attempts`, `-parallel` | (per stage) | if set, override attempts per session and sessions in every stage |
| `-no-auto` | off | skip the automation tactics tried before the LLM |
| `-budget` | 0.50 | USD cap for the run/session |
| `-workers` | 1 | warm Lean workers (each job gets a fresh one; use 2 with `-i`) |
| `-sandbox` | `bwrap` | Lean isolation: `bwrap` or `none` (trusted input only) |
| `-sandbox-mem`, `-sandbox-tasks` | `10G`, 256 | per-worker memory and process/thread limits |
| `-confirm`, `-no-split`, `-no-explain`, `-json`, `-timeout`, `-lean-dir`, `-env` | | see `-h` |

## HTTP server

```sh
./bin/server -addr 127.0.0.1:8080        # loads Lean once, then serves jobs
```

| Endpoint | |
|---|---|
| `POST /v1/jobs` | `{"input": "...", "options": {...}}` → 202 + job view |
| `GET /v1/jobs` | job summaries, newest first |
| `GET /v1/jobs/{id}` | full job state (`?detail=full` adds Lean feedback) |
| `GET /v1/jobs/{id}/events` | SSE progress stream; resume with `Last-Event-ID` or `?after=N` |
| `POST /v1/jobs/{id}/confirm` | `{"stmt": 1, "accept": true}` or `{"stmt": 1, "correction": "..."}` |
| `POST /v1/jobs/{id}/cancel` | cancel a queued or running job |
| `GET /healthz`, `GET /readyz` | liveness; readiness (Lean loaded) and stats |

Job options: `confirm`, `budget_usd`, `attempts`, `parallel`, `no_split`, `no_explain`.
Job states: `queued → running ⇄ awaiting_confirmation → done | failed | canceled`.

```sh
ID=$(curl -s localhost:8080/v1/jobs -H 'Content-Type: application/json' \
  -d '{"input": "For all real x, x^2 >= 0."}' | jq -r .id)
curl -N localhost:8080/v1/jobs/$ID/events      # live progress
curl -s localhost:8080/v1/jobs/$ID | jq        # result
```

Example client (bash + curl + jq) with live progress, confirmations at the
terminal, reconnect on dropped streams, and Ctrl-C to cancel:

```sh
scripts/client.sh prove "For all real x, x^2 >= 0."
scripts/client.sh prove --confirm -v "1) ... 2) ..."
scripts/client.sh list | watch ID | result ID | status ID | cancel ID
```

Pick the server with `-s URL` before the command (for example
`scripts/client.sh -s http://127.0.0.1:18080 list`) or with `PROVER_URL`
(default `http://127.0.0.1:8080`). Set `API_TOKEN` if the server needs one.

- **Limits:** `-max-running`, `-max-queued` (429 when full), `-job-budget`,
  `-max-job-budget`, `-daily-budget`, `-job-timeout`. Input is capped at 8 KB
  and 10 statements.
- **Auth:** set `API_TOKEN` in `.env` to require `Authorization: Bearer <token>`.
  There is none by default, so keep the server on localhost.
- **Storage:** jobs live in memory, are kept for 24 h, and are lost on restart.
- **Shutdown:** on Ctrl-C or SIGTERM the server stops accepting jobs, waits
  `-grace` for running ones, cancels the rest, then exits.

## Running as a service

`deploy/quod-server.service` is a systemd **user** unit. It assumes the
checkout is at `~/quod-server` and starts at boot (lingering), restarting
on failure:

```sh
mkdir -p ~/.config/systemd/user
cp deploy/quod-server.service ~/.config/systemd/user/
echo 'QUOD_ADDR=100.x.y.z:8080' > ~/.config/quod-server.env   # optional; default 127.0.0.1:8080
loginctl enable-linger "$USER"
systemctl --user daemon-reload
systemctl --user enable --now quod-server
```

```sh
systemctl --user start|stop|restart|status quod-server
journalctl --user -u quod-server -f          # logs
systemctl --user disable --now quod-server   # stop and don't start at boot
```

Update: `git pull && scripts/bootstrap.sh && go build -o bin/server ./cmd/server && systemctl --user restart quod-server`.


```sh
go test ./internal/...                                          # unit tests (no Lean, no API)
LEAN_INTEGRATION=1 go test -v ./internal/lean/                  # real Lean: REPL, sandbox escapes, verifier vs. cheats (~8 min)
LEAN_INTEGRATION=1 LEAN_SANDBOX_MEMTEST=1 go test -v -run TestSandbox ./internal/lean/   # + memory bomb (needs >10 GB free)
DEEPSEEK_LIVE=1 go test -v -run TestLive ./internal/llm/        # real API (< $0.01)
```

## Project structure

```
cmd/prove/            CLI driver for the pipeline
cmd/server/           HTTP API server
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
  jobs/               job manager: queue, limits, budgets, event log, confirm hand-off
  server/             HTTP handlers, SSE, auth
  dotenv/             .env loader
lean_env/             Lake project: Mathlib + REPL + Harness
  Harness/AxiomCheck.lean   #harness_axioms command
scripts/bootstrap.sh  environment setup
scripts/client.sh     example API client
```

## How it works

- **Proof search.** First, automation: `omega`, `decide`, `norm_num`, `simp`,
  `positivity`, `linarith`, `nlinarith`, `aesop` and `exact?` are tried
  directly in Lean, with no LLM cost. If `exact?` succeeds, the term it found
  becomes the proof. Then the model works up a ladder of stages, each racing
  parallel sessions (propose → Lean check → feedback). A stage runs only if
  the previous one failed, and repeated proofs are shared across sessions:

  | stage | effort | sessions × attempts |
  |---|---|---|
  | `fast` | `none` | 4 × 3 |
  | `think` | `low` | 2 × 3 |
  | `deep` | `high` | 2 × 4 |

  On easy goals, the model's thinking time goes mostly into second-guessing
  Mathlib lemma names it cannot look up, which can mean 3–20k tokens and up
  to a minute or more per attempt. Lean answers those questions in
  milliseconds, so many cheap attempts with feedback usually win. Long
  thinking streams `thinking… N tokens` progress events, and every attempt
  records its model, effort and token counts.

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

Lean can run arbitrary code at elaboration time (`#eval`, custom
elaborators, `unsafe`), and users can steer what the LLM writes, so every
Lean process is treated as running hostile code. Defenses, outermost first:

1. **One process per job.** A job leases fresh workers and they are killed
   when it ends, never reused (`lean.Lease`). Warm spares hide the ~3 s
   startup. Mathlib's 5 GB of `.olean` files are shared through the page
   cache, so each extra worker costs ~0.5 GB of private memory.
2. **Sandbox per process** (`-sandbox bwrap`, the default), using
   bubblewrap and a systemd user scope:
   - fresh user, pid, net, ipc and uts namespaces, so there is no network
   - toolchain and `lean_env` mounted read-only, so nothing persistent can be
     tampered with (e.g. `Harness.olean`)
   - a private tmpfs `/tmp`, and no other host paths except `/usr`
   - cgroup limits: `MemoryMax` (no swap) and `TasksMax`
3. **Scrubbed environment.** Lean only sees `LEAN_*`, a toolchain-only
   `PATH` and `LD_LIBRARY_PATH`, and `HOME=/tmp`. API keys never reach it.
4. **Independent final verification** (`-verify`, on by default). The
   in-job check runs next to the job's own code, which could shadow it.
   So when a job finishes, three fresh sandboxes re-derive the result
   (`internal/lean/verify.go`, ~30 s):
   - compile the confirmed statements alone (`theorem stmt_n <sig> := sorry`,
     only `import Mathlib`), and separately compile the job's final file;
   - `leanchecker` re-checks every declaration of the job in Lean's kernel;
   - `harness_verify` (`lean_env/HarnessVerify.lean`) runs no job code. It
     checks that each `stmt_n` is a theorem whose type is identical to the
     separately compiled statement, so notation or instance tricks don't
     count. It walks the dependency closure for axioms, allowing only
     `propext`, `Classical.choice` and `Quot.sound`, without trusting
     Lean's cached axiom data. And it rejects any imported module outside
     the read-only toolchain, Mathlib or Harness locations.

   A proof the verifier rejects becomes `failed`. If verification cannot
   run, the statement is `unverified`, never `proved`.
5. **Forbidden-syntax filter** (`forbidden.go`) and the in-job axiom check.
   These give fast feedback during attempts; they are not a security
   boundary.

`internal/lean/sandbox_test.go` checks each channel against a live sandbox:
secrets, host files, `/proc`, writes to `lean_env`, network, task limits,
`/tmp` shared between workers, worker reuse, and optionally a memory bomb.

Requirements: `bwrap` with unprivileged user namespaces, and a systemd user
session for the limits. On a headless server run
`loginctl enable-linger $USER`.

Not covered: a kernel-level escape from the namespaces. Add gVisor or a
microVM for public deployments.
