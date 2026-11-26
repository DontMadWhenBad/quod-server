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

Needs Go ≥ 1.26 (the pure-Go SQLite driver requires it).

```sh
# 1. Lean toolchain, Mathlib (+ prebuilt cache), REPL, axiom checker, smoke tests
scripts/bootstrap.sh

# 2. API key
cp .env.example .env    # then set DEEPSEEK_API_KEY=...

# 3. Build
go build -o bin/prove ./cmd/prove
go build -o bin/server ./cmd/server
go build -o bin/quod-admin ./cmd/quod-admin
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
| `GET /v1/status` | your key's usage today (spend, remaining budget, active jobs) and server load |
| `GET /healthz`, `GET /readyz` | public; liveness and readiness only (`{"ready": true}`), no stats |

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
scripts/client.sh -r prove "..." | jq -c 'select(.type=="attempt")'   # --raw: server JSON as-is
```

With `-r`/`--raw`, each command prints the server's JSON unchanged.
`prove` and `watch` print one JSON object per line: the submitted job,
every event, then the final job. Error bodies go to stderr, with exit
status 1.

Pick the server with `-s URL` before the command (for example
`scripts/client.sh -s http://127.0.0.1:18080 list`) or with `PROVER_URL`
(default `http://127.0.0.1:8080`). The access key is read from `QUOD_KEY`,
`-k FILE`, or `~/.config/quod/key`.

- **Limits:** `-max-running`, `-max-queued` (429 when full), `-job-budget`,
  `-max-job-budget`, `-daily-budget`, `-job-timeout`. Input is capped at 8 KB
  and 10 statements. Per client:

  | limit | default | flag |
  |---|---|---|
  | requests without a valid key, per client IP | 20/min, burst 10; once exhausted, keys are not checked | `-rate-unauth` |
  | requests per key | 120/min, burst 30 | `-rate-key` |
  | queued + running jobs per key | 2 | `-max-jobs-per-key` |
  | open event streams per key | 8 | `-max-streams-per-key` |

  A limited request gets 429 with `Retry-After`.
- **Behind a reverse proxy** (e.g. Cloudflare Tunnel), set
  `-trusted-proxies 127.0.0.1,::1`. The client IP is then taken from
  `CF-Connecting-IP` or `X-Forwarded-For`, but only when the direct peer is a
  trusted proxy; otherwise those headers are ignored so clients cannot spoof
  them. Each request is logged with a request id (also returned as
  `X-Request-ID`), client IP and key name, never the key itself.
  Server-wide stats are logged once a minute.
- **Auth off only locally:** `-auth none` refuses to start on anything but a
  loopback address.
- **Access keys** (`-auth keys`, the default): every `/v1` request needs
  `Authorization: Bearer qk_...`, a key issued by the server's admin.
  - Clients never handle the DeepSeek key; the service pays for LLM calls.
  - Each key sees and controls only its own jobs; other jobs look like 404.
  - A key can have its own daily budget.
  - Keys live in a SQLite database (`-db`, default `quod.db`, mode 0600) as
    SHA-256 hashes only. Every request is checked against the database, so
    a created or revoked key takes effect on the next request, without a
    restart. `quod-admin` and the server can use the database at the same
    time.
  - `-auth none` disables authentication, for local development only.
  - Login (OAuth) can be added later as another authenticator; jobs and
    budgets only see the caller's identity.

  ```sh
  go build -o bin/quod-admin ./cmd/quod-admin
  bin/quod-admin keys create --name alice --daily-budget 2   # prints the key once
  bin/quod-admin keys list
  bin/quod-admin keys revoke alice
  bin/quod-admin db backup quod-backup.db    # consistent copy while the server runs (mode 0600)
  ```
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
cmd/quod-admin/       access key management
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
  server/             HTTP handlers, SSE, auth middleware
  auth/               principals, access key store (SQLite)
  dotenv/             .env loader
lean_env/             Lake project: Mathlib + REPL + Harness
  Harness/AxiomCheck.lean   #harness_axioms command
scripts/bootstrap.sh  environment setup
scripts/client.sh     example API client
```

## How it works

- **Proof search.** First, automation: `omega`, `decide`, `norm_num`,
  `simp`, `positivity`, `linarith`, `nlinarith`, `aesop` and `exact?` are
  tried directly in Lean, with no LLM cost. If `exact?` succeeds, the term it
  found becomes the proof. Then the model works up a ladder of stages, each
  racing parallel sessions (propose → Lean check → feedback). A stage runs
  only if the previous one failed, and failed proofs are shared across
  sessions:

  | stage | effort | tools | sessions × attempts |
  |---|---|---|---|
  | `fast` | `none` | – | 4 × 3 |
  | `tools` | `none` | ✓ | 2 × 3 (≤ 16 tool calls each) |
  | `deep-tools` | `high` | ✓ | 2 × 3 |

  Choose your own ladder with `-stages` (presets: `fast`, `tools`,
  `tools-low`, `think`, `deep`, `deep-tools`).

  **Tools** let the model use Lean while it works instead of guessing:
  - `search_lemmas` finds theorems by name fragments (`#harness_search`);
  - `check_name` shows a lemma's exact statement;
  - `find_lemma` runs `exact?` on a sub-goal;
  - `check_proof` compiles a candidate proof and returns errors and goals.
    It accepts `sorry` placeholders and lists the open goal at each one, but
    a proof with `sorry` is never verified. A passing `check_proof` ends the
    search.

  Arguments are validated before they become Lean code: single line,
  bounded length, and the forbidden-syntax filter still applies.

  Why: on easy goals, a thinking model spends most of its time (3–20k
  tokens, a minute or more per attempt) second-guessing Mathlib lemma names
  it cannot look up. With tools and no thinking, `3 ∣ n³ − n` went from
  about 4 minutes to about 25 s. Every attempt records its stage, model,
  effort, token counts and tool calls, and long generations stream
  `thinking… N tokens` progress events.

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
