#!/usr/bin/env bash
set -euo pipefail

if [[ "${CLAWVISOR_LIVE_CODEX_SMOKE:-}" != "1" ]]; then
  cat >&2 <<'EOF'
Refusing to run live model smoke by default.

Set CLAWVISOR_LIVE_CODEX_SMOKE=1 to run. This uses the real codex CLI,
the live model configured in ~/.codex, and the local Clawvisor proxy logs.
EOF
  exit 2
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TRACE_LOG="${CLAWVISOR_TRACE_LOG:-$HOME/.clawvisor/logs/lite-proxy-trace.jsonl}"
RAW_LOG="${CLAWVISOR_RAW_LOG:-$HOME/.clawvisor/logs/lite-proxy-raw.jsonl}"
OUT_DIR="${CLAWVISOR_SMOKE_OUT_DIR:-$ROOT/.context/live-codex-secret-smoke-$(date +%Y%m%d-%H%M%S)}"
WORK_DIR="${CLAWVISOR_SMOKE_WORKDIR:-$(mktemp -d "${TMPDIR:-/tmp}/clawvisor-codex-smoke.XXXXXX")}"
CLAWVISOR_URL="${CLAWVISOR_URL:-http://127.0.0.1:25297}"
CLAWVISOR_URL="${CLAWVISOR_URL%/}"
OPENAI_BASE_URL="${CLAWVISOR_OPENAI_BASE_URL:-$CLAWVISOR_URL/v1}"
AGENT_TOKEN="${CLAWVISOR_AGENT_TOKEN:-}"
AGENT_ID="${CLAWVISOR_SMOKE_AGENT_ID:-}"
MODEL_ARGS=()
if [[ -n "${CLAWVISOR_SMOKE_MODEL:-}" ]]; then
  MODEL_ARGS=(-m "$CLAWVISOR_SMOKE_MODEL")
fi

mkdir -p "$OUT_DIR" "$WORK_DIR"

if ! command -v codex >/dev/null 2>&1; then
  echo "codex CLI not found on PATH" >&2
  exit 127
fi

if [[ -z "$AGENT_TOKEN" && -n "${CLAWVISOR_SMOKE_AGENT:-}" ]]; then
  registry_path="${CLAWVISOR_AGENT_REGISTRY:-$HOME/.clawvisor/agents.json}"
  if [[ ! -f "$registry_path" ]]; then
    printf 'registered agent %q not found; missing %s\n' "$CLAWVISOR_SMOKE_AGENT" "$registry_path" >&2
    exit 2
  fi
  resolved="$(
    python3 - "$registry_path" "$CLAWVISOR_SMOKE_AGENT" <<'PY'
import json
import sys

path, alias = sys.argv[1:3]
with open(path, "r", encoding="utf-8") as f:
    data = json.load(f)
entry = (data.get("agents") or {}).get(alias)
if not entry or not (entry.get("token") or "").strip():
    raise SystemExit(1)
print((entry.get("token") or "").strip())
print((entry.get("agent_id") or "").strip())
print((entry.get("server_url") or "").strip())
PY
  )" || {
    printf 'registered agent %q not found in %s\n' "$CLAWVISOR_SMOKE_AGENT" "$registry_path" >&2
    exit 2
  }
  AGENT_TOKEN="$(printf '%s\n' "$resolved" | sed -n '1p')"
  AGENT_ID="$(printf '%s\n' "$resolved" | sed -n '2p')"
  registered_url="$(printf '%s\n' "$resolved" | sed -n '3p')"
  if [[ "${CLAWVISOR_URL:-}" == "http://127.0.0.1:25297" && -n "$registered_url" ]]; then
    CLAWVISOR_URL="${registered_url%/}"
    OPENAI_BASE_URL="${CLAWVISOR_OPENAI_BASE_URL:-$CLAWVISOR_URL/v1}"
  fi
fi

if [[ -z "$AGENT_TOKEN" ]]; then
  cat >&2 <<'EOF'
Live Codex smoke requires a Clawvisor agent token so the codex CLI actually
routes through the local lite proxy.

Set CLAWVISOR_AGENT_TOKEN=cvis_... or set CLAWVISOR_SMOKE_AGENT=<registered alias>.
You can create an alias with:
  clawvisor agent register live-codex-smoke
EOF
  exit 2
fi

trace_start=1
raw_start=1
if [[ -f "$TRACE_LOG" ]]; then
  trace_start=$(( $(wc -l < "$TRACE_LOG") + 1 ))
fi
if [[ -f "$RAW_LOG" ]]; then
  raw_start=$(( $(wc -l < "$RAW_LOG") + 1 ))
fi

secret="${CLAWVISOR_SMOKE_SECRET:-re_CVISLiveSmoke$(date +%s)AbCdEf1234567890}"

exec_args=(
  -c model_provider=clawvisor
  -c 'model_providers.clawvisor.name="clawvisor"'
  -c "model_providers.clawvisor.base_url=\"$OPENAI_BASE_URL\""
  -c 'model_providers.clawvisor.env_key="CLAWVISOR_AGENT_TOKEN"'
  --json
  --skip-git-repo-check
  --sandbox read-only
  -c features.hooks=false
  "${MODEL_ARGS[@]}"
)
resume_args=(
  -c model_provider=clawvisor
  -c 'model_providers.clawvisor.name="clawvisor"'
  -c "model_providers.clawvisor.base_url=\"$OPENAI_BASE_URL\""
  -c 'model_providers.clawvisor.env_key="CLAWVISOR_AGENT_TOKEN"'
  --json
  --skip-git-repo-check
  -c features.hooks=false
  "${MODEL_ARGS[@]}"
)

run_turn() {
  local label="$1"
  local prompt="$2"
  local json_out="$OUT_DIR/${label}.jsonl"
  local text_out="$OUT_DIR/${label}.txt"
  if [[ "$label" == "turn1" ]]; then
    (cd "$WORK_DIR" && \
      CLAWVISOR_AGENT_TOKEN="$AGENT_TOKEN" \
      CLAWVISOR_URL="$CLAWVISOR_URL" \
      CLAWVISOR_PROXY_LITE=1 \
      CLAWVISOR_PROXY_LITE_PROVIDER=codex \
      OPENAI_API_KEY="$AGENT_TOKEN" \
      OPENAI_BASE_URL="$OPENAI_BASE_URL" \
      codex exec "${exec_args[@]}" -o "$text_out" "$prompt") | tee "$json_out"
  else
    (cd "$WORK_DIR" && \
      CLAWVISOR_AGENT_TOKEN="$AGENT_TOKEN" \
      CLAWVISOR_URL="$CLAWVISOR_URL" \
      CLAWVISOR_PROXY_LITE=1 \
      CLAWVISOR_PROXY_LITE_PROVIDER=codex \
      OPENAI_API_KEY="$AGENT_TOKEN" \
      OPENAI_BASE_URL="$OPENAI_BASE_URL" \
      codex exec resume --last "${resume_args[@]}" -o "$text_out" "$prompt") | tee "$json_out"
  fi
}

run_turn turn1 "Live Clawvisor secret replay smoke. Do not use tools. I am going to provide a synthetic Resend-looking API key for proxy testing only: $secret. Reply with one short sentence saying whether the credential you see is raw or a placeholder."

run_turn turn2 "vault resend"

run_turn turn3 "Continue the smoke test. Do not use tools. In one short sentence, say whether the credential visible in the earlier user request now appears raw or as an autovault placeholder."

python3 - "$OUT_DIR" "$TRACE_LOG" "$RAW_LOG" "$trace_start" "$raw_start" "$secret" "$AGENT_ID" <<'PY'
import json
import pathlib
import sys

out_dir = pathlib.Path(sys.argv[1])
trace_log = pathlib.Path(sys.argv[2])
raw_log = pathlib.Path(sys.argv[3])
trace_start = int(sys.argv[4])
raw_start = int(sys.argv[5])
secret = sys.argv[6]
agent_id = sys.argv[7]

def read_new(path, start):
    if not path.exists():
        return []
    lines = path.read_text(errors="replace").splitlines()
    return lines[start - 1:]

trace_lines = read_new(trace_log, trace_start)
raw_lines = read_new(raw_log, raw_start)
combined_text = "\n".join(p.read_text(errors="replace") for p in out_dir.glob("turn*.txt") if p.exists())
combined_json = "\n".join(p.read_text(errors="replace") for p in out_dir.glob("turn*.jsonl") if p.exists())

secret_events = []
for line in trace_lines:
    try:
        ev = json.loads(line)
    except Exception:
        continue
    if ev.get("event") == "secret_pipeline" and (not agent_id or ev.get("agent_id") == agent_id):
        secret_events.append(ev)

hold_events = [ev for ev in secret_events if ev.get("stage") == "hold_created"]
vault_events = [ev for ev in secret_events if ev.get("stage") == "decision_vaulted_finding"]
rewrite_events = [ev for ev in secret_events if ev.get("stage") == "rewrite_scan_done" and ev.get("modified")]
history_events = [ev for ev in secret_events if ev.get("stage") == "history_stripped"]

failures = []
if len(hold_events) != 1:
    failures.append(f"expected exactly one secret hold, saw {len(hold_events)}")
if not vault_events:
    failures.append("expected a decision_vaulted_finding trace event")
if not rewrite_events:
    failures.append("expected a modified rewrite_scan_done trace event")
if not history_events:
    failures.append("expected a history_stripped trace event")
if combined_text.count("Clawvisor detected a possible raw secret") != 1 and combined_json.count("Clawvisor detected a possible raw secret") != 1:
    failures.append("expected exactly one visible Clawvisor secret prompt across turns")
raw_secret_phases = []
for line in raw_lines:
    if secret not in line:
        continue
    try:
        ev = json.loads(line)
    except Exception:
        raw_secret_phases.append("<unparseable>")
        continue
    raw_secret_phases.append(ev.get("phase") or "<missing>")
unexpected_raw_secret_phases = [phase for phase in raw_secret_phases if phase != "proxy_received_request"]
if unexpected_raw_secret_phases:
    failures.append(f"raw secret appeared outside proxy_received_request phases: {unexpected_raw_secret_phases}")

summary = {
    "out_dir": str(out_dir),
    "trace_events": len(secret_events),
    "hold_events": len(hold_events),
    "vault_events": len(vault_events),
    "rewrite_events": len(rewrite_events),
    "history_events": len(history_events),
    "raw_secret_phases": raw_secret_phases,
    "trace_log": str(trace_log),
    "raw_log": str(raw_log),
    "secret": "<synthetic redacted>",
    "agent_id": agent_id,
    "failures": failures,
}
(out_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
print(json.dumps(summary, indent=2))
if failures:
    sys.exit(1)
PY

echo "Live Codex smoke artifacts: $OUT_DIR"
