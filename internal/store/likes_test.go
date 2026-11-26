package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"networkai/internal/store"
	"networkai/internal/testutil"

	"github.com/google/uuid"
)

// Sharded counters must stay exact under concurrency: the sum of the shard
// rows equals the number of likes rows after concurrent likes, duplicate
// likes and unlikes.
func TestShardedLikeCountersStayExact(t *testing.T) {
	for _, shards := range []int{1, 16} {
		t.Run(fmt.Sprintf("shards=%d", shards), func(t *testing.T) {
			pool := testutil.NewDB(t)
			st := store.New(pool).WithLikeShards(shards)
			ctx := context.Background()
			author, _ := st.CreateUser(ctx, "author", "h")
			p, _ := st.CreatePost(ctx, store.NewPost{ID: uuid.New(), AuthorID: author.ID, Message: "viral", MediaType: "none"})
			var users []store.User
			for i := 0; i < 100; i++ {
				u, err := st.CreateUser(ctx, fmt.Sprintf("fan_%03d", i), "h")
				if err != nil {
					t.Fatal(err)
				}
				users = append(users, u)
			}
			var wg sync.WaitGroup
			for i, u := range users {
				wg.Add(1)
				go func() {
					defer wg.Done()
					st.Like(ctx, u.ID, p.ID) //nolint:errcheck
					st.Like(ctx, u.ID, p.ID) //nolint:errcheck // duplicate: no-op
					if i%4 == 0 {
						st.Unlike(ctx, u.ID, p.ID) //nolint:errcheck
						st.Unlike(ctx, u.ID, p.ID) //nolint:errcheck // already gone: no-op
					}
				}()
			}
			wg.Wait()
			counts, err := st.LikeCounts(ctx, []string{p.ID.String()})
			if err != nil {
				t.Fatal(err)
			}
			var rows int
			pool.QueryRow(ctx, `SELECT count(*) FROM likes WHERE post_id = $1`, p.ID).Scan(&rows) //nolint:errcheck
			if counts[p.ID.String()] != 75 || rows != 75 {
				t.Fatalf("counter=%d likes rows=%d, want 75 and 75", counts[p.ID.String()], rows)
			}
			var used int
			pool.QueryRow(ctx, `SELECT count(*) FROM post_like_counts WHERE post_id = $1`, p.ID).Scan(&used) //nolint:errcheck
			if used > shards {
				t.Fatalf("used %d shard rows, max %d", used, shards)
			}
		})
	}
}
