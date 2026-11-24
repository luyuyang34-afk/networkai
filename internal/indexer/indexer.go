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
	"time"

	"networkai/internal/outbox"
	"networkai/internal/search"
	"networkai/internal/store"

	"github.com/google/uuid"
)

type postSource interface {
	GetPostForIndex(ctx context.Context, id uuid.UUID) (store.Post, error)
	PostsForIndexAfter(ctx context.Context, after uuid.UUID, limit int) ([]store.Post, error)
	PostIDsUpdatedSince(ctx context.Context, since time.Time) ([]uuid.UUID, error)
	Now(ctx context.Context) (time.Time, error)
}

type docSink = search.Sink

type Indexer struct {
	Posts postSource
	Index docSink
	Log   *slog.Logger
	// Embed, if set, adds a semantic vector to each document.
	Embed func(text string) []float32
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
	applied, err := ix.Index.Upsert(ctx, ix.doc(p), p.Version)
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

// Reindex re-applies every post to the current index in place. It is safe to
// run while the worker is running: external versions make concurrent writes
// converge.
//
// Because equal versions are rejected, an in-place reindex only repairs
// missing or stale documents. It cannot change documents whose version is
// unchanged (for example, to add a new field such as embeddings). Use
// RebuildFresh for that.
//
// It reads posts in id-ordered pages and writes each page with one _bulk
// request: two round trips per 500 posts instead of two per post.
func (ix *Indexer) Reindex(ctx context.Context) (int, error) {
	return ix.reindexInto(ctx, ix.Index)
}

func (ix *Indexer) reindexInto(ctx context.Context, sink docSink) (int, error) {
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
			batch[i] = search.VersionedDoc{Doc: ix.doc(p), Version: p.Version}
		}
		if _, err := sink.BulkUpsert(ctx, batch); err != nil {
			return n, err
		}
		n += len(posts)
		after = posts[len(posts)-1].ID
	}
}

// Rebuilder is what a blue/green rebuild needs from Elasticsearch.
type Rebuilder interface {
	docSink
	CreateIndex(ctx context.Context, name string) error
	SwapAlias(ctx context.Context, newIndex string) ([]string, error)
	ForceMerge(ctx context.Context, index string) error
	DeleteIndex(ctx context.Context, name string) error
	// Into returns a sink that writes to a specific physical index.
	Into(index string) search.Sink
}

// RebuildFresh does a zero-downtime blue/green rebuild:
//
//  1. create a new physical index (current mapping)
//  2. bulk-load every post into it while searches keep using the old one
//  3. force-merge it to one segment (cheaper kNN), still offline
//  4. atomically swap the alias to the new index
//  5. catch up: re-apply posts changed since step 2 began. The live worker
//     kept writing to the old index during the load; external versions
//     make this replay safe.
//  6. delete the old index
func (ix *Indexer) RebuildFresh(ctx context.Context, es Rebuilder, newIndex string) (loaded, caughtUp int, err error) {
	start, err := ix.Posts.Now(ctx)
	if err != nil {
		return 0, 0, err
	}
	if err := es.CreateIndex(ctx, newIndex); err != nil {
		return 0, 0, fmt.Errorf("create %s: %w", newIndex, err)
	}
	loaded, err = ix.reindexInto(ctx, es.Into(newIndex))
	if err != nil {
		return loaded, 0, err
	}
	if err := es.ForceMerge(ctx, newIndex); err != nil {
		return loaded, 0, fmt.Errorf("force merge: %w", err)
	}
	old, err := es.SwapAlias(ctx, newIndex)
	if err != nil {
		return loaded, 0, err
	}
	// Small margin for clock skew between the transaction that committed
	// and the moment we read the clock.
	changed, err := ix.Posts.PostIDsUpdatedSince(ctx, start.Add(-5*time.Second))
	if err != nil {
		return loaded, 0, err
	}
	for _, id := range changed {
		if err := ix.IndexPost(ctx, id); err != nil {
			return loaded, caughtUp, err
		}
		caughtUp++
	}
	for _, idx := range old {
		if err := es.DeleteIndex(ctx, idx); err != nil && ix.Log != nil {
			ix.Log.Warn("could not delete old index", "index", idx, "err", err)
		}
	}
	return loaded, caughtUp, nil
}

func (ix *Indexer) doc(p store.Post) search.Doc {
	d := ToDoc(p)
	if ix.Embed != nil && p.DeletedAt == nil {
		d.Embedding = ix.Embed(p.Message)
	}
	return d
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
