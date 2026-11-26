-- Follow graph for the home timeline.
CREATE TABLE follows (
    follower_id BIGINT      NOT NULL REFERENCES users (id),
    followee_id BIGINT      NOT NULL REFERENCES users (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (follower_id, followee_id),
    CHECK (follower_id <> followee_id)
);

-- Fan-out reads "everyone who follows X" in id order.
CREATE INDEX follows_followee_idx ON follows (followee_id, follower_id);

-- Denormalised counter, updated in the same transaction as follows. It
-- decides whether an author's posts are pushed (fan-out on write) or pulled
-- (fan-out on read), so it must be cheap to read on every post.
ALTER TABLE users ADD COLUMN follower_count BIGINT NOT NULL DEFAULT 0;
