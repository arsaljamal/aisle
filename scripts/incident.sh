#!/usr/bin/env bash
# Runs one break-it scenario end to end:
#   traffic on -> break switch on -> wait -> page Drill Down Harness -> save report -> switch off.
# Usage: scripts/incident.sh <fault> [minutes-to-wait]   (aisle serve must be running)
set -euo pipefail
fault=${1:?usage: scripts/incident.sh <fault> [minutes]}
wait_min=${2:-3}
line=$(awk -F'\t' -v f="$fault" '$1==f' ops/scenarios.tsv)
[ -n "$line" ] || { echo "unknown fault $fault (see ops/scenarios.tsv)"; exit 2; }
alert=$(cut -f2 <<<"$line"); keywords=$(cut -f3 <<<"$line")
out="incidents/$fault/$(date +%Y%m%d-%H%M)"; mkdir -p "$out"
DRILL=${DRILL:-drill}

# Ask traffic shares the local GPU with query embedding, so only send it when Ask is the subject.
ask_every=0; [ "$fault" = llm-down ] && ask_every=5
./bin/aisle load --rps 4 --ask-every "$ask_every" >"$out/load.log" 2>&1 &
load=$!
trap './bin/aisle break "$fault" off >/dev/null 2>&1 || true; kill $load 2>/dev/null || true' EXIT

echo "healthy traffic for 90 s ..."; sleep 90
./bin/aisle break "$fault"
date -u +%FT%TZ >"$out/fault_started_at"
echo "fault $fault on; waiting $wait_min min for the alert to be real ..."; sleep $((wait_min * 60))

echo "paging the harness: $alert"
start=$(date +%s)
set +e
"$DRILL" run --config ops/drill.yaml "$alert" >"$out/report.md" 2>"$out/transcript.log"
code=$?
set -e
secs=$(( $(date +%s) - start ))
found=no; grep -qiE "$keywords" <(sed -n '/Root cause/,/Evidence/p' "$out/report.md") && found=yes
printf 'fault=%s exit=%s seconds=%s root_cause_named=%s\n' "$fault" "$code" "$secs" "$found" | tee "$out/summary.txt"
