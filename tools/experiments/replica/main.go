// Command replica measures read-your-writes across a lagging read replica.
//
//	REPLICA_APPLY_DELAY=1s docker compose up -d
//	go run ./tools/experiments/replica -n 100
//
// For each iteration a user creates a post and immediately lists their own
// posts, once without and once with the consistency token from the write. A
// "violation" is the new post missing from the read. It also reports where
// reads were routed (from the API's Prometheus counters).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

var api = flag.String("api", "http://localhost:8080", "API base URL")

func main() {
	n := flag.Int("n", 100, "write-then-read iterations per mode")
	flag.Parse()
	user := fmt.Sprintf("ryw_%d", rand.IntN(1_000_000))
	call("POST", "/users", "", "", map[string]string{"username": user, "password": "password123"})
	var login struct{ Token string }
	_, body := call("POST", "/auth/login", "", "", map[string]string{"username": user, "password": "password123"})
	json.Unmarshal(body, &login) //nolint:errcheck

	fmt.Println("| Mode | Iterations | Own post missing right after writing | Read P50 |")
	fmt.Println("|---|---|---|---|")
	for _, withToken := range []bool{false, true} {
		before := routes()
		missing := 0
		var lat []time.Duration
		for i := 0; i < *n; i++ {
			msg := fmt.Sprintf("ryw probe %d %d", i, rand.Int())
			h, b := call("POST", "/posts", login.Token, "", map[string]string{"message": msg})
			var p struct{ ID string }
			json.Unmarshal(b, &p) //nolint:errcheck
			tok := ""
			if withToken {
				tok = h.Get("X-Consistency-Token")
			}
			t0 := time.Now()
			_, lb := call("GET", "/posts?author="+user+"&limit=1", "", tok, nil)
			lat = append(lat, time.Since(t0))
			var page struct{ Items []struct{ ID string } }
			json.Unmarshal(lb, &page) //nolint:errcheck
			if len(page.Items) == 0 || page.Items[0].ID != p.ID {
				missing++
			}
		}
		after := routes()
		mode := "No token (plain read/write splitting)"
		if withToken {
			mode = "Consistency token (read-your-writes)"
		}
		fmt.Printf("| %s | %d | **%d (%.0f%%)** | %s |\n", mode, *n, missing, 100*float64(missing)/float64(*n), median(lat))
		fmt.Printf("|  | routed: %s |  |  |\n", diff(before, after))
	}

	// Offload: plain browsing traffic (no token) should all hit the replica.
	before := routes()
	for i := 0; i < 500; i++ {
		call("GET", "/posts?limit=20", "", "", nil)
	}
	fmt.Printf("\n500 anonymous browse requests routed: %s\n", diff(before, routes()))
}

func call(method, path, token, consistency string, body any) (http.Header, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, *api+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if consistency != "" {
		req.Header.Set("X-Consistency-Token", consistency)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.Header, b
}

var routeRe = regexp.MustCompile(`networkai_db_read_routes_total\{target="([a-z_]+)"\} ([0-9.e+]+)`)

func routes() map[string]float64 {
	_, b := call("GET", "/metrics", "", "", nil)
	out := map[string]float64{}
	for _, m := range routeRe.FindAllStringSubmatch(string(b), -1) {
		v, _ := strconv.ParseFloat(m[2], 64)
		out[m[1]] = v
	}
	return out
}

func diff(a, b map[string]float64) string {
	s := ""
	for _, k := range []string{"replica", "primary_token", "primary_forced", "primary_error"} {
		if d := b[k] - a[k]; d > 0 {
			s += fmt.Sprintf("%s=%.0f ", k, d)
		}
	}
	return s
}

func median(d []time.Duration) string {
	for i := range d { // insertion sort; n is small
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
	return fmt.Sprintf("%.1f ms", float64(d[len(d)/2].Microseconds())/1000)
}
