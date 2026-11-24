// Command networkai runs the NetworkAI backend.
//
//	networkai api       HTTP API (applies migrations on start)
//	networkai indexer   outbox worker: PostgreSQL -> Elasticsearch
//	networkai migrate   apply database migrations and exit
//	networkai reindex [-fresh]  re-apply all posts to the index (in place,
//	                            or blue/green into a new index + alias swap)
//	networkai sweep-media [-dry-run]  delete media files no live post references
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"networkai/internal/auth"
	"networkai/internal/config"
	"networkai/internal/db"
	"networkai/internal/embed"
	"networkai/internal/feed"
	"networkai/internal/httpapi"
	"networkai/internal/indexer"
	"networkai/internal/maintenance"
	"networkai/internal/media"
	"networkai/internal/metrics"
	"networkai/internal/outbox"
	"networkai/internal/search"
	"networkai/internal/store"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: networkai <api|indexer|migrate|reindex>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1], log); err != nil {
		log.Error("fatal", "cmd", os.Args[1], "err", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return l
}

func run(ctx context.Context, cmd string, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pool, err := connectWithRetry(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.New(pool)
	es := search.New(cfg.ESURL, cfg.ESUsername, cfg.ESPassword)

	switch cmd {
	case "migrate":
		return db.Migrate(ctx, pool)
	case "api":
		if err := db.Migrate(ctx, pool); err != nil {
			return err
		}
		return runAPI(ctx, cfg, st, pool, es, log)
	case "indexer":
		return runIndexer(ctx, cfg, st, pool, es, log)
	case "reindex":
		fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
		fresh := fs.Bool("fresh", false, "blue/green: build a new index, swap the alias, delete the old one")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		if err := es.EnsureIndex(ctx); err != nil {
			return err
		}
		ix := &indexer.Indexer{Posts: st, Index: es, Log: log, Embed: loadEmbedder(cfg, log)}
		if *fresh {
			name := fmt.Sprintf("%s_%s", search.DefaultAlias, time.Now().UTC().Format("20060102t150405"))
			loaded, caught, err := ix.RebuildFresh(ctx, es, name)
			log.Info("fresh rebuild finished", "index", name, "posts", loaded, "caught_up", caught)
			return err
		}
		n, err := ix.Reindex(ctx)
		log.Info("reindex finished", "posts", n)
		return err
	case "sweep-media":
		fs := flag.NewFlagSet("sweep-media", flag.ContinueOnError)
		dry := fs.Bool("dry-run", false, "only report orphans")
		grace := fs.Duration("grace", time.Hour, "skip files younger than this")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		ms, _, err := openMedia(ctx, cfg)
		if err != nil {
			return err
		}
		lister, ok := ms.(interface {
			media.Lister
			Delete(context.Context, string) error
		})
		if !ok {
			return fmt.Errorf("media backend %q cannot list objects", cfg.MediaBackend)
		}
		res, err := maintenance.SweepOrphanedMedia(ctx, lister, st, *grace, *dry, log)
		log.Info("media sweep finished", "scanned", res.Scanned, "orphans", res.Orphans, "deleted", res.Deleted, "dry_run", *dry)
		return err
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

type readiness struct {
	*store.Store
	pool *pgxpool.Pool
}

func (r readiness) OutboxStats(ctx context.Context) (outbox.Stats, error) {
	return outbox.ReadStats(ctx, r.pool)
}

func runAPI(ctx context.Context, cfg config.Config, st *store.Store, pool *pgxpool.Pool, es *search.Client, log *slog.Logger) error {
	ms, localDir, err := openMedia(ctx, cfg)
	if err != nil {
		return err
	}

	handler := httpapi.New(httpapi.Deps{
		Users: st, Posts: st, Search: es, Media: ms,
		Ready:          readiness{Store: st, pool: pool},
		Tokens:         auth.NewTokens(cfg.JWTSecret, cfg.JWTTTL),
		Log:            log,
		CORSOrigins:    cfg.CORSOrigins,
		MaxUploadBytes: cfg.MaxUploadBytes,
		LocalMediaDir:  localDir,
		Metrics:        apiMetrics(pool),
		Likes:          st,
		Feed:           loadFeed(cfg, st, es, log),
	})
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       2 * time.Minute, // large uploads
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", cfg.HTTPAddr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Graceful shutdown: stop accepting, let in-flight requests finish.
	log.Info("shutting down api")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runIndexer(ctx context.Context, cfg config.Config, st *store.Store, pool *pgxpool.Pool, es *search.Client, log *slog.Logger) error {
	// Elasticsearch may still be starting (or down). Keep trying to create
	// the index; meanwhile the outbox simply accumulates.
	for {
		err := es.EnsureIndex(ctx)
		if err == nil {
			break
		}
		log.Warn("elasticsearch not ready, retrying", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
	m := metrics.New()
	m.RegisterOutboxBacklog(pool)
	go serveMetrics(ctx, cfg.MetricsAddr, m, log)
	go purgeTombstonesPeriodically(ctx, es, log)

	ix := &indexer.Indexer{Posts: st, Index: es, Log: log, Embed: loadEmbedder(cfg, log)}
	w := &outbox.Worker{
		OnEvent:      m.ObserveOutboxEvent,
		OnPause:      m.SetOutboxPaused,
		Pool:         pool,
		Handle:       ix.Handle,
		BatchSize:    cfg.IndexerBatchSize,
		PollInterval: cfg.IndexerPoll,
		MaxAttempts:  cfg.IndexerMaxAttempts,
		Log:          log,
		IsUnavailable: func(err error) bool {
			return errors.Is(err, search.ErrUnavailable)
		},
	}
	return w.Run(ctx)
}

func loadEmbedder(cfg config.Config, log *slog.Logger) func(string) []float32 {
	m, err := embed.Load(filepath.Join(cfg.ModelDir, "lsa.json"))
	if err != nil {
		log.Warn("no embedding model; indexing without embeddings", "err", err)
		return nil
	}
	return m.Embed
}

func loadFeed(cfg config.Config, st *store.Store, es *search.Client, log *slog.Logger) httpapi.Feed {
	r, err := feed.LoadRanker(filepath.Join(cfg.ModelDir, "ranker.json"))
	if err != nil {
		log.Warn("no ranker model; GET /feed disabled", "err", err)
		return nil
	}
	return &feed.Service{Signals: st, Index: es, Ranker: r}
}

func openMedia(ctx context.Context, cfg config.Config) (media.Store, string, error) {
	if cfg.MediaBackend == "gcs" {
		g, err := media.NewGCS(ctx, cfg.GCSBucket)
		return g, "", err
	}
	l, err := media.NewLocal(cfg.MediaDir, cfg.MediaBaseURL)
	return l, cfg.MediaDir, err
}

func apiMetrics(pool *pgxpool.Pool) *metrics.Registry {
	m := metrics.New()
	m.RegisterOutboxBacklog(pool)
	return m
}

// purgeTombstonesPeriodically removes week-old deleted-post tombstones. Running
// it in every indexer replica is harmless: delete_by_query is idempotent.
func purgeTombstonesPeriodically(ctx context.Context, es *search.Client, log *slog.Logger) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := es.PurgeTombstones(ctx, 7*24*time.Hour)
			if err != nil {
				log.Warn("tombstone purge failed", "err", err)
				continue
			}
			log.Info("tombstones purged", "count", n)
		}
	}
}

func serveMetrics(ctx context.Context, addr string, m *metrics.Registry, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Info("indexer metrics listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("metrics server failed", "err", err)
	}
}

func connectWithRetry(ctx context.Context, url string, log *slog.Logger) (*pgxpool.Pool, error) {
	var lastErr error
	for i := 0; i < 30; i++ {
		pool, err := db.Connect(ctx, url)
		if err == nil {
			return pool, nil
		}
		lastErr = err
		log.Warn("postgres not ready, retrying", "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, lastErr
}
