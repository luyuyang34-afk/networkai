// Command networkai runs the NetworkAI backend.
//
//	networkai api       HTTP API (applies migrations on start)
//	networkai indexer   outbox worker: PostgreSQL -> Elasticsearch
//	networkai migrate   apply database migrations and exit
//	networkai reindex   rebuild the search index from PostgreSQL and exit
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"networkai/internal/auth"
	"networkai/internal/config"
	"networkai/internal/db"
	"networkai/internal/httpapi"
	"networkai/internal/indexer"
	"networkai/internal/media"
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
		if err := es.EnsureIndex(ctx); err != nil {
			return err
		}
		ix := &indexer.Indexer{Posts: st, Index: es, Log: log}
		n, err := ix.Reindex(ctx)
		log.Info("reindex finished", "posts", n)
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
	var ms media.Store
	var localDir string
	switch cfg.MediaBackend {
	case "gcs":
		g, err := media.NewGCS(ctx, cfg.GCSBucket)
		if err != nil {
			return err
		}
		ms = g
	default:
		l, err := media.NewLocal(cfg.MediaDir, cfg.MediaBaseURL)
		if err != nil {
			return err
		}
		ms, localDir = l, cfg.MediaDir
	}

	handler := httpapi.New(httpapi.Deps{
		Users: st, Posts: st, Search: es, Media: ms,
		Ready:          readiness{Store: st, pool: pool},
		Tokens:         auth.NewTokens(cfg.JWTSecret, cfg.JWTTTL),
		Log:            log,
		CORSOrigins:    cfg.CORSOrigins,
		MaxUploadBytes: cfg.MaxUploadBytes,
		LocalMediaDir:  localDir,
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
	ix := &indexer.Indexer{Posts: st, Index: es, Log: log}
	w := &outbox.Worker{
		Pool:         pool,
		Handle:       ix.Handle,
		BatchSize:    cfg.IndexerBatchSize,
		PollInterval: cfg.IndexerPoll,
		MaxAttempts:  cfg.IndexerMaxAttempts,
		Log:          log,
	}
	return w.Run(ctx)
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
