#!/usr/bin/env bash
# Loads a generated dataset into PostgreSQL. Posts are inserted directly
# (bypassing the outbox), so run `networkai reindex` afterwards to index them.
# Every seeded user's password is "password123".
#
#   python3 tools/synth/generate.py --out data/synth
#   tools/synth/load.sh data/synth
set -euo pipefail
DIR=${1:-data/synth}
DB=${DATABASE_URL:-postgres://networkai:networkai@localhost:5432/networkai?sslmode=disable}
HASH='$2a$10$Ad6sB5gt9zHGjV36oU/iJ.la5UOErm9wWFaxlSFJw/S6ORVnMusu2' # bcrypt("password123")

psql "$DB" -v ON_ERROR_STOP=1 -q <<SQL
CREATE TEMP TABLE s_users (id bigint, username text);
CREATE TEMP TABLE s_posts (id uuid, author_id bigint, message text, created_at timestamptz);
CREATE TEMP TABLE s_inter (user_id bigint, post_id uuid, kind text, created_at timestamptz);
\copy s_users FROM '$DIR/users.csv' CSV HEADER
\copy s_posts FROM '$DIR/posts.csv' CSV HEADER
\copy s_inter FROM '$DIR/interactions.csv' CSV HEADER

-- Map generator user ids to real ids via username, so this works on a
-- database that already has other users.
INSERT INTO users (username, password_hash)
  SELECT username, '$HASH' FROM s_users ON CONFLICT (username) DO NOTHING;

-- Shift the generated timeline so its newest post is "now"; otherwise every
-- post would fall outside the feed's recent-candidate window.
CREATE TEMP TABLE s_shift AS SELECT now() - max(created_at) AS d FROM s_posts;
UPDATE s_posts SET created_at = created_at + (SELECT d FROM s_shift);
UPDATE s_inter SET created_at = created_at + (SELECT d FROM s_shift);

INSERT INTO posts (id, author_id, message, media_type, created_at, updated_at)
  SELECT p.id, u.id, p.message, 'none', p.created_at, p.created_at
  FROM s_posts p JOIN s_users su ON su.id = p.author_id JOIN users u ON u.username = su.username
  ON CONFLICT (id) DO NOTHING;
-- Likes only: impressions ("view" rows) are not logged by the API.
INSERT INTO likes (user_id, post_id, created_at)
  SELECT u.id, i.post_id, i.created_at
  FROM s_inter i JOIN s_users su ON su.id = i.user_id JOIN users u ON u.username = su.username
  JOIN posts p ON p.id = i.post_id
  WHERE i.kind = 'like'
  ON CONFLICT DO NOTHING;
ANALYZE users; ANALYZE posts; ANALYZE likes;
SQL
psql "$DB" -Atc "SELECT 'users=' || count(*) FROM users; SELECT 'posts=' || count(*) FROM posts; SELECT 'likes=' || count(*) FROM likes;"
