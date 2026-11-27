// Command failover runs a region-failover drill and measures data loss
// (RPO), recovery time (RTO) and split brain.
//
//	go run ./tools/experiments/failover -mode async
//	go run ./tools/experiments/failover -mode fenced
//	go run ./tools/experiments/failover -mode sync
//
// It (re)creates the throwaway cluster of docker compose profile
// "multiregion":
//
//   - pg-us is the primary in region US;
//   - pg-eu is a replica in region EU, streaming over a 70 ms RTT link
//     (wan-repl);
//   - EU clients reach the primary through vip-eu, a switchable endpoint;
//   - US clients connect to pg-us directly.
//
// The failure is a network partition between the regions, not a crash:
//   - US clients can still reach pg-us and keep writing, which is the hard
//     case;
//   - after N failed probes, the EU side promotes pg-eu and points vip-eu
//     at it.
//
// Modes:
//
//   - async: asynchronous replication, promote on detection;
//   - fenced: async plus a leader lease, as Patroni does with etcd. A US
//     agent renews a lease through a witness link. When it cannot, after
//     the TTL it makes pg-us read-only and drops sessions. EU promotes only
//     after the TTL has passed since it lost contact;
//   - sync: synchronous replication to pg_eu (synchronous_commit = on).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	usDSN    = "postgres://networkai:networkai@localhost:5451/drill?sslmode=disable"
	euDSN    = "postgres://networkai:networkai@localhost:5452/drill?sslmode=disable"
	vipDSN   = "postgres://networkai:networkai@localhost:5453/drill?sslmode=disable"
	witness  = "localhost:5454"
	adminVIP = "http://localhost:19112"
)

var links = []string{"http://localhost:19111", adminVIP, "http://localhost:19113"} // repl, vip, witness

type ack struct {
	writer string
	seq    int64
	at     time.Duration
	lat    time.Duration
}

func main() {
	mode := flag.String("mode", "async", "async | fenced | sync")
	warmup := flag.Duration("warmup", 5*time.Second, "writes before the partition")
	after := flag.Duration("after", 6*time.Second, "writes after promotion")
	probes := flag.Int("probes", 3, "failed probes (every 500 ms) before failover")
	ttl := flag.Duration("lease-ttl", 3*time.Second, "leader lease TTL (fenced mode)")
	perRegion := flag.Int("writers", 4, "writers per region")
	flag.Parse()
	ctx := context.Background()

	resetCluster(*mode)
	us := connect(ctx, usDSN)
	must2(us.Exec(ctx, `CREATE TABLE IF NOT EXISTS drill_writes (writer text, seq bigint, PRIMARY KEY (writer, seq))`))
	waitReplicaHasTable(ctx)

	t0 := time.Now()
	var mu sync.Mutex
	var acks []ack
	var failures atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	writer := func(name, dsn string) {
		defer wg.Done()
		pool := connect(ctx, dsn)
		defer pool.Close()
		for seq := int64(1); ; seq++ {
			select {
			case <-stop:
				return
			default:
			}
			wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			start := time.Now()
			_, err := pool.Exec(wctx, `INSERT INTO drill_writes VALUES ($1, $2)`, name, seq)
			cancel()
			if err != nil {
				failures.Add(1)
				time.Sleep(50 * time.Millisecond)
				continue
			}
			mu.Lock()
			acks = append(acks, ack{name, seq, time.Since(t0), time.Since(start)})
			mu.Unlock()
		}
	}
	for i := 0; i < *perRegion; i++ {
		wg.Add(2)
		go writer(fmt.Sprintf("us-%d", i), usDSN)
		go writer(fmt.Sprintf("eu-%d", i), vipDSN)
	}

	// Fenced mode: the US primary's leader lease, renewed through the
	// witness link every 500 ms.
	var fencedAt atomic.Int64
	if *mode == "fenced" {
		go func() {
			last := time.Now()
			for {
				select {
				case <-stop:
					return
				case <-time.After(500 * time.Millisecond):
				}
				if c, err := net.DialTimeout("tcp", witness, 400*time.Millisecond); err == nil {
					// A TCP accept proves nothing through a proxy: require an
					// answer from the far side.
					c.SetReadDeadline(time.Now().Add(400 * time.Millisecond)) //nolint:errcheck
					c.Write(pgSSLRequest)                                     //nolint:errcheck
					b := make([]byte, 1)
					if _, err := c.Read(b); err == nil {
						last = time.Now()
					}
					c.Close()
				}
				if time.Since(last) > *ttl {
					fence(ctx, us)
					fencedAt.Store(int64(time.Since(t0)))
					log.Printf("US agent: lease expired, pg-us fenced (read-only)")
					return
				}
			}
		}()
	}

	time.Sleep(*warmup)
	tPart := time.Since(t0)
	for _, l := range links {
		post(l + "/partition?on=true")
	}
	log.Printf("partition at %.1fs", tPart.Seconds())

	// EU-side detector.
	fails := 0
	var lostContact time.Duration
	for fails < *probes {
		time.Sleep(500 * time.Millisecond)
		pctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
		c, err := pgx.Connect(pctx, vipDSN)
		if err == nil {
			err = c.Ping(pctx)
			c.Close(ctx)
		}
		cancel()
		if err != nil {
			if fails == 0 {
				lostContact = time.Since(t0)
			}
			fails++
		} else {
			fails = 0
		}
	}
	if *mode == "fenced" {
		// Wait until the old primary must have fenced itself: its last
		// successful renewal was at most when we lost contact.
		wait := lostContact + *ttl + time.Second - time.Since(t0)
		if wait > 0 {
			log.Printf("waiting %.1fs for the old primary's lease to expire", wait.Seconds())
			time.Sleep(wait)
		}
	}
	eu := connect(ctx, euDSN)
	must2(eu.Exec(ctx, `SELECT pg_promote(true, 30)`))
	post(adminVIP + "/target?addr=pg-eu:5432")
	post(adminVIP + "/partition?on=false")
	tPromote := time.Since(t0)
	log.Printf("promoted pg-eu at %.1fs", tPromote.Seconds())

	time.Sleep(*after)
	close(stop)
	wg.Wait()

	// What survived on the new primary?
	rows, err := eu.Query(ctx, `SELECT writer, seq FROM drill_writes`)
	must(err)
	have := map[string]bool{}
	for rows.Next() {
		var w string
		var s int64
		must(rows.Scan(&w, &s))
		have[fmt.Sprint(w, "/", s)] = true
	}
	must(rows.Err())

	type bucket struct{ acked, lost int }
	var usBefore, usDuring, usSplit, euBefore, euAfter bucket
	var usLat, euLat []time.Duration
	firstEUAfter := time.Duration(-1)
	for _, a := range acks {
		lost := !have[fmt.Sprint(a.writer, "/", a.seq)]
		var b *bucket
		us := strings.HasPrefix(a.writer, "us")
		switch {
		case us && a.at < tPart:
			b = &usBefore
			usLat = append(usLat, a.lat)
		case us && a.at < tPromote:
			b = &usDuring
		case us:
			b = &usSplit
		case a.at < tPart:
			b = &euBefore
			euLat = append(euLat, a.lat)
		default:
			b = &euAfter
			if firstEUAfter < 0 || a.at < firstEUAfter {
				firstEUAfter = a.at
			}
		}
		b.acked++
		if lost {
			b.lost++
		}
	}
	fmt.Printf("# Region failover drill: %s\n\n", *mode)
	fmt.Printf("Partition at %.1fs, promotion at %.1fs (detection %d x 500 ms", tPart.Seconds(), tPromote.Seconds(), *probes)
	if *mode == "fenced" {
		fmt.Printf(", plus lease TTL %s", *ttl)
	}
	fmt.Printf(").\n\n")
	fmt.Printf("Write latency before the partition: US client P50 %s, EU client P50 %s\n\n", p50(usLat), p50(euLat))
	fmt.Println("| Writes acknowledged | Count | Missing on the new primary |")
	fmt.Println("|---|---|---|")
	fmt.Printf("| US clients, before partition | %d | %d |\n", usBefore.acked, usBefore.lost)
	fmt.Printf("| EU clients, before partition | %d | %d |\n", euBefore.acked, euBefore.lost)
	fmt.Printf("| US clients, during partition (old primary) | %d | **%d** |\n", usDuring.acked, usDuring.lost)
	fmt.Printf("| US clients, AFTER promotion (split brain) | %d | **%d** |\n", usSplit.acked, usSplit.lost)
	fmt.Printf("| EU clients, after promotion | %d | %d |\n", euAfter.acked, euAfter.lost)
	rto := "never"
	if firstEUAfter >= 0 {
		rto = fmt.Sprintf("%.1fs", (firstEUAfter - tPart).Seconds())
	}
	fmt.Printf("\nEU write outage (partition -> first EU write acknowledged): %s\n", rto)
	if f := fencedAt.Load(); f > 0 {
		fmt.Printf("US primary fenced itself at %.1fs (%.1fs after the partition)\n",
			time.Duration(f).Seconds(), (time.Duration(f) - tPart).Seconds())
	}
	fmt.Printf("Failed write attempts (timeouts, refused, read-only): %d\n", failures.Load())
}

// pgSSLRequest makes any PostgreSQL server answer one byte ('N' or 'S').
var pgSSLRequest = []byte{0, 0, 0, 8, 4, 210, 22, 47}

// fence makes the old primary refuse writes: new transactions are
// read-only, existing sessions are dropped.
func fence(ctx context.Context, us *pgxpool.Pool) {
	must2(us.Exec(ctx, `ALTER SYSTEM SET default_transaction_read_only = on`))
	must2(us.Exec(ctx, `SELECT pg_reload_conf()`))
	must2(us.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()`))
}

func resetCluster(mode string) {
	svcs := []string{"pg-us", "pg-eu", "wan-repl", "wan-witness", "vip-eu"}
	run(nil, append([]string{"compose", "--profile", "multiregion", "rm", "-sfv"}, svcs...)...)
	run(nil, append([]string{"compose", "--profile", "multiregion", "up", "-d"}, svcs...)...)
	waitStreaming("async")
	if mode == "sync" {
		ctx := context.Background()
		c, err := pgx.Connect(ctx, usDSN)
		must(err)
		must2(c.Exec(ctx, `ALTER SYSTEM SET synchronous_standby_names = 'pg_eu'`))
		must2(c.Exec(ctx, `SELECT pg_reload_conf()`))
		c.Close(ctx)
		waitStreaming("sync")
	}
}

func waitStreaming(want string) {
	for i := 0; i < 120; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		c, err := pgx.Connect(ctx, usDSN)
		if err == nil {
			var state string
			err = c.QueryRow(ctx, `SELECT sync_state FROM pg_stat_replication WHERE application_name = 'pg_eu' AND state = 'streaming'`).Scan(&state)
			c.Close(ctx)
			if err == nil && state == want {
				cancel()
				log.Printf("cluster ready: pg_eu streaming (%s)", state)
				return
			}
		}
		cancel()
		time.Sleep(time.Second)
	}
	log.Fatal("replica did not reach streaming state")
}

func waitReplicaHasTable(ctx context.Context) {
	for i := 0; i < 50; i++ {
		c, err := pgx.Connect(ctx, euDSN)
		if err == nil {
			var ok bool
			err = c.QueryRow(ctx, `SELECT to_regclass('drill_writes') IS NOT NULL`).Scan(&ok)
			c.Close(ctx)
			if err == nil && ok {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Fatal("table did not replicate")
}

func run(env []string, args ...string) {
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Fatalf("docker %v: %v\n%s", args, err, out)
	}
}

func post(u string) {
	resp, err := http.Post(u, "text/plain", nil)
	if err != nil {
		log.Fatal(err)
	}
	resp.Body.Close()
}

func connect(ctx context.Context, dsn string) *pgxpool.Pool {
	cfg, err := pgxpool.ParseConfig(dsn)
	must(err)
	cfg.ConnConfig.ConnectTimeout = time.Second
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	return p
}

func p50(d []time.Duration) string {
	if len(d) == 0 {
		return "-"
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return fmt.Sprintf("%.1f ms", float64(d[len(d)/2].Microseconds())/1000)
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func must2[T any](_ T, err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
