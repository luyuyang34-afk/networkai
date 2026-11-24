#!/usr/bin/env bash
# Failure-injection demo: take Elasticsearch down, keep using the app, bring it
# back, and show that nothing was lost. Run after `make up`.
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/lib.sh

token=$(login demo_user password123)

bold "1. Stopping Elasticsearch"
docker compose stop elasticsearch >/dev/null

bold "2. Posting 5 times while search is down"
for i in 1 2 3 4 5; do
  echo "  created $(post "$token" "Outage post $i about kayaking")"
done

bold "3. Browsing still works (served from PostgreSQL):"
curl -s "$API/posts?author=demo_user&limit=3" | jq -r '.items[] | "  \(.message)"'

bold "4. Search degrades to a clean 503 instead of a crash:"
curl -s "$API/posts/search?q=kayaking" | jq -c .

bold "5. Outbox backlog (events waiting to be indexed):"
curl -s "$API/readyz" | jq .outbox

bold "6. Starting Elasticsearch again"
docker compose start elasticsearch >/dev/null
printf '  waiting for the backlog to drain'
until [ "$(pending)" = "0" ]; do printf .; sleep 1; done
sleep 1.2
echo

bold "7. All posts written during the outage are searchable:"
curl -s "$API/posts/search?q=kayaking&author=demo_user" | jq -r '.items[] | "  \(.message)"'
curl -s "$API/readyz" | jq .outbox
