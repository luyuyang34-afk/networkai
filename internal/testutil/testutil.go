// Package testutil provides throwaway databases for integration tests.
//
// Integration tests run only when TEST_DATABASE_URL (and, for search tests,
// TEST_ES_URL) are set; otherwise they are skipped so `go test ./...` works on
// any machine. `make test-integration` sets them against docker compose.
package testutil

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"testing"

	"networkai/internal/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewDB creates a fresh, migrated database and drops it when the test ends,
// so tests never see each other's rows and can run in parallel.
func NewDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin db: %v", err)
	}
	defer admin.Close(ctx)

	name := fmt.Sprintf("networkai_test_%d", rand.Uint64())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test db: %v", err)
	}

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck
	})

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// ESURL returns the Elasticsearch URL for search integration tests or skips.
func ESURL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("TEST_ES_URL")
	if u == "" {
		t.Skip("TEST_ES_URL not set; skipping Elasticsearch integration test")
	}
	return u
}
