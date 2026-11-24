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

pending() { curl -s "$API/readyz" | jq -r .outbox.pending; }
