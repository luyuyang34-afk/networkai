# Shared helpers for the demo scripts. Requires curl and jq.
API=${API:-http://localhost:8080}
command -v jq >/dev/null || { echo "jq is required (brew install jq / apt install jq)"; exit 1; }

bold() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# login USER PASSWORD -> prints a token, signing the user up first if needed
login() {
  curl -s -o /dev/null -X POST "$API/users" -H 'Content-Type: application/json' \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}"
  curl -sf -X POST "$API/auth/login" -H 'Content-Type: application/json' \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}" | jq -r .token
}

# post TOKEN MESSAGE -> prints the new post id
post() {
  curl -sf -X POST "$API/posts" -H "Authorization: Bearer $1" -F "message=$2" | jq -r .id
}

# searchable QUERY N: wait (up to 60 s) until search returns at least N items.
# Waiting on search itself works in both outbox modes; with CDC the outbox
# table empties as soon as the relay publishes, before indexing finishes.
searchable() {
  for _ in $(seq 1 120); do
    [ "$(curl -s "$API/posts/search?$1" | jq '.items | length' 2>/dev/null || echo 0)" -ge "$2" ] && return 0
    sleep 0.5
  done
  echo "timed out waiting for search: $1" >&2; return 1
}
