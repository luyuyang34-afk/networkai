// Package maintenance holds periodic cleanup jobs.
package maintenance

import (
	"context"
	"log/slog"
	"time"

	"networkai/internal/media"
)

type keyChecker interface {
	MediaKeysInUse(ctx context.Context, keys []string) (map[string]bool, error)
}

type deleter interface {
	Delete(ctx context.Context, key string) error
}

// SweepResult summarises one sweep.
type SweepResult struct {
	Scanned int `json:"scanned"`
	Orphans int `json:"orphans"`
	Deleted int `json:"deleted"`
}

// SweepOrphanedMedia deletes stored files that no live post references.
//
// Orphans happen when the process dies between uploading a file and
// committing the post row, or when the best-effort delete after a post
// deletion fails. Files younger than grace are skipped: they may belong to an
// upload whose database transaction has not committed yet. Deleting those
// would break a post that is about to exist.
func SweepOrphanedMedia(ctx context.Context, store interface {
	media.Lister
	deleter
}, posts keyChecker, grace time.Duration, dryRun bool, log *slog.Logger) (SweepResult, error) {
	var res SweepResult
	cutoff := time.Now().Add(-grace)
	var batch []string

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		used, err := posts.MediaKeysInUse(ctx, batch)
		if err != nil {
			return err
		}
		for _, k := range batch {
			if used[k] {
				continue
			}
			res.Orphans++
			if dryRun {
				log.Info("orphaned media (dry run)", "key", k)
				continue
			}
			if err := store.Delete(ctx, k); err != nil {
				log.Warn("delete orphaned media failed", "key", k, "err", err)
				continue
			}
			res.Deleted++
		}
		batch = batch[:0]
		return nil
	}

	err := store.List(ctx, func(o media.Object) error {
		res.Scanned++
		if o.ModTime.After(cutoff) {
			return nil
		}
		batch = append(batch, o.Key)
		if len(batch) >= 500 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	return res, flush()
}
