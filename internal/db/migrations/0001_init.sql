-- PostgreSQL is the source of truth. Elasticsearch is a derived index that
-- can always be rebuilt from these tables (see `networkai reindex`).

CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username      TEXT        NOT NULL UNIQUE CHECK (username ~ '^[a-z0-9_]{3,20}$'),
    password_hash TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE posts (
    id          UUID PRIMARY KEY,
    author_id   BIGINT      NOT NULL REFERENCES users (id),
    message     TEXT        NOT NULL CHECK (char_length(message) <= 5000),
    media_type  TEXT        NOT NULL CHECK (media_type IN ('none', 'image', 'video')),
    media_key   TEXT,
    media_url   TEXT,
    -- Incremented on every change. Used as the Elasticsearch external version
    -- so stale or out-of-order index writes are rejected instead of applied.
    version     BIGINT      NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Soft delete: the row (and its version) survives so the indexer can
    -- propagate the deletion with a version newer than any earlier write.
    deleted_at  TIMESTAMPTZ
);

-- Keyset pagination: newest first, id as a tie-breaker.
CREATE INDEX posts_feed_idx   ON posts (created_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX posts_author_idx ON posts (author_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;

-- Transactional outbox: written in the same transaction as the post change,
-- consumed asynchronously by the indexer worker.
CREATE TABLE outbox (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    aggregate_type  TEXT        NOT NULL,
    aggregate_id    TEXT        NOT NULL,
    event_type      TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    processed_at    TIMESTAMPTZ,
    -- Set when the event exhausted its retries; needs operator attention.
    dead_at         TIMESTAMPTZ
);

CREATE INDEX outbox_pending_idx ON outbox (next_attempt_at, id)
    WHERE processed_at IS NULL AND dead_at IS NULL;
