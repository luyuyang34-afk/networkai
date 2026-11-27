# Data model

## PostgreSQL (source of truth)

```mermaid
erDiagram
    users ||--o{ posts : writes
    users ||--o{ likes : gives
    posts ||--o{ likes : receives
    posts ||--o{ post_like_counts : "counted in (16 shards)"
    users ||--o{ follows : "follower"
    users ||--o{ follows : "followee"
    posts ||--o{ outbox : "change events"

    users {
        bigint id PK
        text username UK "a-z 0-9 _ , 3-20 chars"
        text password_hash "bcrypt"
        bigint follower_count "kept in the follow transaction"
        timestamptz created_at
    }
    posts {
        uuid id PK
        bigint author_id FK
        text message "max 5000 chars"
        text media_type "none, image, video"
        text media_key
        text media_url
        bigint version "bumped on every change; external version in ES"
        timestamptz created_at
        timestamptz updated_at
        timestamptz deleted_at "soft delete"
    }
    likes {
        bigint user_id PK
        uuid post_id PK
        timestamptz created_at
    }
    post_like_counts {
        uuid post_id PK
        smallint shard PK "0-15, picked at random per like"
        bigint n
    }
    follows {
        bigint follower_id PK
        bigint followee_id PK
        timestamptz created_at
    }
    outbox {
        bigint id PK
        text aggregate_type "post"
        text aggregate_id "post id"
        text event_type "post.changed"
        timestamptz created_at
    }
```

Supporting tables:
- `daily_post_counts` and `stats_counted_posts` belong to the `post-stats`
  consumer. The second is its dedupe table, which makes replays safe.
- `schema_migrations` records which migrations have run.
- Outbox rows are deleted once Kafka has them.
- The publication `networkai_outbox` covers outbox inserts only, so the
  relay never sees the deletes.

Key design points:
- **Every post write inserts an outbox row in the same transaction**, so a
  change can never be committed without its event.
- `posts.version` is what makes indexing idempotent and order-independent:
  Elasticsearch only accepts a document with a higher external version.
- Like counts are the sum of up to 16 `post_like_counts` rows. A viral
  post's likes spread over 16 row locks instead of one.

## Derived stores (rebuildable)

| Store | Key | Value | Rebuilt from |
|---|---|---|---|
| Redis | `tl:{user_id}` | sorted set of post ids, scored by time; capped at 800; a marker entry means "built" | followees' posts in PostgreSQL (primary) |
| Redis | `post:{id}` | cached post JSON with its load time and expiry (for XFetch); "not found" cached for 5 s | PostgreSQL primary, on the next miss |
| Redis | `post-lease:{id}` | lease token, 2 s TTL | nothing (transient) |
| Elasticsearch | index behind alias `posts` | message (analysed + exact), author, media type, time, embedding vector | `networkai reindex`, or a blue/green rebuild |
| Kafka | topic `post-events` | `{id, aggregate_type, aggregate_id, event_type, created_at}`, keyed by post id | the outbox via the relay. Consumers re-read PostgreSQL, so events carry ids, not data |

## Sharded post store (subsystem, separate databases)

```mermaid
erDiagram
    shard_map {
        smallint lshard PK "0-63"
        text node "physical node name"
        bigint epoch "bumped on every move"
    }
    shard_owner {
        smallint lshard PK
        boolean owned "checked FOR SHARE by every write"
    }
    shard_posts {
        uuid id PK "UUIDv7 layout, byte 9 = logical shard"
        smallint lshard
        bigint author_id
        text message
        bigint version
        timestamptz created_at
        timestamptz deleted_at
        xid8 writer_xid "drives incremental copy"
    }
```

- `shard_map` lives in the directory database.
- Each node has its own `shard_owner` and `shard_posts`.
- `hash(author_id) % 64` chooses the logical shard; the directory maps it
  to a node.
