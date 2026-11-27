package shard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Mover moves one logical shard to another node while it keeps serving.
//
//  1. Prepare: the target records owned = false, so it refuses writes for
//     the shard until cutover.
//  2. Copy rounds, with no locks held on the source:
//     - round 0 copies every row of the shard;
//     - each later round copies only rows written since the previous round
//     began, until the remaining delta is small.
//     Copies upsert by version, so repeating one is harmless.
//  3. Cutover (the fence):
//     - lock the shard's shard_owner row on the source (FOR UPDATE). This
//     waits for in-flight writes and blocks new ones;
//     - copy the last delta;
//     - mark the source not-owner and commit;
//     - mark the target owner;
//     - update the directory.
//     Writes for this one logical shard pause for a few milliseconds.
//     Other shards never pause.
//  4. Cleanup: delete the shard's rows from the source.
//
// "Rows written since round k began" must not miss a transaction that was
// still running when round k read, then committed with an older position.
// So rows record the writing transaction id (writer_xid). Each round starts
// from the previous round's snapshot xmin: every transaction older than
// that had finished, so its rows were visible to, and copied by, that
// round.
//
// Failure safety: the source gives up ownership (committed) before the
// target takes it. A crash in between leaves the shard owned by nobody.
// Writes for it fail until the move is re-run, which is idempotent. There
// is never a moment where two nodes accept writes for the same shard.
type Mover struct {
	R *Router
	// SmallDelta ends the copy rounds when a round copies at most this many
	// rows (default 200). MaxRounds caps them (default 8).
	SmallDelta, MaxRounds int
	// Naive skips the fence and the final delta: copy, then flip the map.
	// It exists only to show what the fence prevents (lost writes).
	Naive bool
	// AfterCopy runs between the copy rounds and the cutover (tests).
	AfterCopy func()
	// KeepSource skips step 4.
	KeepSource bool
}

type MoveStats struct {
	LShard     int
	From, To   string
	Rows       int           // rows copied, all rounds
	Rounds     int           // copy rounds before the fence
	FenceRows  int           // rows copied while writes were blocked
	Fence      time.Duration // writes to this shard blocked
	Total      time.Duration
	Deleted    int64
	NaiveMoved bool
}

func (m *Mover) Move(ctx context.Context, l int, target string) (MoveStats, error) {
	st := MoveStats{LShard: l, To: target, NaiveMoved: m.Naive}
	start := time.Now()
	if err := m.R.Dir.QueryRow(ctx, `SELECT node FROM shard_map WHERE lshard = $1`, l).Scan(&st.From); err != nil {
		return st, fmt.Errorf("read shard map: %w", err)
	}
	if st.From == target {
		return st, nil
	}
	src, dst := m.R.Nodes[st.From], m.R.Nodes[target]
	if src == nil || dst == nil {
		return st, fmt.Errorf("unknown node in move %s -> %s", st.From, target)
	}

	// 1. Prepare.
	if _, err := dst.Exec(ctx, `INSERT INTO shard_owner VALUES ($1, false)
		ON CONFLICT (lshard) DO UPDATE SET owned = false`, l); err != nil {
		return st, err
	}

	// 2. Copy rounds.
	horizon := "0"
	for round := 0; ; round++ {
		n, next, err := copyRound(ctx, src, dst, l, horizon)
		if err != nil {
			return st, fmt.Errorf("copy round %d: %w", round, err)
		}
		st.Rows += n
		st.Rounds++
		horizon = next
		if n <= orInt(m.SmallDelta, 200) || st.Rounds >= orInt(m.MaxRounds, 8) {
			break
		}
	}
	// The target's planner statistics still describe the table before the
	// bulk copy. On a new node that is an empty table: it plans a seq scan
	// plus sort for "newest posts", measured at 150-280 ms instead of
	// 0.2 ms. Refresh them before the target starts serving.
	if _, err := dst.Exec(ctx, `ANALYZE shard_posts`); err != nil {
		return st, fmt.Errorf("analyze target: %w", err)
	}
	if m.AfterCopy != nil {
		m.AfterCopy()
	}

	fenceStart := time.Now()
	if m.Naive {
		if _, err := src.Exec(ctx, `UPDATE shard_owner SET owned = false WHERE lshard = $1`, l); err != nil {
			return st, err
		}
		if _, err := dst.Exec(ctx, `UPDATE shard_owner SET owned = true WHERE lshard = $1`, l); err != nil {
			return st, err
		}
	} else {
		// 3. Cutover.
		err := pgx.BeginFunc(ctx, src, func(tx pgx.Tx) error { // READ COMMITTED
			if _, err := tx.Exec(ctx, `SELECT 1 FROM shard_owner WHERE lshard = $1 FOR UPDATE`, l); err != nil {
				return err
			}
			// Lock held: no write to this shard is in flight or can start,
			// so a fresh statement sees everything.
			rows, err := tx.Query(ctx, selectChanges, l, horizon)
			if err != nil {
				return err
			}
			batch, err := collect(rows)
			if err != nil {
				return err
			}
			if err := upsert(ctx, dst, batch); err != nil {
				return err
			}
			st.FenceRows = len(batch.ids)
			_, err = tx.Exec(ctx, `UPDATE shard_owner SET owned = false WHERE lshard = $1`, l)
			return err
		})
		if err != nil {
			return st, fmt.Errorf("cutover: %w", err)
		}
		if _, err := dst.Exec(ctx, `UPDATE shard_owner SET owned = true WHERE lshard = $1`, l); err != nil {
			return st, fmt.Errorf("cutover (shard %d owned by nobody until re-run): %w", l, err)
		}
		st.Rows += st.FenceRows
	}
	if _, err := m.R.Dir.Exec(ctx, `UPDATE shard_map SET node = $2, epoch = epoch + 1 WHERE lshard = $1`, l, target); err != nil {
		return st, fmt.Errorf("directory update (re-run the move): %w", err)
	}
	// Writes to this shard were blocked or retrying from the lock until
	// the directory names the new owner.
	st.Fence = time.Since(fenceStart)
	_ = m.R.Refresh(ctx)

	// 4. Cleanup. The source no longer owns the shard, so nothing reads
	// these rows.
	if !m.KeepSource {
		for {
			tag, err := src.Exec(ctx, `DELETE FROM shard_posts WHERE id IN
				(SELECT id FROM shard_posts WHERE lshard = $1 LIMIT 5000)`, l)
			if err != nil {
				return st, fmt.Errorf("cleanup: %w", err)
			}
			st.Deleted += tag.RowsAffected()
			if tag.RowsAffected() == 0 {
				break
			}
		}
		// Reclaim the dead rows (index scans skip them until then) and
		// refresh statistics. VACUUM does not block reads or writes.
		if _, err := src.Exec(ctx, `VACUUM (ANALYZE) shard_posts`); err != nil {
			return st, fmt.Errorf("vacuum source: %w", err)
		}
	}
	st.Total = time.Since(start)
	return st, nil
}

const selectChanges = `SELECT id, lshard, author_id, message, version, created_at, updated_at, deleted_at
	FROM shard_posts WHERE lshard = $1 AND writer_xid >= $2::text::xid8`

// copyRound copies rows of shard l written by transactions >= horizon, in
// one REPEATABLE READ snapshot, and returns that snapshot's xmin as the next
// horizon.
func copyRound(ctx context.Context, src, dst *pgxpool.Pool, l int, horizon string) (int, string, error) {
	var next string
	var b rowBatch
	err := pgx.BeginTxFunc(ctx, src, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		// First statement: takes the snapshot this round reads with.
		if err := tx.QueryRow(ctx, `SELECT pg_snapshot_xmin(pg_current_snapshot())::text`).Scan(&next); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, selectChanges, l, horizon)
		if err != nil {
			return err
		}
		b, err = collect(rows)
		return err
	})
	if err != nil {
		return 0, "", err
	}
	if err := upsert(ctx, dst, b); err != nil {
		return 0, "", err
	}
	return len(b.ids), next, nil
}

type rowBatch struct {
	ids               []pgtype.UUID
	lshards           []int16
	authors, versions []int64
	messages          []string
	created, updated  []pgtype.Timestamptz
	deleted           []pgtype.Timestamptz
}

func collect(rows pgx.Rows) (rowBatch, error) {
	var b rowBatch
	defer rows.Close()
	for rows.Next() {
		var id pgtype.UUID
		var l int16
		var a, v int64
		var msg string
		var c, u, d pgtype.Timestamptz
		if err := rows.Scan(&id, &l, &a, &msg, &v, &c, &u, &d); err != nil {
			return b, err
		}
		b.ids, b.lshards, b.authors, b.messages = append(b.ids, id), append(b.lshards, l), append(b.authors, a), append(b.messages, msg)
		b.versions, b.created, b.updated, b.deleted = append(b.versions, v), append(b.created, c), append(b.updated, u), append(b.deleted, d)
	}
	return b, rows.Err()
}

// upsert applies a batch to the target; a row only replaces an older version.
func upsert(ctx context.Context, dst *pgxpool.Pool, b rowBatch) error {
	const chunk = 2000
	for i := 0; i < len(b.ids); i += chunk {
		j := min(i+chunk, len(b.ids))
		_, err := dst.Exec(ctx, `
			INSERT INTO shard_posts (id, lshard, author_id, message, version, created_at, updated_at, deleted_at)
			SELECT * FROM unnest($1::uuid[], $2::smallint[], $3::bigint[], $4::text[], $5::bigint[],
			                     $6::timestamptz[], $7::timestamptz[], $8::timestamptz[])
			ON CONFLICT (id) DO UPDATE SET message = EXCLUDED.message, version = EXCLUDED.version,
				updated_at = EXCLUDED.updated_at, deleted_at = EXCLUDED.deleted_at,
				writer_xid = pg_current_xact_id()
			WHERE shard_posts.version < EXCLUDED.version`,
			b.ids[i:j], b.lshards[i:j], b.authors[i:j], b.messages[i:j], b.versions[i:j],
			b.created[i:j], b.updated[i:j], b.deleted[i:j])
		if err != nil {
			return err
		}
	}
	return nil
}

// Rebalance moves every logical shard whose node differs from place(l),
// one at a time, and reports each move.
func (m *Mover) Rebalance(ctx context.Context, place func(l int) string, onMove func(MoveStats)) error {
	current := m.R.Placement()
	if err := m.R.Refresh(ctx); err == nil {
		current = m.R.Placement()
	}
	var errs []error
	for l := 0; l < LogicalShards; l++ {
		if current[l] == place(l) {
			continue
		}
		st, err := m.Move(ctx, l, place(l))
		if err != nil {
			errs = append(errs, fmt.Errorf("shard %d: %w", l, err))
			continue
		}
		if onMove != nil {
			onMove(st)
		}
	}
	return errors.Join(errs...)
}

func orInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}
