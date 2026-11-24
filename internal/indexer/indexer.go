// Package indexer turns outbox events into Elasticsearch writes.
//
// Events carry only the post id. The handler re-reads the post's *current*
// state from PostgreSQL and writes it with the post's version as the
// Elasticsearch external version. Two properties fall out of that design:
//
//   - Idempotent: replaying an event writes the same version again, which
//     Elasticsearch rejects as a conflict (treated as success).
//   - Order-independent: whichever event is processed last, the index ends up
//     holding the highest version, i.e. the latest state in PostgreSQL.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"networkai/internal/outbox"
	"networkai/internal/search"
	"networkai/internal/store"

	"github.com/google/uuid"
)

type postSource interface {
	GetPostForIndex(ctx context.Context, id uuid.UUID) (store.Post, error)
	PostsForIndexAfter(ctx context.Context, after uuid.UUID, limit int) ([]store.Post, error)
}

type docSink interface {
	Upsert(ctx context.Context, doc search.Doc, version int64) (bool, error)
	BulkUpsert(ctx context.Context, docs []search.VersionedDoc) (search.BulkResult, error)
}

type Indexer struct {
	Posts postSource
	Index docSink
	Log   *slog.Logger
}

// Handle is an outbox.Handler.
func (ix *Indexer) Handle(ctx context.Context, ev outbox.Event) error {
	if ev.AggregateType != store.AggregatePost {
		return fmt.Errorf("%w: unknown aggregate type %q", outbox.ErrPermanent, ev.AggregateType)
	}
	id, err := uuid.Parse(ev.AggregateID)
	if err != nil {
		return fmt.Errorf("%w: bad post id %q", outbox.ErrPermanent, ev.AggregateID)
	}
	return ix.IndexPost(ctx, id)
}

func (ix *Indexer) IndexPost(ctx context.Context, id uuid.UUID) error {
	p, err := ix.Posts.GetPostForIndex(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		// Posts are only soft-deleted, so this should not happen. Nothing
		// to index either way.
		return nil
	}
	if err != nil {
		return fmt.Errorf("load post %s: %w", id, err)
	}
	applied, err := ix.Index.Upsert(ctx, ToDoc(p), p.Version)
	if err != nil {
		return err
	}
	if ix.Log != nil {
		ix.Log.Debug("indexed post", "id", id, "version", p.Version, "applied", applied, "deleted", p.DeletedAt != nil)
	}
	return nil
}

// ReindexBatchSize is the number of posts per PostgreSQL page and per
// Elasticsearch _bulk request.
const ReindexBatchSize = 500

// Reindex rebuilds the search index from PostgreSQL. It is safe to run while
// the worker is running: external versions make concurrent writes converge.
//
// It reads posts in id-ordered pages and writes each page with one _bulk
// request: two round trips per 500 posts instead of two per post.
func (ix *Indexer) Reindex(ctx context.Context) (int, error) {
	n := 0
	var after uuid.UUID
	for {
		posts, err := ix.Posts.PostsForIndexAfter(ctx, after, ReindexBatchSize)
		if err != nil {
			return n, err
		}
		if len(posts) == 0 {
			return n, nil
		}
		batch := make([]search.VersionedDoc, len(posts))
		for i, p := range posts {
			batch[i] = search.VersionedDoc{Doc: ToDoc(p), Version: p.Version}
		}
		if _, err := ix.Index.BulkUpsert(ctx, batch); err != nil {
			return n, err
		}
		n += len(posts)
		after = posts[len(posts)-1].ID
	}
}

func ToDoc(p store.Post) search.Doc {
	return search.Doc{
		ID:        p.ID.String(),
		Author:    p.Author,
		Message:   p.Message,
		MediaType: p.MediaType,
		MediaURL:  p.MediaURL,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
		Deleted:   p.DeletedAt != nil,
	}
}
