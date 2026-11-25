#!/usr/bin/env bash
# Bootstrap everything the server needs locally:
#   1. Lean toolchain (via elan, pinned by lean_env/lean-toolchain)
#   2. Mathlib + REPL sources (pinned in lean_env/lakefile.toml)
#   3. Mathlib prebuilt .olean cache (avoids hours of compiling)
#   4. Harness lib (custom axiom check) and the REPL binary
#   5. Smoke tests: a valid proof passes, a `sorry` proof is rejected
#   6. Go module deps (if go.mod exists)
#
# Idempotent: safe to re-run. Usage:
#   scripts/bootstrap.sh            # full bootstrap
#   SKIP_TESTS=1 scripts/bootstrap.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LEAN_ENV="$ROOT/lean_env"

START=$SECONDS
STEP=0
# Each step prints its number and total elapsed time so long phases are visible.
log()  {
  STEP=$((STEP + 1))
  local t=$((SECONDS - START))
  printf '\033[1;34m==> [%d | %02d:%02d]\033[0m %s\n' "$STEP" $((t / 60)) $((t % 60)) "$*"
}
warn() { printf '\033[1;33mwarn:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- prerequisites
log "Checking prerequisites"
command -v git  >/dev/null || die "git not found"
command -v curl >/dev/null || die "curl not found"
if ! command -v elan >/dev/null; then
  die "elan not found. Install it:
  curl https://raw.githubusercontent.com/leanprover/elan/master/elan-init.sh -sSf | sh -s -- -y --default-toolchain none"
fi
command -v go >/dev/null || warn "go not found (needed to build the server, not for Lean)"

avail_gb=$(df -Pk "$ROOT" | awk 'NR==2 {print int($4/1024/1024)}')
(( avail_gb >= 10 )) || warn "only ${avail_gb}GB free; Mathlib cache needs ~6GB"
mem_gb=$(awk '/MemTotal/ {print int($2/1024/1024)}' /proc/meminfo 2>/dev/null || echo 0)
(( mem_gb >= 8 )) || warn "only ${mem_gb}GB RAM; each Lean process importing Mathlib uses several GB"

cd "$LEAN_ENV"

# ---------------------------------------------------------------- toolchain
TOOLCHAIN="$(tr -d '[:space:]' < lean-toolchain)"
log "Installing Lean toolchain $TOOLCHAIN"
elan toolchain install "$TOOLCHAIN"

# ---------------------------------------------------------------- dependencies
if [[ ! -f lake-manifest.json ]]; then
  # Only when there is no lockfile: pick revisions and write one.
  log "Resolving dependencies (no lake-manifest.json yet)"
  lake update
fi

# With a committed lake-manifest.json (the normal case on a fresh clone),
# any lake command clones missing packages at the locked revisions, so this
# step both materializes .lake/packages and downloads Mathlib's .olean cache.
log "Fetching dependencies at locked revisions + Mathlib .olean cache (first run: several GB)"
lake exe cache get

# Mathlib must use exactly our toolchain, or the cache is useless.
[[ -f .lake/packages/mathlib/lean-toolchain ]] || die "Mathlib was not fetched into lean_env/.lake/packages"
MATHLIB_TC="$(tr -d '[:space:]' < .lake/packages/mathlib/lean-toolchain)"
[[ "$MATHLIB_TC" == "$TOOLCHAIN" ]] || die "toolchain mismatch: lean_env=$TOOLCHAIN mathlib=$MATHLIB_TC
Update lean_env/lean-toolchain to match, then re-run."

# ---------------------------------------------------------------- build
log "Building Harness (axiom check)"
lake build Harness

log "Building harness_verify (independent final checker)"
lake build harness_verify

log "Building REPL"
lake build REPL/repl
REPL_BIN="$LEAN_ENV/.lake/packages/REPL/.lake/build/bin/repl"
[[ -x "$REPL_BIN" ]] || die "REPL binary not found at $REPL_BIN"

# ---------------------------------------------------------------- smoke tests
if [[ "${SKIP_TESTS:-0}" != "1" ]]; then
  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT

  cat > "$TMP/good.lean" <<'EOF'
import Mathlib
import Harness

theorem harness_smoke (a b : ℕ) : a + b = b + a := by
  omega

#harness_axioms harness_smoke
EOF

  cat > "$TMP/bad.lean" <<'EOF'
import Harness

theorem harness_cheat : 1 = 2 := by
  sorry

#harness_axioms harness_cheat
EOF

  log "Smoke test: valid Mathlib proof must pass"
  out="$(lake env lean "$TMP/good.lean" 2>&1)" || die "good.lean failed to compile:
$out"
  grep -q HARNESS_AXIOMS_OK <<<"$out" || die "good.lean missing HARNESS_AXIOMS_OK:
$out"

  log "Smoke test: sorry proof must be rejected"
  if out="$(lake env lean "$TMP/bad.lean" 2>&1)"; then
    die "bad.lean compiled without error; axiom check is broken:
$out"
  fi
  grep -q HARNESS_AXIOMS_BAD <<<"$out" || die "bad.lean failed for the wrong reason:
$out"

  log "Smoke test: REPL responds"
  out="$(printf '{"cmd": "example : 1 + 1 = 2 := rfl"}\n\n' | lake env "$REPL_BIN" 2>&1)" \
    || die "REPL failed:
$out"
  grep -q '"env"' <<<"$out" || die "unexpected REPL output:
$out"
fi

# ---------------------------------------------------------------- go deps
if [[ -f "$ROOT/go.mod" ]] && command -v go >/dev/null; then
  log "Downloading Go modules"
  (cd "$ROOT" && go mod download)
fi

log "Bootstrap complete"
cat <<EOF
  Lean project : $LEAN_ENV
  Toolchain    : $TOOLCHAIN
  REPL binary  : $REPL_BIN
  Compile file : (cd $LEAN_ENV && lake env lean <file.lean>)
EOF
