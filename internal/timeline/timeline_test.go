package timeline_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"networkai/internal/store"
	"networkai/internal/testutil"
	"networkai/internal/timeline"

	"github.com/google/uuid"
)

type env struct {
	t   *testing.T
	st  *store.Store
	tl  *timeline.Service
	ctx context.Context
}

func setup(t *testing.T, threshold int64) *env {
	pool := testutil.NewDB(t)
	rdb, prefix := testutil.Redis(t)
	st := store.New(pool)
	return &env{t: t, st: st, ctx: context.Background(),
		tl: &timeline.Service{Redis: rdb, Store: st, CelebrityThreshold: threshold, Cap: 50, Prefix: prefix}}
}

func (e *env) user(name string) store.User {
	e.t.Helper()
	u, err := e.st.CreateUser(e.ctx, name, "h")
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *env) follow(follower store.User, followee string) {
	e.t.Helper()
	if _, err := e.st.Follow(e.ctx, follower.ID, followee); err != nil {
		e.t.Fatal(err)
	}
}

// post creates a post and runs fan-out, as the outbox worker would.
func (e *env) post(author store.User, msg string) uuid.UUID {
	e.t.Helper()
	p, err := e.st.CreatePost(e.ctx, store.NewPost{ID: uuid.New(), AuthorID: author.ID, Message: msg, MediaType: "none"})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.tl.HandlePostChanged(e.ctx, p.ID); err != nil {
		e.t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // distinct millisecond scores
	return p.ID
}

func (e *env) read(u store.User, before time.Time, limit int) timeline.Page {
	e.t.Helper()
	pg, err := e.tl.Read(e.ctx, u.ID, before, limit)
	if err != nil {
		e.t.Fatal(err)
	}
	return pg
}

func messages(pg timeline.Page) []string {
	var out []string
	for _, p := range pg.Posts {
		out = append(out, p.Message)
	}
	return out
}

func TestPushedPostsAppearNewestFirst(t *testing.T) {
	e := setup(t, 1000)
	alice, bob, carol := e.user("alice"), e.user("bob"), e.user("carol")
	e.follow(alice, "bob")
	e.follow(alice, "carol")

	// First read builds the timeline from PostgreSQL.
	if pg := e.read(alice, time.Time{}, 10); !pg.Rebuilt || len(pg.Posts) != 0 {
		t.Fatalf("first read: %+v", pg)
	}
	e.post(bob, "b1")
	e.post(carol, "c1")
	e.post(bob, "b2")
	e.post(alice, "own post") // not in your own home timeline

	pg := e.read(alice, time.Time{}, 10)
	if pg.Rebuilt || fmt.Sprint(messages(pg)) != "[b2 c1 b1]" {
		t.Fatalf("timeline = %v (rebuilt=%v), want [b2 c1 b1] from the cache", messages(pg), pg.Rebuilt)
	}
}

// Fan-out must not create timelines for users who are not reading: those are
// built from PostgreSQL on demand and include the post anyway.
func TestFanoutSkipsInactiveUsersButRebuildIncludesPost(t *testing.T) {
	e := setup(t, 1000)
	alice, bob := e.user("alice"), e.user("bob")
	e.follow(alice, "bob")
	e.post(bob, "while alice was away")
	if n, _ := e.tl.Redis.Exists(e.ctx, e.tl.Prefix+fmt.Sprintf("tl:%d", alice.ID)).Result(); n != 0 {
		t.Fatal("fan-out created a timeline for an inactive user")
	}
	pg := e.read(alice, time.Time{}, 10)
	if !pg.Rebuilt || fmt.Sprint(messages(pg)) != "[while alice was away]" {
		t.Fatalf("rebuild = %v", messages(pg))
	}
}

// The hybrid: a celebrity's post is never pushed, yet appears in followers'
// timelines, merged in time order with pushed posts.
func TestCelebrityPostsArePulledAndMerged(t *testing.T) {
	e := setup(t, 3) // 3+ followers = celebrity
	star := e.user("star")
	fans := []store.User{e.user("fan1"), e.user("fan2"), e.user("fan3")}
	friend := e.user("friend")
	for _, f := range fans {
		e.follow(f, "star")
	}
	e.follow(fans[0], "friend")
	e.read(fans[0], time.Time{}, 10) // build fans[0]'s timeline

	var fanoutWrites int
	e.tl.OnFanout = func(n int, _ time.Duration) { fanoutWrites += n }
	e.post(friend, "friend 1")
	e.post(star, "star 1")
	e.post(friend, "friend 2")
	e.post(star, "star 2")

	if fanoutWrites != 2 { // two friend posts x one follower; star posts push nothing
		t.Fatalf("fan-out writes = %d, want 2 (celebrity posts must not be pushed)", fanoutWrites)
	}
	pg := e.read(fans[0], time.Time{}, 10)
	if fmt.Sprint(messages(pg)) != "[star 2 friend 2 star 1 friend 1]" || pg.Pulled != 2 {
		t.Fatalf("merged = %v pulled=%d", messages(pg), pg.Pulled)
	}
	if got := messages(e.read(fans[1], time.Time{}, 10)); fmt.Sprint(got) != "[star 2 star 1]" {
		t.Fatalf("pure pull follower = %v", got)
	}
}

// Paging through a merged timeline must not skip or repeat posts.
func TestPaginationAcrossPushAndPull(t *testing.T) {
	e := setup(t, 2)
	reader, star, friend := e.user("reader"), e.user("star"), e.user("friend")
	other := e.user("other")
	e.follow(reader, "star")
	e.follow(other, "star") // star now has 2 followers: celebrity
	e.follow(reader, "friend")
	e.read(reader, time.Time{}, 1)
	var want []string
	for i := 0; i < 9; i++ {
		a := friend
		if i%3 == 0 {
			a = star
		}
		m := fmt.Sprintf("p%d", i)
		e.post(a, m)
		want = append([]string{m}, want...)
	}
	var got []string
	before := time.Time{}
	for pages := 0; pages < 10; pages++ {
		pg := e.read(reader, before, 4)
		got = append(got, messages(pg)...)
		if pg.NextCursor == nil {
			break
		}
		before = *pg.NextCursor
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("paged = %v\nwant    %v", got, want)
	}
}

func TestDeleteRemovesAndReplayIsHarmless(t *testing.T) {
	e := setup(t, 1000)
	alice, bob := e.user("alice"), e.user("bob")
	e.follow(alice, "bob")
	e.read(alice, time.Time{}, 10)
	id := e.post(bob, "oops")
	keep := e.post(bob, "keep")
	// Replays (at-least-once delivery) change nothing.
	for i := 0; i < 3; i++ {
		e.tl.HandlePostChanged(e.ctx, keep) //nolint:errcheck
	}
	if _, err := e.st.DeletePost(e.ctx, id, bob.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.tl.HandlePostChanged(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := messages(e.read(alice, time.Time{}, 10)); fmt.Sprint(got) != "[keep]" {
		t.Fatalf("after delete = %v", got)
	}
	n, _ := e.tl.Redis.ZCard(e.ctx, e.tl.Prefix+fmt.Sprintf("tl:%d", alice.ID)).Result()
	if n != 2 { // marker + "keep": no duplicates from replays
		t.Fatalf("timeline size = %d, want 2", n)
	}
}

func TestTimelineIsCapped(t *testing.T) {
	e := setup(t, 1000)
	e.tl.Cap = 5
	alice, bob := e.user("alice"), e.user("bob")
	e.follow(alice, "bob")
	e.read(alice, time.Time{}, 10)
	for i := 0; i < 12; i++ {
		e.post(bob, fmt.Sprintf("p%d", i))
	}
	n, _ := e.tl.Redis.ZCard(e.ctx, e.tl.Prefix+fmt.Sprintf("tl:%d", alice.ID)).Result()
	if n != 5 {
		t.Fatalf("timeline size = %d, want cap 5", n)
	}
	if got := messages(e.read(alice, time.Time{}, 3)); fmt.Sprint(got) != "[p11 p10 p9]" {
		t.Fatalf("newest = %v", got)
	}
}

func TestFollowCountersAndSelfFollow(t *testing.T) {
	e := setup(t, 1000)
	alice := e.user("alice")
	e.user("bob")
	e.follow(alice, "bob")
	e.follow(alice, "bob") // idempotent
	p, err := e.st.GetProfile(e.ctx, "bob", alice.ID)
	if err != nil || p.FollowerCount != 1 || !p.FollowedByMe {
		t.Fatalf("profile = %+v %v", p, err)
	}
	if _, err := e.st.Follow(e.ctx, alice.ID, "alice"); err != store.ErrSelfFollow {
		t.Fatalf("self follow: %v", err)
	}
	e.st.Unfollow(e.ctx, alice.ID, "bob") //nolint:errcheck
	e.st.Unfollow(e.ctx, alice.ID, "bob") //nolint:errcheck
	if p, _ := e.st.GetProfile(e.ctx, "bob", alice.ID); p.FollowerCount != 0 {
		t.Fatalf("after unfollow: %+v", p)
	}
}
