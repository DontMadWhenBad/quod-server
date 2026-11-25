#!/usr/bin/env bash
# Example client for the prover HTTP API (cmd/server). Needs curl and jq.
#
#   scripts/client.sh prove "For all real x, x^2 >= 0."
#   scripts/client.sh prove --confirm "1) ... 2) ..."     # approve each formalization
#   echo "..." | scripts/client.sh prove -
#   scripts/client.sh list | watch ID | result ID | status ID | cancel ID
#   scripts/client.sh confirm ID STMT yes|no|"correction text"
#
# Environment:
#   PROVER_URL  server base URL (default http://127.0.0.1:8080)
#   API_TOKEN   bearer token, if the server requires one
set -euo pipefail

URL="${PROVER_URL:-http://127.0.0.1:8080}"
URL="${URL%/}"

die() { printf 'client: %s\n' "$*" >&2; exit 1; }
command -v curl >/dev/null || die "curl not found"
command -v jq   >/dev/null || die "jq not found"

if [[ -t 1 ]]; then
  B=$'\e[1m' DIM=$'\e[2m' RED=$'\e[31m' GRN=$'\e[32m' YEL=$'\e[33m' BLU=$'\e[34m' RST=$'\e[0m'
else
  B='' DIM='' RED='' GRN='' YEL='' BLU='' RST=''
fi

auth=()
[[ -n "${API_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $API_TOKEN")

# api METHOD PATH [JSON_BODY] -> prints body; fails with the server's error message.
api() {
  local method=$1 path=$2 body=${3:-} out code
  local args=(-sS -X "$method" "${auth[@]}" -w $'\n%{http_code}')
  [[ -n "$body" ]] && args+=(-H 'Content-Type: application/json' --data-binary "$body")
  out=$(curl "${args[@]}" "$URL$path") || die "cannot reach $URL"
  code=${out##*$'\n'}
  out=${out%$'\n'*}
  if (( code >= 400 )); then
    die "HTTP $code: $(jq -r '.error.message // .' <<<"$out" 2>/dev/null || echo "$out")"
  fi
  printf '%s' "$out"
}

usage() {
  sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
  cat <<'EOF'

prove options:
  --confirm         ask before proving each formalized statement
  --yes             with --confirm: accept everything (non-interactive)
  --no-split        treat input as one statement
  --no-explain      skip explanations
  --attempts N      proof attempts per session
  --parallel N      parallel proof sessions per statement
  --budget USD      LLM budget for this job
  -v, --verbose     show Lean feedback for failed attempts
EOF
  exit "${1:-0}"
}

# ------------------------------------------------------------------ rendering

# One jq program renders every event type as display text.
read -r -d '' RENDER <<'JQ' || true
def secs: ((. // 0) / 1e8 | round) / 10;
def ms: ((. // 0) / 1e6 | round);
def one: gsub("\\s+"; " ");
def tag: if (.stmt // 0) > 0 then "stmt_\(.stmt): " else "" end;
if .type == "state" then "state → \(.state)"
elif .type == "split" then "split into \(.message)"
elif .type == "formalizing" then "\(tag)formalizing (\(.message))"
elif .type == "statement_error" then "\(tag)Lean rejected the statement: \(.message | split("\n")[0])"
elif .type == "formalized" then
  "\(tag)formalized\n      theorem \(.statement.name) \(.statement.signature | one)\n      means: \(.statement.back_translation | one)"
elif .type == "revising" then "\(tag)re-formalizing: \(.message)"
elif .type == "attempt" then .attempt as $a |
  "\(tag)attempt \($a.session).\($a.n) "
  + (if $a.verified then "✓ verified" else "✗ " + (($a.error_kinds // []) | join(", ")) end)
  + "  (llm \($a.llm_time | secs)s, lean \($a.lean_time | ms)ms, $\((($a.cost_usd // 0) * 10000 | round) / 10000))"
  + (if $verbose and ($a.feedback // "") != "" then "\n" + ($a.feedback | split("\n") | map("      │ " + .) | join("\n")) else "" end)
elif .type == "proved" then "\(tag)PROVED"
elif .type == "failed" then "\(tag)FAILED: \(.statement.error // "")"
elif .type == "explained" then "\(tag)explanation ready"
elif .type == "error" then "\(tag)error: \(.message)"
elif .type == "done" then "done: \(.message)"
elif .type == "confirm_required" then "\(tag)waiting for confirmation"
else "\(.type) \(.message // "")" end
JQ

color_for() {
  case $1 in
    proved|done) printf '%s' "$GRN" ;;
    failed|error|statement_error) printf '%s' "$RED" ;;
    confirm_required|revising) printf '%s' "$YEL" ;;
    formalized) printf '%s' "$BLU" ;;
    state|split|formalizing|explained) printf '%s' "$DIM" ;;
    *) printf '' ;;
  esac
}

START=$SECONDS
show_event() { # $1 = event json
  local type text
  type=$(jq -r .type <<<"$1")
  text=$(jq -r --argjson verbose "$VERBOSE" "$RENDER" <<<"$1")
  printf '%s[%4ds]%s %s%s%s\n' "$DIM" $((SECONDS - START)) "$RST" "$(color_for "$type")" "$text" "$RST"
}

# ------------------------------------------------------------------ confirmation

ask_confirm() { # $1 = job id, $2 = event json
  local id=$1 ev=$2 stmt ans body
  stmt=$(jq -r .proposal.index <<<"$ev")
  jq -r '.proposal | "\n  \(.name): \(.text | gsub("\\s+";" "))\n  Lean:  theorem \(.name) \(.signature | gsub("\\s+";" "))\n  Means: \(.back_translation | gsub("\\s+";" "))" + (if (.notes // "") != "" then "\n  Notes: " + (.notes | gsub("\n";"\n         ")) else "" end)' <<<"$ev" >&2
  if [[ $AUTO_YES == 1 ]]; then
    ans=""
    printf '  %s(auto-accepted)%s\n' "$DIM" "$RST" >&2
  else
    [[ -r /dev/tty ]] || die "confirmation needs a terminal (or use --yes)"
    printf '  %sProve this? [Enter=yes, n=skip, or type a correction]:%s ' "$YEL" "$RST" >&2
    IFS= read -r ans </dev/tty || ans=n
  fi
  case "${ans,,}" in
    ""|y|yes) body=$(jq -nc --argjson s "$stmt" '{stmt: $s, accept: true}') ;;
    n|no)     body=$(jq -nc --argjson s "$stmt" '{stmt: $s, accept: false}') ;;
    *)        body=$(jq -nc --argjson s "$stmt" --arg c "$ans" '{stmt: $s, accept: false, correction: $c}') ;;
  esac
  api POST "/v1/jobs/$id/confirm" "$body" >/dev/null
}

# ------------------------------------------------------------------ streaming

# stream_job ID: follow the SSE stream until the server sends "end",
# reconnecting with Last-Event-ID if the connection drops.
stream_job() {
  local id=$1 last=0 ended=0 tries=0 line data
  while (( ! ended )); do
    while IFS= read -r line; do
      line=${line%$'\r'}
      case $line in
        "id: "*)       last=${line#id: } ;;
        "event: end")  ended=1 ;;
        "data: "*)
          data=${line#data: }
          [[ $data == "{}" ]] && continue
          show_event "$data"
          if [[ $(jq -r .type <<<"$data") == confirm_required ]]; then
            # Only answer if it is still pending (we may be replaying history).
            if [[ $(api GET "/v1/jobs/$id" | jq -r '.pending_confirmation.index // empty') == "$(jq -r .proposal.index <<<"$data")" ]]; then
              ask_confirm "$id" "$data"
            fi
          fi
          ;;
      esac
    done < <(curl -sN "${auth[@]}" -H "Accept: text/event-stream" -H "Last-Event-ID: $last" "$URL/v1/jobs/$id/events" || true)
    if (( ! ended )); then
      (( ++tries > 10 )) && die "lost the event stream (job $id is still on the server)"
      printf '%s(stream dropped, reconnecting after event %s)%s\n' "$DIM" "$last" "$RST" >&2
      sleep 2
    fi
  done
}

# ------------------------------------------------------------------ result

print_result() { # $1 = job id
  api GET "/v1/jobs/$1" | jq -r --arg B "$B" --arg R "$RST" --arg G "$GRN" --arg X "$RED" '
    def one: gsub("\\s+"; " ");
    def ind: split("\n") | map("    " + .) | join("\n");
    "",
    (.statements[] |
      "\($B)── \(.name) [\(if .status == "proved" then $G else $X end)\(.status)\($R)\($B)] ──\($R)",
      "  \(.text | one)",
      (if .signature then "  Lean:  theorem \(.name) \(.signature | one)\n  Means: \(.back_translation // "" | one)" else empty end),
      (if .proof then "  Proof:\n\(.proof | ind)" else empty end),
      (if .explanation then "  Explanation:\n\(.explanation | ind)" else empty end),
      (if .error and .status != "proved" then "  Error: \(.error)" else empty end),
      ""),
    (if .lean_file then "\($B)── Lean file ──\($R)\n\(.lean_file)" else empty end),
    "job \(.id): \(.state)" + (if .error then " (\(.error))" else "" end)
      + ", $\(((.cost_usd // 0) * 10000 | round) / 10000) over \(.llm_calls) LLM calls"'
}

# ------------------------------------------------------------------ commands

VERBOSE=false
AUTO_YES=0
JOB_ID=""

on_interrupt() {
  if [[ -n $JOB_ID ]]; then
    printf '\n%sinterrupted: canceling job %s%s\n' "$YEL" "$JOB_ID" "$RST" >&2
    api POST "/v1/jobs/$JOB_ID/cancel" >/dev/null || true
  fi
  exit 130
}

cmd_prove() {
  local confirm=false split=true explain=true attempts=0 parallel=0 budget=0 input
  while (( $# )); do
    case $1 in
      --confirm)    confirm=true ;;
      --yes)        AUTO_YES=1 ;;
      --no-split)   split=false ;;
      --no-explain) explain=false ;;
      --attempts)   attempts=${2:?}; shift ;;
      --parallel)   parallel=${2:?}; shift ;;
      --budget)     budget=${2:?}; shift ;;
      -v|--verbose) VERBOSE=true ;;
      -h|--help)    usage ;;
      --)           shift; break ;;
      -)            break ;;
      -*)           die "unknown option $1" ;;
      *)            break ;;
    esac
    shift
  done
  if [[ $# -eq 0 || $1 == "-" ]]; then
    input=$(cat)
  else
    input="$*"
  fi
  [[ -n ${input//[[:space:]]/} ]] || die "no input"

  local body view
  body=$(jq -nc --arg input "$input" --argjson confirm "$confirm" --argjson split "$split" \
    --argjson explain "$explain" --argjson attempts "$attempts" --argjson parallel "$parallel" \
    --argjson budget "$budget" '
    {input: $input, options: ({confirm: $confirm, no_split: ($split | not), no_explain: ($explain | not),
      attempts: $attempts, parallel: $parallel, budget_usd: $budget} | with_entries(select(.value != 0 and .value != false)))}')
  view=$(api POST /v1/jobs "$body")
  JOB_ID=$(jq -r .id <<<"$view")
  printf '%sjob %s submitted (budget $%s)%s\n' "$B" "$JOB_ID" "$(jq -r .budget_usd <<<"$view")" "$RST"
  trap on_interrupt INT
  stream_job "$JOB_ID"
  trap - INT
  print_result "$JOB_ID"
}

cmd_watch() {
  [[ ${1:-} ]] || die "usage: watch ID [-v]"
  [[ ${2:-} == -v ]] && VERBOSE=true
  JOB_ID=$1
  stream_job "$1"
  print_result "$1"
}

cmd_list() {
  api GET "/v1/jobs?limit=${1:-20}" | jq -r '
    (["ID", "STATE", "PROVED", "COST", "CREATED", "INPUT"] | @tsv),
    (.jobs[] | [.id, .state + (if .awaiting_confirmation then "*" else "" end), "\(.proved)/\(.total)",
      "$\(((.cost_usd // 0) * 10000 | round) / 10000)", (.created | .[0:19]), (.input | gsub("\\s+";" ") | .[0:60])] | @tsv)' |
    column -t -s $'\t'
}

cmd_confirm() {
  local id=${1:?usage: confirm ID STMT yes|no|correction} stmt=${2:?} ans=${3:-yes} body
  case "${ans,,}" in
    y|yes) body=$(jq -nc --argjson s "$stmt" '{stmt: $s, accept: true}') ;;
    n|no)  body=$(jq -nc --argjson s "$stmt" '{stmt: $s, accept: false}') ;;
    *)     body=$(jq -nc --argjson s "$stmt" --arg c "$ans" '{stmt: $s, accept: false, correction: $c}') ;;
  esac
  api POST "/v1/jobs/$id/confirm" "$body" | jq -r '"job \(.id): \(.state)"'
}

case ${1:-} in
  prove)   shift; cmd_prove "$@" ;;
  watch)   shift; cmd_watch "$@" ;;
  result)  shift; print_result "${1:?usage: result ID}" ;;
  status)  shift; api GET "/v1/jobs/${1:?usage: status ID}${2:+?detail=full}" | jq . ;;
  list)    shift; cmd_list "$@" ;;
  cancel)  shift; api POST "/v1/jobs/${1:?usage: cancel ID}/cancel" | jq -r '"job \(.id): \(.state)"' ;;
  confirm) shift; cmd_confirm "$@" ;;
  health)  api GET /readyz | jq . ;;
  -h|--help|help|"") usage ;;
  *)       die "unknown command $1 (see --help)" ;;
esac
