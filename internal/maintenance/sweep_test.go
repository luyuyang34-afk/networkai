package maintenance_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"networkai/internal/maintenance"
	"networkai/internal/media"
	"networkai/internal/store"
	"networkai/internal/testutil"

	"github.com/google/uuid"
)

func TestSweepOrphanedMedia(t *testing.T) {
	pool := testutil.NewDB(t)
	st := store.New(pool)
	ctx := context.Background()
	dir := t.TempDir()
	ms, _ := media.NewLocal(dir, "http://m")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	u, _ := st.CreateUser(ctx, "alice", "h")
	put := func(key string, age time.Duration) {
		ms.Put(ctx, key, strings.NewReader("x"), "image/png") //nolint:errcheck
		old := time.Now().Add(-age)
		os.Chtimes(filepath.Join(dir, key), old, old) //nolint:errcheck
	}

	live := uuid.New()
	// Referenced by a live post: keep.
	st.CreatePost(ctx, store.NewPost{ID: live, AuthorID: u.ID, Message: "m", MediaType: "image", MediaKey: "live.png"}) //nolint:errcheck
	put("live.png", 48*time.Hour)

	// Post deleted but its file was left behind: delete.
	gone := uuid.New()
	st.CreatePost(ctx, store.NewPost{ID: gone, AuthorID: u.ID, Message: "m", MediaType: "image", MediaKey: "deleted.png"}) //nolint:errcheck
	st.DeletePost(ctx, gone, u.ID)                                                                                         //nolint:errcheck
	put("deleted.png", 48*time.Hour)

	put("orphan.png", 48*time.Hour)     // crash between upload and commit: delete
	put("inflight.png", 10*time.Second) // too young; its transaction may still commit: keep

	dry, err := maintenance.SweepOrphanedMedia(ctx, ms, st, time.Hour, true, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Orphans != 2 || dry.Deleted != 0 {
		t.Fatalf("dry run = %+v; want 2 orphans, 0 deleted", dry)
	}

	res, err := maintenance.SweepOrphanedMedia(ctx, ms, st, time.Hour, false, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 4 || res.Deleted != 2 {
		t.Fatalf("sweep = %+v; want scanned 4, deleted 2", res)
	}
	for key, want := range map[string]bool{"live.png": true, "inflight.png": true, "deleted.png": false, "orphan.png": false} {
		_, err := os.Stat(filepath.Join(dir, key))
		if exists := err == nil; exists != want {
			t.Errorf("%s exists=%v, want %v", key, exists, want)
		}
	}
}
