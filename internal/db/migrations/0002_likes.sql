-- Likes: the engagement signal for feed ranking and the label for training.
CREATE TABLE likes (
    user_id    BIGINT      NOT NULL REFERENCES users (id),
    post_id    UUID        NOT NULL REFERENCES posts (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, post_id)
);

-- Like counts per post (ranking feature) and "my recent likes" (user profile).
CREATE INDEX likes_post_idx ON likes (post_id);
CREATE INDEX likes_user_recent_idx ON likes (user_id, created_at DESC);
