-- Change data capture: the relay streams inserts into the outbox table from
-- the PostgreSQL write-ahead log (logical replication, pgoutput) instead of
-- polling it. Only inserts are published; the relay deletes rows once the
-- broker has them, and those deletes are not streamed.
CREATE PUBLICATION networkai_outbox FOR TABLE outbox WITH (publish = 'insert');

-- Posts created per day, maintained by the "post-stats" consumer group. It is
-- the example of a team that joins later and replays history from the log.
CREATE TABLE daily_post_counts (
    day   DATE   PRIMARY KEY,
    posts BIGINT NOT NULL
);

-- Idempotent-consumer bookkeeping: a post is counted at most once however
-- many times its events are delivered or replayed.
CREATE TABLE stats_counted_posts (
    post_id UUID PRIMARY KEY
);
