# ADR 0005: Hybrid fan-out for the home timeline

- Status: accepted
- Date: 2026-11-26

## Context

Users can follow each other, and the home timeline (`GET /timeline`) shows
posts from followees, newest first. There are two classic ways to build it:

- **Fan-out on read (pull):** at read time, query the recent posts of
  everyone the user follows and merge them. Writes are free. Reads cost
  grows with the number of followees, and every timeline refresh hits the
  database.
- **Fan-out on write (push):** at post time, append the post id to each
  follower's precomputed timeline. Reads are a single cache lookup. Writes
  cost grows with the author's follower count: one post by an account with
  5 million followers is 5 million writes.

Real follower distributions are extremely skewed. Most accounts have a few
hundred followers; a few have millions. Each pure approach fails badly for
one end of that distribution.

## Decision

**Hybrid**, as described publicly for large social timelines (e.g.
Twitter's home timeline):

- Authors below `CELEBRITY_THRESHOLD` followers (default 10,000) are
  **pushed**. The outbox worker appends the post id to each follower's Redis
  sorted set (score = created_at), capped at 800 entries.
- Authors at or above the threshold are **pulled**. At read time, their
  recent posts are fetched from PostgreSQL (one indexed query across all
  followed celebrities) and merged with the cached timeline.
- **Redis is a cache, never the source of truth.**
  - A missing timeline is rebuilt from PostgreSQL on first read.
  - Fan-out only touches timelines that already exist (an atomic Lua
    "if exists then add and trim"), so inactive users cost nothing.
  - Timelines expire after 7 days without reads.
- Follow and unfollow invalidate the follower's timeline. It is rebuilt
  with the new followee set on the next read.
- Fan-out is idempotent (same member, same score; removing a missing member
  is a no-op), so outbox replays are safe. Deletes remove the id from
  followers' timelines, and reads also drop deleted posts during hydration.

## Alternatives considered

| Option | Why not |
|---|---|
| Pure pull | Every refresh does an N-way merge in PostgreSQL. Fine at our size, but it is the scaling problem timelines exist to solve. |
| Pure push | Write amplification proportional to follower count; measured below. |
| Push to all followers including inactive ones | Most write cost would go to users who never read it. |
| Timelines in PostgreSQL | Works, but a sorted-set cache with trimming is the natural fit for a bounded, recency-ordered list. |

## Consequences

- The celebrity problem is bounded: a celebrity post costs **zero** fan-out
  writes. The price is one extra PostgreSQL query per timeline read for users
  who follow celebrities. Results are in
  [timeline-experiment.md](../timeline-experiment.md).
- An author crossing the threshold changes path. Previously pushed posts stay
  in timelines, and the merge dedupes by id.
- Fan-out runs in the **same outbox worker** as search indexing. A slow
  fan-out delays indexing and vice versa, and a Redis outage pauses search
  indexing too. This coupling is measured in the experiment and addressed by
  the CDC + message queue change (one consumer group per job).
