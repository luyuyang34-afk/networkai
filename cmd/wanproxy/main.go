// Command wanproxy is a TCP proxy that makes a local link behave like a
// cross-region one, for multi-region experiments on a single machine.
//
//	wanproxy -listen :5432 -target postgres:5432 -delay 35ms -admin :9100
//
// Every chunk of bytes is delivered -delay after it was read, in order, in
// each direction, so a request/response costs 2 x delay (the RTT).
//
// The admin endpoint changes the link at run time:
//
//	POST /delay?d=50ms        new one-way delay
//	POST /partition?on=true   drop all connections, refuse new ones
//	POST /target?addr=h:p     send new connections elsewhere (a "VIP" flip
//	                          during failover); existing ones are closed
//	GET  /                    current settings
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

type link struct {
	mu        sync.Mutex
	target    string
	delay     time.Duration
	partition bool
	conns     map[net.Conn]struct{}
}

func (l *link) settings() (string, time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.target, l.delay, l.partition
}

func (l *link) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.conns {
		c.Close()
	}
	l.conns = map[net.Conn]struct{}{}
}

func (l *link) track(cs ...net.Conn) {
	l.mu.Lock()
	for _, c := range cs {
		l.conns[c] = struct{}{}
	}
	l.mu.Unlock()
}

func (l *link) untrack(cs ...net.Conn) {
	l.mu.Lock()
	for _, c := range cs {
		delete(l.conns, c)
	}
	l.mu.Unlock()
}

func main() {
	listen := flag.String("listen", ":5432", "listen address")
	target := flag.String("target", "postgres:5432", "upstream address")
	delay := flag.Duration("delay", 35*time.Millisecond, "one-way delay")
	admin := flag.String("admin", ":9100", "admin HTTP address")
	flag.Parse()
	l := &link{target: *target, delay: *delay, conns: map[net.Conn]struct{}{}}

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /delay", func(w http.ResponseWriter, r *http.Request) {
			d, err := time.ParseDuration(r.URL.Query().Get("d"))
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			l.mu.Lock()
			l.delay = d
			l.mu.Unlock()
			log.Printf("delay -> %s", d)
		})
		mux.HandleFunc("POST /partition", func(w http.ResponseWriter, r *http.Request) {
			on := r.URL.Query().Get("on") == "true"
			l.mu.Lock()
			l.partition = on
			l.mu.Unlock()
			if on {
				l.closeAll()
			}
			log.Printf("partition -> %v", on)
		})
		mux.HandleFunc("POST /target", func(w http.ResponseWriter, r *http.Request) {
			addr := r.URL.Query().Get("addr")
			if addr == "" {
				http.Error(w, "addr required", 400)
				return
			}
			l.mu.Lock()
			l.target = addr
			l.mu.Unlock()
			l.closeAll()
			log.Printf("target -> %s", addr)
		})
		mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			t, d, p := l.settings()
			fmt.Fprintf(w, "target=%s delay=%s partition=%v\n", t, d, p)
		})
		log.Fatal(http.ListenAndServe(*admin, mux))
	}()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("wanproxy %s -> %s, one-way delay %s", *listen, *target, *delay)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go l.serve(c)
	}
}

func (l *link) serve(client net.Conn) {
	target, _, partition := l.settings()
	if partition {
		client.Close()
		return
	}
	up, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		client.Close()
		return
	}
	l.track(client, up)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); l.pipe(up, client) }()
	go func() { defer wg.Done(); l.pipe(client, up) }()
	wg.Wait()
	l.untrack(client, up)
}

type chunk struct {
	due  time.Time
	data []byte
}

// pipe copies src to dst, delivering each chunk `delay` after it was read.
// A reader goroutine timestamps chunks; the writer sleeps until each is due.
// This models propagation delay without limiting bandwidth.
func (l *link) pipe(dst, src net.Conn) {
	q := make(chan chunk, 4096)
	go func() {
		defer close(q)
		buf := make([]byte, 64<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				_, d, _ := l.settings()
				q <- chunk{time.Now().Add(d), append([]byte(nil), buf[:n]...)}
			}
			if err != nil {
				return
			}
		}
	}()
	for c := range q {
		if w := time.Until(c.due); w > 0 {
			time.Sleep(w)
		}
		if _, err := dst.Write(c.data); err != nil {
			break
		}
	}
	// Half-close so the other direction can drain, then close.
	if tc, ok := dst.(*net.TCPConn); ok {
		tc.CloseWrite() //nolint:errcheck
	} else {
		dst.Close()
	}
	io.Copy(io.Discard, src) //nolint:errcheck
	src.Close()
	dst.Close()
}
