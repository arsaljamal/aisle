#!/usr/bin/env bash
# Runs every break switch through the control room: break it, wait until the
# alert would really fire, page the investigator, then repair. Results land in
# the control room scoreboard (incidents/investigations.jsonl).
set -uo pipefail
cd "$(dirname "$0")/.."
H=${AISLE_ADMIN:-http://localhost:7071}
live() { curl -s "$H/live"; }
# condition <python expression over s = recent buckets with traffic>
until_live() {
  until [ "$(live | python3 -c "import json,sys;s=[p for p in json.load(sys.stdin)['series'] if p['rps']][-30:];print(int(bool(s) and ($1)))")" = 1 ]; do sleep 5; done
}
run() { # fault alert condition
  local fault=$1 alert=$2 cond=$3
  echo "== $fault: breaking, waiting for the $alert alert"
  ./bin/aisle break "$fault" >/dev/null
  if [ "$fault" = llm-down ]; then
    for i in $(seq 1 12); do curl -s -m 10 "$H/ask?q=sofa" >/dev/null; sleep 5; done
  else
    until_live "$cond"
  fi
  curl -sN -m 1200 "$H/investigate?alert=$alert" | grep -A1 '^event: done' | tail -1 |
    python3 -c "import json,sys;d=json.loads(sys.stdin.read()[6:]);print('   verdict:',d['verdict'] or '-','| seconds:',round(d['seconds']),'| queries:',d['queries'],'| sent back:',d['nudges'])"
  ./bin/aisle break "$fault" off >/dev/null
  echo "   repaired; letting it recover"; sleep 75
}
curl -s -X POST "$H/admin/traffic?rps=4" >/dev/null; sleep 30
run slow-embed     latency   "sum(p['p95_ms']>1000 for p in s)>=25"
run db-pool        latency   "sum(p['p95_ms']>1000 for p in s)>=25"
run model-mismatch relevance "sum(1 for p in s if p['ndcg'] is not None and p['ndcg']<.5)>=20"
run bad-deploy     errors    "sum(p['err']>.05 for p in s)>=20"
run llm-down       ask       "True"
echo ALL DONE
