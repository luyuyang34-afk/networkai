#!/usr/bin/env bash
# Creates a few users and posts so search and browsing have something to show.
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/lib.sh

alice=$(login alice password123)
bob=$(login bob_k password123)

for m in "Sunrise run along the Hudson river" \
         "Best coffee shops near campus for studying" \
         "Notes on transactional outbox vs change data capture" \
         "Weekend hiking trip to Bear Mountain"; do
  post "$alice" "$m" >/dev/null
done
for m in "Running my first half marathon this spring" \
         "Café review: the espresso was excellent" \
         "Learning Elasticsearch analyzers and stemming"; do
  post "$bob" "$m" >/dev/null
done

bold "Seeded 7 posts. Waiting for the indexer..."
searchable "q=runs" 2

bold "Search 'runs' (stemming matches 'run' and 'Running'):"
curl -s "$API/posts/search?q=runs" | jq -r '.items[] | "  \(.author): \(.message)"'
bold "Search 'cafe' (ASCII folding matches 'Café'):"
curl -s "$API/posts/search?q=cafe" | jq -r '.items[] | "  \(.author): \(.message)"'
