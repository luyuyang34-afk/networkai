-- Sharded like counters. A viral post receives many likes per second; one
-- counter row per post would serialise every like on that row's lock (a hot
-- key). Each like instead increments one of N shard rows chosen at random,
-- and readers sum the N rows. Updated in the same transaction as the likes
-- row, so the total is always exact.
CREATE TABLE post_like_counts (
    post_id UUID     NOT NULL REFERENCES posts (id),
    shard   SMALLINT NOT NULL,
    n       BIGINT   NOT NULL,
    PRIMARY KEY (post_id, shard)
);

-- Backfill existing likes into shard 0.
INSERT INTO post_like_counts (post_id, shard, n)
SELECT post_id, 0, count(*) FROM likes GROUP BY post_id;
