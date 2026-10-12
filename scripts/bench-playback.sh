#!/usr/bin/env bash
set -euo pipefail

# ──────────────────────────────────────────────────────────────────────────────
# bench-playback.sh — benchmark virtual playback cold vs warm start latency
#
# Measures POST /api/v1/playback/start against a live Silo server.
#
# Usage:
#   ./scripts/bench-playback.sh [WARM_REPEATS] [COOLDOWN_SECONDS]
#
#   WARM_REPEATS     iterations per file for warm-start stats (default: 20)
#   COOLDOWN_SECONDS pause between files to drain URL memo (default: 35)
#
# Env vars:
#   SILO_SERVER   — base URL  (default: http://localhost:8090)
#   SILO_API_KEY  — API key   (required; no default is committed)
#   BENCH_DIAG_DIR — directory for terminal-response diagnostics
#                   (default: ./bench-diagnostics-<timestamp>)
#
# Terminal diagnosis: every cold start whose outcome is not "playable" has
# its raw start-response JSON copied to BENCH_DIAG_DIR and its terminal +
# plan fields extracted (terminal.message disambiguates the five
# adaptation_unavailable branches in internal/playback/plan_v3.go, which
# share one reason). Phase 5 prints the per-file diagnosis.
#
# Settings are also read from .silo-dev.env when present (the same file
# scripts/silo-dev reads). Never put the API key on the command line.
# ──────────────────────────────────────────────────────────────────────────────

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ENV_FILE="${SILO_DEV_ENV_FILE:-$REPO_ROOT/.silo-dev.env}"
if [[ -f "$ENV_FILE" ]]; then
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

WARM_REPS=${1:-20}
COOLDOWN=${2:-35}
DIAG_DIR="${BENCH_DIAG_DIR:-./bench-diagnostics-$(date +%Y%m%d-%H%M%S)}"
if [[ "$DIAG_DIR" != /tmp/* && "$DIAG_DIR" != "$HOME/.cache/"* ]]; then
  printf 'bench-playback: refusing to write diagnostics to %s (set BENCH_DIAG_DIR under /tmp or ~/.cache to keep raw responses out of the repo).\n' "$DIAG_DIR" >&2
  exit 2
fi
mkdir -p "$DIAG_DIR"
echo "  Diagnostics: $DIAG_DIR (raw responses for non-playable cold starts)"
SERVER="${SILO_SERVER:-http://localhost:8090}"
API_KEY="${SILO_API_KEY:-}"
if [[ -z "$API_KEY" ]]; then
  printf 'bench-playback: SILO_API_KEY is not set. Export it or add it to %s (see .silo-dev.env.example).\n' "$ENV_FILE" >&2
  exit 2
fi
SAMPLE_SIZE="${SAMPLE_SIZE:-0}"
SEED="${SEED:-42}"
PROFILE_ID="${PROFILE_ID:-06ddc31a-4694-4fa8-946c-a661f2099baf}"
MOVIE_LIBRARY_ID="${MOVIE_LIBRARY_ID:-31}"
SERIES_LIBRARY_ID="${SERIES_LIBRARY_ID:-32}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

AUTH="Authorization: Bearer $API_KEY"

# ── Helpers ───────────────────────────────────────────────────────────────────

curl_json() {
  curl -sf -H "$AUTH" -H "Content-Type: application/json" "$@"
}

# ── Capability profiles ───────────────────────────────────────────────────────

CAPS='{
  "video_evidence": "declared",
  "audio_evidence": "declared",
  "codecs_video": ["h264", "hevc", "vp9", "av1"],
  "codecs_video_hardware": ["h264", "hevc"],
  "codecs_audio": ["aac", "ac3", "eac3", "opus", "flac", "mp3"],
  "containers": ["mp4", "mkv", "webm", "ts"],
  "max_resolution": "2160p",
  "hdr": true,
  "hdr_details": {
    "hdr10": true,
    "hlg": true,
    "dolby_vision": true
  }
}'

CTX_PLAYABLE='{
  "protocol_version": 3,
  "form_factor": "desktop",
  "app_version": "bench-1.0",
  "device": {"platform": "linux", "os_version": "6.1"},
  "deliveries": {
    "hls": {
      "enabled": true,
      "supported_on_device": true,
      "containers": ["hls", "mp4", "mkv", "ts"],
      "video_codecs": ["h264", "hevc", "vp9", "av1"],
      "audio_decode_codecs": ["aac", "ac3", "eac3", "opus", "flac", "mp3"],
      "audio_passthrough_codecs": ["aac", "ac3", "eac3", "opus", "flac", "mp3"],
      "subtitles": {"embedded_text": true, "sidecar_text": true}
    },
    "progressive": {
      "enabled": true,
      "supported_on_device": true,
      "containers": ["mp4", "mkv", "webm", "ts"],
      "video_codecs": ["h264", "hevc", "vp9", "av1"],
      "audio_decode_codecs": ["aac", "ac3", "eac3", "opus", "flac", "mp3"],
      "audio_passthrough_codecs": ["aac", "ac3", "eac3", "opus", "flac", "mp3"],
      "subtitles": {"embedded_text": true, "sidecar_text": true}
    }
  }
}'

CTX_TERMINAL='{
  "protocol_version": 3,
  "form_factor": "desktop",
  "app_version": "bench-1.0",
  "device": {"platform": "linux", "os_version": "6.1"}
}'

build_body() {
  local file_id=$1 attempt_id=$2 ctx=$3
  cat <<EOF
{
  "protocol_version": 3,
  "file_id": $file_id,
  "profile_id": "$PROFILE_ID",
  "playback_attempt_id": "$attempt_id",
  "quality_preference": "automatic",
  "subtitle_fidelity_preference": "compatible",
  "client_capabilities": $CAPS,
  "client_playback_context": $ctx
}
EOF
}

# Fire a playback start and return: ttfb_ms total_ms outcome session_id
fire_start() {
  local file_id=$1 attempt_id=$2 ctx=$3 tag=$4
  local resp_file="$TMPDIR/resp-${tag}-${file_id}-${attempt_id}.json"
  local body
  body=$(build_body "$file_id" "$attempt_id" "$ctx")

  local timing
  timing=$(curl -s \
    -X POST "$SERVER/api/v1/playback/start" \
    -H "$AUTH" \
    -H "Content-Type: application/json" \
    -H "X-Profile-Id: $PROFILE_ID" \
    -H "X-Device-Id: bench-${tag}-${file_id}" \
    -o "$resp_file" \
    -w "%{time_starttransfer} %{time_total}" \
    -d "$body" 2>/dev/null || echo "0 0")

  local ttfb total
  ttfb=$(echo "$timing" | awk '{print $1}')
  total=$(echo "$timing" | awk '{print $2}')

  local ttfb_ms total_ms outcome sid
  # awk rather than a Python f-string: f'{float('...')}' needs Python 3.12+,
  # and older hosts fail it silently, reporting every duration as 0 ms.
  ttfb_ms=$(awk -v v="$ttfb" 'BEGIN{printf "%.0f", v*1000}' 2>/dev/null || echo "0")
  total_ms=$(awk -v v="$total" 'BEGIN{printf "%.0f", v*1000}' 2>/dev/null || echo "0")
  outcome="error"
  sid=""
  reason=""
  if [[ -f "$resp_file" ]]; then
    outcome=$(python3 -c "import json; d=json.load(open('$resp_file')); print(d.get('outcome','error'))" 2>/dev/null || echo "error")
    sid=$(python3 -c "import json; d=json.load(open('$resp_file')); print(d.get('session_id',''))" 2>/dev/null || echo "")
    reason=$(python3 -c "import json; d=json.load(open('$resp_file')); t=d.get('terminal') or {}; print(t.get('reason',''))" 2>/dev/null || echo "")
  fi
  echo "$ttfb_ms $total_ms $outcome $sid $reason"
}

# Stop a session (best-effort)
stop_session() {
  local sid=$1
  [[ -z "$sid" ]] && return
  curl -s -X DELETE "$SERVER/api/v1/playback/$sid" -H "$AUTH" >/dev/null 2>&1 || true
}

# ── Phase 1: Discover file_ids ───────────────────────────────────────────────

echo "=== Phase 1: Discovering file_ids ==="
echo ""

FILES=()
TITLES=()
TYPES=()

while IFS= read -r line; do
  cid=$(echo "$line" | cut -d'|' -f1)
  title=$(echo "$line" | cut -d'|' -f2)
  fid=$(curl_json "$SERVER/api/v1/catalog/items/$cid" 2>/dev/null \
    | python3 -c "import sys,json; d=json.load(sys.stdin); vs=d.get('playback_variants',[]); print(vs[0]['default_file_id'] if vs else '')" 2>/dev/null || echo "")
  if [[ -n "$fid" && "$fid" != "" ]]; then
    FILES+=("$fid"); TITLES+=("$title"); TYPES+=("movie")
    echo "  movie:     $title → file_id=$fid"
  fi
done < <(curl_json "$SERVER/api/v1/catalog?library_id=$MOVIE_LIBRARY_ID&limit=500&type=movie" 2>/dev/null \
  | python3 -c "import sys,json; [print(f\"{i['content_id']}|{i['title']}\") for i in json.load(sys.stdin).get('items',[])]" 2>/dev/null)

while IFS= read -r line; do
  cid=$(echo "$line" | cut -d'|' -f1)
  title=$(echo "$line" | cut -d'|' -f2)
  fid=$(curl_json "$SERVER/api/v1/catalog/items/$cid" 2>/dev/null \
    | python3 -c "import sys,json; d=json.load(sys.stdin); vs=d.get('playback_variants',[]); print(vs[0]['default_file_id'] if vs else '')" 2>/dev/null || echo "")
  if [[ -n "$fid" && "$fid" != "" ]]; then
    FILES+=("$fid"); TITLES+=("$title"); TYPES+=("episode")
    echo "  episode:   $title → file_id=$fid"
  fi
done < <(curl_json "$SERVER/api/v1/catalog?library_id=$SERIES_LIBRARY_ID&limit=500&type=episode" 2>/dev/null \
  | python3 -c "import sys,json; [print(f\"{i['content_id']}|{i['title']}\") for i in json.load(sys.stdin).get('items',[])]" 2>/dev/null)

NFILES=${#FILES[@]}
echo ""
echo "  Discovered $NFILES files"
echo ""

if [[ $SAMPLE_SIZE -gt 0 && $NFILES -gt $SAMPLE_SIZE ]]; then
  mapfile -t order < <(seq 0 $((NFILES-1)) | awk -v seed="$SEED" 'BEGIN{srand(seed)}{print rand()"\t"$0}' | sort -n -k1,1 | cut -f2)
  NEW_FILES=(); NEW_TITLES=(); NEW_TYPES=()
  for j in "${order[@]:0:$SAMPLE_SIZE}"; do
    NEW_FILES+=("${FILES[$j]}"); NEW_TITLES+=("${TITLES[$j]}"); NEW_TYPES+=("${TYPES[$j]}")
  done
  FILES=("${NEW_FILES[@]}"); TITLES=("${NEW_TITLES[@]}"); TYPES=("${NEW_TYPES[@]}")
  NFILES=${#FILES[@]}
  echo "  Random sample: $NFILES of ${#order[@]} discovered (seed=$SEED)"
  echo ""
fi

if [[ $NFILES -eq 0 ]]; then
  echo "ERROR: no files discovered — check server and API key" >&2
  exit 1
fi

# ── Phase 2: Cold start ──────────────────────────────────────────────────────

echo "=== Phase 2: Cold start (cooldown=${COOLDOWN}s between files) ==="
echo ""

COLD_TTFB=()
COLD_TOTAL=()
COLD_OUTCOME=()
COLD_SESSION=()
COLD_DIAG=()

for i in "${!FILES[@]}"; do
  fid="${FILES[$i]}"
  title="${TITLES[$i]}"
  ftype="${TYPES[$i]}"
  attempt="bench-cold-${fid}-$(date +%s)"

  if [[ $i -gt 0 ]]; then
    echo "  ⏳ cooling down ${COOLDOWN}s..."
    sleep "$COOLDOWN"
  fi

  printf "  [%2d/%d] %-36s " "$((i+1))" "$NFILES" "$title ($ftype)"

  result=$(fire_start "$fid" "$attempt" "$CTX_PLAYABLE" "cold")
  ttfb_ms=$(echo "$result" | awk '{print $1}')
  total_ms=$(echo "$result" | awk '{print $2}')
  outcome=$(echo "$result" | awk '{print $3}')
  sid=$(echo "$result" | awk '{print $4}')
  reason=$(echo "$result" | awk '{print $5}')

  COLD_TTFB+=("$ttfb_ms")
  COLD_TOTAL+=("$total_ms")
  COLD_OUTCOME+=("$outcome")
  COLD_SESSION+=("$sid")

  if [[ "$outcome" != "playable" ]]; then
    # Terminal diagnosis: keep the raw response and extract the fields that
    # disambiguate the branch. terminal.message (not reason) names which of
    # the five adaptation_unavailable branches fired; the plan fields name
    # the rejected source/recipe. Never logs tokens or signed URLs.
    diag_file="$DIAG_DIR/cold-${fid}.json"
    cp "$TMPDIR/resp-cold-${fid}-${attempt}.json" "$diag_file" 2>/dev/null || true
    diag_summary=$(python3 - "$diag_file" <<'PYEOF' 2>/dev/null || echo "diag_unavailable"
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("diag_unavailable"); sys.exit(0)
t = d.get("terminal") or {}
plan = d.get("playback_plan") or {}
sel = plan.get("selected_tracks") or {}
src = plan.get("source") or {}
out = [
    "terminal_reason=" + str(t.get("reason", "")),
    "terminal_message=" + str(t.get("message", ""))[:160],
    "delivery=" + str(plan.get("delivery", "")),
    "decision=" + str(plan.get("decision_reason", "")),
    "video=" + str(src.get("video_codec", "")) + "/" + str(src.get("video_range", "")) + "/" + str(src.get("container", "")),
    "audio=" + str(src.get("audio_codec", "")) + "/" + str(src.get("audio_channels", "")),
]
print(" | ".join(out))
PYEOF
)
    COLD_DIAG+=("$diag_summary")
    printf "ttfb=%6sms  total=%6sms  %s (%s)\n" "$ttfb_ms" "$total_ms" "$outcome" "$reason"
    printf "    ↳ diag: %s\n" "$diag_summary"
    printf "    ↳ raw:  %s\n" "$diag_file"
  elif [[ -n "$reason" ]]; then
    COLD_DIAG+=("")
    printf "ttfb=%6sms  total=%6sms  %s (%s)\n" "$ttfb_ms" "$total_ms" "$outcome" "$reason"
  else
    COLD_DIAG+=("")
    printf "ttfb=%6sms  total=%6sms  %s\n" "$ttfb_ms" "$total_ms" "$outcome"
  fi

  stop_session "$sid"
done

echo ""

# ── Phase 3: Warm start ──────────────────────────────────────────────────────

echo "=== Phase 3: Warm start (reps=$WARM_REPS per file) ==="
echo ""

WARM_VALS=()  # all warm TTFB values across all files (for aggregate)

for i in "${!FILES[@]}"; do
  fid="${FILES[$i]}"
  title="${TITLES[$i]}"
  ftype="${TYPES[$i]}"

  printf "  [%2d/%d] %-36s " "$((i+1))" "$NFILES" "$title ($ftype)"

  # Prime the memo
  prime_body=$(build_body "$fid" "bench-warm-prime-${fid}-$(date +%s)" "$CTX_PLAYABLE")
  curl -s \
    -X POST "$SERVER/api/v1/playback/start" \
    -H "$AUTH" \
    -H "Content-Type: application/json" \
    -H "X-Profile-Id: $PROFILE_ID" \
    -H "X-Device-Id: bench-warm-${fid}" \
    -o /dev/null \
    -d "$prime_body" >/dev/null 2>&1 || true

  vals_file="$TMPDIR/warm-vals-${fid}.txt"
  > "$vals_file"

  for r in $(seq 1 "$WARM_REPS"); do
    attempt="bench-warm-${fid}-${r}-$(date +%s%N)"
    body=$(build_body "$fid" "$attempt" "$CTX_PLAYABLE")
    ttfb=$(curl -s \
      -X POST "$SERVER/api/v1/playback/start" \
      -H "$AUTH" \
      -H "Content-Type: application/json" \
      -H "X-Profile-Id: $PROFILE_ID" \
      -H "X-Device-Id: bench-warm-${fid}" \
      -o /dev/null \
      -w "%{time_starttransfer}" \
      -d "$body" 2>/dev/null || echo "0")
    ttfb_ms=$(awk -v v="$ttfb" 'BEGIN{printf "%.1f", v*1000}' 2>/dev/null || echo "0")
    echo "$ttfb_ms" >> "$vals_file"
  done

  # Print per-file stats
  cat "$vals_file" | xargs python3 -c "
import sys
vals = sorted(float(x) for x in sys.argv[1:] if x.strip())
n = len(vals)
if n == 0:
    print('no data'); sys.exit(0)
avg = sum(vals) / n
p50 = vals[n//2]
p95 = vals[int(n*0.95)] if n > 1 else vals[0]
p99 = vals[int(n*0.99)] if n > 1 else vals[0]
print(f'p50={p50:.0f}  p95={p95:.0f}  p99={p99:.0f}  avg={avg:.0f}  min={vals[0]:.0f}  max={vals[-1]:.0f}')
" 2>/dev/null

  # Collect for aggregate
  while IFS= read -r v; do
    WARM_VALS+=("$v")
  done < "$vals_file"

  if [[ $i -lt $((NFILES - 1)) ]]; then
    echo "  ⏳ cooling down ${COOLDOWN}s..."
    sleep "$COOLDOWN"
  fi
done

echo ""

# ── Phase 4: Summary ────────────────────────────────────────────────────────

echo "═══════════════════════════════════════════════════════════════════════════"
echo "  VIRTUAL PLAYBACK BENCHMARK RESULTS"
echo "═══════════════════════════════════════════════════════════════════════════"
echo ""
echo "  Server:       $SERVER"
echo "  Profile:      $PROFILE_ID"
echo "  Files:        $NFILES"
echo "  Warm reps:    $WARM_REPS per file"
echo "  Cooldown:     ${COOLDOWN}s"
echo "  Date:         $(date -Iseconds)"
echo ""
echo "  ┌──────────────────────────────────────┬──────────┬────────┬────────┬────────┬────────┬────────┬────────────────┐"
echo "  │ FILE                                 │ TYPE     │COLD ms │W min   │W p50   │W p95   │W p99   │ OUTCOME        │"
echo "  ├──────────────────────────────────────┼──────────┼────────┼────────┼────────┼────────┼────────┼────────────────┤"

ALL_COLD=""
ALL_WARM=""

for i in "${!FILES[@]}"; do
  title="${TITLES[$i]}"
  ftype="${TYPES[$i]}"
  cold="${COLD_TTFB[$i]}"
  outcome="${COLD_OUTCOME[$i]}"
  vals_file="$TMPDIR/warm-vals-${FILES[$i]}.txt"

  warm_min="--"; warm_p50="--"; warm_p95="--"; warm_p99="--"
  if [[ -f "$vals_file" ]]; then
    read -r warm_min warm_p50 warm_p95 warm_p99 <<< $(cat "$vals_file" | xargs python3 -c "
import sys
vals = sorted(float(x) for x in sys.argv[1:] if x.strip())
n = len(vals)
if n == 0: print('-- -- -- --'); sys.exit(0)
p50 = vals[n//2]
p95 = vals[int(n*0.95)] if n > 1 else vals[0]
p99 = vals[int(n*0.99)] if n > 1 else vals[0]
print(f'{vals[0]:.0f} {p50:.0f} {p95:.0f} {p99:.0f}')
" 2>/dev/null || echo "-- -- -- --")
  fi

  ALL_COLD="$ALL_COLD $cold"
  short_title="${title:0:36}"
  printf "  │ %-36s │ %-8s │ %6s │ %6s │ %6s │ %6s │ %6s │ %-14s │\n" \
    "$short_title" "$ftype" "$cold" "$warm_min" "$warm_p50" "$warm_p95" "$warm_p99" "$outcome"
done

echo "  └──────────────────────────────────────┴──────────┴────────┴────────┴────────┴────────┴────────┴────────────────┘"

# Aggregate
echo ""
echo "  ── Aggregate ──"
echo ""

echo -n "  Cold start:  "
ALL_COLD_TRIMMED=$(echo "$ALL_COLD" | xargs)
echo "$ALL_COLD_TRIMMED" | xargs python3 -c "
import sys
vals = sorted(float(x) for x in sys.argv[1:] if x.strip())
if not vals: print('no data'); sys.exit(0)
n = len(vals)
print(f'avg={sum(vals)/n:.0f}  min={vals[0]:.0f}  max={vals[-1]:.0f}  n={n}')
" 2>/dev/null

echo -n "  Warm start:  "
echo "${WARM_VALS[*]}" | xargs python3 -c "
import sys
vals = sorted(float(x) for x in sys.argv[1:] if x.strip())
if not vals: print('no data'); sys.exit(0)
n = len(vals)
avg = sum(vals) / n
p50 = vals[n//2]
p95 = vals[int(n*0.95)] if n > 1 else vals[0]
p99 = vals[int(n*0.99)] if n > 1 else vals[0]
print(f'avg={avg:.0f}  min={vals[0]:.0f}  max={vals[-1]:.0f}  p50={p50:.0f}  p95={p95:.0f}  p99={p99:.0f}  n={n}')
" 2>/dev/null

cold_avg=$(echo "$ALL_COLD_TRIMMED" | xargs python3 -c "
import sys
vals = [float(x) for x in sys.argv[1:] if x.strip()]
print(f'{sum(vals)/len(vals):.0f}' if vals else '0')
" 2>/dev/null || echo "0")

warm_avg=$(echo "${WARM_VALS[*]}" | xargs python3 -c "
import sys
vals = [float(x) for x in sys.argv[1:] if x.strip()]
print(f'{sum(vals)/len(vals):.0f}' if vals else '0')
" 2>/dev/null || echo "0")

speedup=$(python3 -c "
c, w = float('$cold_avg'), float('$warm_avg')
print(f'{c/w:.0f}x' if w > 0 else 'N/A')
" 2>/dev/null || echo "N/A")

echo ""
echo "  Speedup:     $speedup (cold → warm)"
echo ""
echo "  Outcomes:"
printf "    %-20s %s\n" "playable" "$(for o in "${COLD_OUTCOME[@]}"; do [[ "$o" == "playable" ]] && echo x; done | wc -l | xargs)/$NFILES"
printf "    %-20s %s\n" "adaptation_unavailable" "$(for o in "${COLD_OUTCOME[@]}"; do [[ "$o" == "adaptation_unavailable" ]] && echo x; done | wc -l | xargs)/$NFILES"
printf "    %-20s %s\n" "error" "$(for o in "${COLD_OUTCOME[@]}"; do [[ "$o" == "error" ]] && echo x; done | wc -l | xargs)/$NFILES"
echo ""
# ── Phase 5: Terminal diagnosis ────────────────────────────────────────────
nonplay=0
for o in "${COLD_OUTCOME[@]}"; do [[ "$o" != "playable" ]] && nonplay=$((nonplay+1)); done
if [[ $nonplay -gt 0 ]]; then
  echo "  ── Terminal diagnosis ($nonplay non-playable cold start) ──"
  echo ""
  for i in "${!FILES[@]}"; do
    outcome="${COLD_OUTCOME[$i]}"
    [[ "$outcome" == "playable" ]] && continue
    printf "  [%s] %s (%s)\n" "${FILES[$i]}" "${TITLES[$i]}" "$outcome"
    printf "    %s\n" "${COLD_DIAG[$i]}"
    printf "    raw: %s/cold-%s.json\n" "$DIAG_DIR" "${FILES[$i]}"
  done
  echo ""
  echo "  Raw responses in $DIAG_DIR — attach terminal_message + plan fields"
  echo "  when filing the fix; branch map: plan_v3.go:904 (route-exhausted),"
  echo "  :1017 (audio-source), :1059 (progressive-audio-recipe),"
  echo "  :1394/:1407 (HLS-recipe)."
  echo ""
fi
echo "═══════════════════════════════════════════════════════════════════════════"
