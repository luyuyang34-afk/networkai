#!/usr/bin/env bash
# Compares the polling outbox worker (one loop does search indexing AND
# timeline fan-out) with CDC + per-job consumer groups, on three questions:
#   1. how long until a new post is searchable (normal conditions)
#   2. does a Redis outage delay search indexing?
#   3. does a celebrity's in-flight fan-out delay search indexing?
#
# Usage (stack running via docker compose):  tools/experiments/coupling.sh
# Run from the repo root. Prints a Markdown table.
set -euo pipefail
cd "$(dirname "$0")/../.."
. scripts/lib.sh
FANS=${FANS:-100000}
psql_() { docker compose exec -T postgres psql -U networkai -d networkai -Atq -c "$1"; }

ms() { date +%s%3N; }

# searchable_ms TOKEN MESSAGE_WORD: post, then poll search until found; prints ms.
post_and_time() {
  local tok=$1 word=$2 t0 id
  t0=$(ms)
  id=$(curl -sf -X POST "$API/posts" -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
       -d "{\"message\":\"coupling probe $word\"}" | jq -r .id)
  local deadline=$(( t0 + ${PROBE_TIMEOUT_MS:-60000} ))
  while [ "$(ms)" -lt "$deadline" ]; do
    if [ "$(curl -s "$API/posts/search?q=$word" | jq '.items | length')" -ge 1 ]; then
      echo "  probe $word: $(( $(ms) - t0 )) ms" >&2
      echo $(( $(ms) - t0 )); return
    fi
    sleep 0.05
  done
  echo "  probe $word: not searchable within the probe timeout" >&2
  echo "> $(( ${PROBE_TIMEOUT_MS:-60000} / 1000 )) s (blocked)"
}

p50() { sort -n | awk '{a[NR]=$1} END{print a[int((NR+1)/2)]}'; }

seed_celebrity() {
  # 1 celebrity with $FANS followers, all with an active (cached) timeline.
  login celeb password123 >/dev/null
  psql_ "INSERT INTO users (username, password_hash)
           SELECT 'cfan_' || g, 'x' FROM generate_series(1, $FANS) g ON CONFLICT DO NOTHING;
         INSERT INTO follows (follower_id, followee_id)
           SELECT f.id, c.id FROM users f, users c WHERE f.username LIKE 'cfan\_%' AND c.username = 'celeb'
           ON CONFLICT DO NOTHING;
         UPDATE users SET follower_count = (SELECT count(*) FROM follows WHERE followee_id = users.id)
           WHERE username = 'celeb';"
  psql_ "SELECT 'ZADD tl:' || id || ' 0 __built__' FROM users WHERE username LIKE 'cfan\_%'" \
    | docker compose exec -T redis redis-cli --pipe >/dev/null
}

# measure_mode TAG LABEL. TAG goes into search words, so it must be a plain
# token: an earlier version used LABEL (with spaces and parentheses), built
# malformed search URLs, and every probe "timed out".
measure_mode() {
  local tag=$1 mode=$2 tok celeb normal
  tok=$(login probe_user password123)
  celeb=$(login celeb password123)

  # 1. normal conditions: median of 10 probes
  local lat=()
  for i in $(seq 1 10); do lat+=("$(post_and_time "$tok" "${tag}n$i$RANDOM")"); done
  normal=$(printf '%s\n' "${lat[@]}" | p50)

  # 2. Redis down while posting. The first post after Redis goes down
  # still gets indexed by the polling worker (it indexes, THEN fails the
  # fan-out and pauses), so post a trigger first and measure the post after.
  docker compose stop redis >/dev/null 2>&1
  curl -sf -X POST "$API/posts" -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
    -d '{"message":"trigger while redis is down"}' >/dev/null
  sleep 2
  local redis_down
  redis_down=$(post_and_time "$tok" "${tag}r$RANDOM")
  docker compose start redis >/dev/null 2>&1
  echo "  ($mode) redis back up" >&2
  sleep 3

  # 3. celebrity post (pure push: threshold is set very high) then a probe
  curl -sf -X POST "$API/posts" -H "Authorization: Bearer $celeb" -H 'Content-Type: application/json' \
    -d '{"message":"celebrity announcement"}' >/dev/null
  local during_fanout
  during_fanout=$(post_and_time "$tok" "${tag}c$RANDOM")
  fmt() { case "$1" in *blocked*) echo "$1" ;; *) echo "$1 ms" ;; esac; }
  echo "| $mode | $(fmt "$normal") | $(fmt "$redis_down") | $(fmt "$during_fanout") |"
}

COMPOSE_WORKERS="relay consumer-search consumer-timeline consumer-stats"
bold "Seeding a celebrity with $FANS followers (active timelines)..." >&2
seed_celebrity

echo "| Mode | New post searchable (P50 of 10) | ...while Redis is down | ...right after a $FANS-follower fan-out |"
echo "|---|---|---|---|"

# Poll mode: stop CDC workers, run the single polling worker.
docker compose stop $COMPOSE_WORKERS >/dev/null 2>&1
CELEBRITY_THRESHOLD=1000000000 docker compose --profile poll up -d indexer >/dev/null 2>&1
sleep 5
measure_mode poll "Polling worker (index + fan-out in one loop)"
docker compose --profile poll stop indexer >/dev/null 2>&1

# CDC mode.
CELEBRITY_THRESHOLD=1000000000 docker compose up -d $COMPOSE_WORKERS >/dev/null 2>&1
sleep 8
measure_mode cdc "CDC + consumer groups"
