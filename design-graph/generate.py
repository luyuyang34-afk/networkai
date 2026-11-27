#!/usr/bin/env python3
"""Generates the evolution diagrams in design-graph/evolution/.

    python3 design-graph/generate.py          # writes the SVGs
    node design-graph/render.js               # renders PNGs (needs Playwright)

Every step uses the same layout, so components keep their place from one
step to the next and the reader sees the system grow. Each step marks what
is new (green), changed (amber) and removed (grey, dashed).
"""
import html
import os

W, H = 1240, 660
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "evolution")

# id: (x, y, w, h)
POS = {
    "client":  (30, 255, 160, 74),
    "api":     (240, 225, 190, 134),
    "pg":      (490, 80, 240, 86),
    "replica": (490, 182, 240, 50),
    "redis":   (490, 248, 240, 76),
    "es":      (490, 340, 240, 70),
    "llm":     (490, 426, 240, 46),
    "media":   (490, 488, 240, 50),
    "relay":   (790, 80, 190, 64),
    "indexer": (790, 190, 190, 76),
    "kafka":   (1020, 80, 200, 64),
    "c_stats": (1020, 164, 200, 44),
    "c_fan":   (1020, 220, 200, 44),
    "c_cache": (1020, 276, 200, 44),
    "c_search": (1020, 352, 200, 44),
    "prom":    (240, 420, 190, 62),
    "shards":  (790, 432, 430, 70),
    "regions": (790, 520, 430, 70),
}

KIND = {  # fill, stroke
    "client": ("#eef0f4", "#5b6474"),
    "svc": ("#e8f1fb", "#2f6fb3"),
    "db": ("#fdf3e7", "#c27c2c"),
    "queue": ("#f3ecfa", "#7b4fb3"),
    "ext": ("#ffffff", "#8a93a3"),
    "sub": ("#f4f6f8", "#5b6474"),
}
STATUS = {  # fill, stroke, tag
    "new": ("#e3f5ea", "#1f8a4c", "NEW"),
    "changed": ("#fff4d6", "#b7791f", "CHANGED"),
    "removed": ("#f2f3f5", "#a0a7b4", "REMOVED"),
}


def esc(s):
    return html.escape(s, quote=True)


def node(nid, title, lines, kind, status=None, at=None):
    if at:
        POS[nid] = at
    x, y, w, h = POS[nid]
    fill, stroke = KIND[kind]
    dash = ' stroke-dasharray="5 4"' if kind == "ext" else ""
    tag = ""
    title_fill = "#1b1f27"
    if status:
        fill, stroke, label = STATUS[status]
        if status == "removed":
            dash = ' stroke-dasharray="5 4"'
            title_fill = "#8a93a3"
        tw = 9 + 7.2 * len(label)
        tag = (f'<rect x="{x + w - tw - 6}" y="{y - 9}" width="{tw}" height="18" rx="9" fill="{stroke}"/>'
               f'<text x="{x + w - tw / 2 - 6}" y="{y + 4}" text-anchor="middle" class="tag">{label}</text>')
    out = [f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="8" fill="{fill}" stroke="{stroke}" stroke-width="1.6"{dash}/>',
           f'<text x="{x + 12}" y="{y + 22}" class="t" fill="{title_fill}">{esc(title)}</text>']
    for i, ln in enumerate(lines):
        out.append(f'<text x="{x + 12}" y="{y + 40 + i * 16}" class="s">{esc(ln)}</text>')
    if status == "removed":
        out.append(f'<line x1="{x + 8}" y1="{y + 17}" x2="{x + 12 + 7.6 * len(title)}" y2="{y + 17}" stroke="#a0a7b4" stroke-width="1.5"/>')
    out.append(tag)
    return "\n".join(out)


def anchor(nid, side, off=0):
    x, y, w, h = POS[nid]
    return {"r": (x + w, y + h / 2 + off), "l": (x, y + h / 2 + off),
            "t": (x + w / 2 + off, y), "b": (x + w / 2 + off, y + h)}[side]


def edge(src, dst, label="", style="req", s_side=None, d_side=None, s_off=0, d_off=0, lx=None, ly=None):
    sx = POS[src][0]
    dx = POS[dst][0]
    if s_side is None:
        s_side, d_side = ("r", "l") if dx > sx else ("l", "r")
    x1, y1 = anchor(src, s_side, s_off)
    x2, y2 = anchor(dst, d_side, d_off)
    color = {"req": "#3b4250", "stream": "#7b4fb3", "async": "#3b4250", "new": "#1f8a4c",
             "newasync": "#1f8a4c", "removed": "#a0a7b4"}[style]
    dash = ' stroke-dasharray="6 4"' if style in ("async", "newasync", "removed") else ""
    marker = {"#3b4250": "a", "#7b4fb3": "p", "#1f8a4c": "g", "#a0a7b4": "x"}[color]
    if s_side in ("r", "l"):
        mx = (x1 + x2) / 2
        d = f"M{x1},{y1} C{mx},{y1} {mx},{y2} {x2},{y2}"
    else:
        my = (y1 + y2) / 2
        d = f"M{x1},{y1} C{x1},{my} {x2},{my} {x2},{y2}"
    out = [f'<path d="{d}" fill="none" stroke="{color}" stroke-width="1.7"{dash} marker-end="url(#{marker})"/>']
    if label:
        tx = lx if lx is not None else (x1 + x2) / 2
        ty = ly if ly is not None else (y1 + y2) / 2 - 6
        out.append(f'<text x="{tx}" y="{ty}" class="l" text-anchor="middle" fill="{color}">{esc(label)}</text>')
    return "\n".join(out)


def bus(targets, style="stream"):
    """Kafka fan-out spine into consumer groups."""
    color = "#1f8a4c" if style == "new" else "#7b4fb3"
    m = "g" if style == "new" else "p"
    kx, ky, kw, kh = POS["kafka"]
    sx = kx - 14
    ys = [POS[t][1] + POS[t][3] / 2 for t in targets]
    out = [f'<path d="M{kx},{ky + kh / 2} L{sx},{ky + kh / 2} L{sx},{max(ys)}" fill="none" stroke="{color}" stroke-width="1.7"/>']
    for t, y in zip(targets, ys):
        out.append(f'<path d="M{sx},{y} L{POS[t][0]},{y}" fill="none" stroke="{color}" stroke-width="1.7" marker-end="url(#{m})"/>')
    return "\n".join(out)


def svg(step, title, subtitle, body):
    return f'''<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" font-family="'Source Sans 3','Helvetica Neue',Arial,sans-serif">
<defs>
<marker id="a" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="#3b4250"/></marker>
<marker id="p" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="#7b4fb3"/></marker>
<marker id="g" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="#1f8a4c"/></marker>
<marker id="x" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="#a0a7b4"/></marker>
<style>
.t{{font-size:15px;font-weight:700}}
.s{{font-size:12.5px;fill:#4a5262}}
.l{{font-size:12px;paint-order:stroke;stroke:#ffffff;stroke-width:4px;stroke-linejoin:round}}
.tag{{font-size:11px;font-weight:700;fill:#ffffff;letter-spacing:.04em}}
</style>
</defs>
<rect width="{W}" height="{H}" fill="#ffffff"/>
<text x="30" y="40" font-size="23" font-weight="700" fill="#1b1f27">Step {step}: {esc(title)}</text>
<text x="30" y="62" font-size="14" fill="#4a5262">{esc(subtitle)}</text>
{body}
<g transform="translate(30,{H - 30})">
<rect x="0" y="-11" width="14" height="14" rx="3" fill="#e3f5ea" stroke="#1f8a4c"/><text x="20" y="1" class="l" fill="#3b4250">new in this step</text>
<rect x="135" y="-11" width="14" height="14" rx="3" fill="#fff4d6" stroke="#b7791f"/><text x="155" y="1" class="l" fill="#3b4250">changed</text>
<rect x="225" y="-11" width="14" height="14" rx="3" fill="#f2f3f5" stroke="#a0a7b4" stroke-dasharray="3 2"/><text x="245" y="1" class="l" fill="#3b4250">removed</text>
<path d="M320,-4 L352,-4" stroke="#3b4250" stroke-width="1.7" marker-end="url(#a)"/><text x="360" y="1" class="l" fill="#3b4250">request</text>
<path d="M425,-4 L457,-4" stroke="#7b4fb3" stroke-width="1.7" marker-end="url(#p)"/><text x="465" y="1" class="l" fill="#3b4250">change stream</text>
<path d="M565,-4 L597,-4" stroke="#3b4250" stroke-width="1.7" stroke-dasharray="6 4" marker-end="url(#a)"/><text x="605" y="1" class="l" fill="#3b4250">async / derived</text>
</g>
</svg>
'''


STEPS = []


def step(n, slug, title, subtitle, parts):
    STEPS.append((n, slug, title, subtitle, "\n".join(parts)))


# ---- Step 0: the prototype ----
step(0, "prototype", "The original prototype",
     "One Go service writing straight to Elasticsearch and Google Cloud Storage.", [
         node("client", "Client", ["web / curl"], "client"),
         node("api", "Go API", ["handlers + JWT", "no timeouts", "unpaginated search"], "svc"),
         node("es", "Elasticsearch", ["the only datastore:", "users + posts, plaintext passwords"], "db"),
         node("media", "Google Cloud Storage", ["public-ACL objects"], "db"),
         edge("client", "api", "REST"),
         edge("api", "es", "read + write", d_off=-8, s_off=20),
         edge("api", "media", "upload, then write ES", s_off=55, d_off=0, lx=455, ly=500),
     ])

# ---- Step 1: PostgreSQL + outbox ----
step(1, "source-of-truth", "PostgreSQL as source of truth, transactional outbox",
     "Writes go to PostgreSQL in one transaction with an outbox row; a worker keeps the search index in sync.", [
         node("client", "Client", ["web / curl"], "client"),
         node("api", "Go API", ["bcrypt, pinned JWT", "ownership checks (row lock)", "keyset pagination", "uniform error contract"], "svc", "changed"),
         node("pg", "PostgreSQL", ["users, posts, outbox", "post + outbox row in 1 txn"], "db", "new"),
         node("es", "Elasticsearch", ["derived search index", "external versions, tombstones"], "db", "changed"),
         node("media", "Media store interface", ["disk or GCS, type sniffed"], "db", "changed"),
         node("indexer", "Outbox worker", ["SKIP LOCKED, retries", "pauses on dependency outage"], "svc", "new"),
         edge("client", "api", "REST"),
         edge("api", "pg", "write: post + outbox", style="new", s_off=-45, lx=455, ly=150),
         edge("api", "es", "search", s_off=30, lx=455, ly=360),
         edge("api", "media", "upload", s_off=55, lx=455, ly=500),
         edge("pg", "indexer", "poll outbox", style="new", s_off=15, d_off=-18),
         edge("indexer", "es", "index (versioned)", style="newasync", d_off=-10, lx=760, ly=330),
     ])

# ---- Step 2: operate and measure ----
step(2, "operate-measure", "A real UI, metrics, alerts and load tests",
     "Next.js frontend; Prometheus metrics and alerts; k6 load tests; bulk reindex; cleanup jobs.", [
         node("client", "Next.js web", ["TypeScript, TanStack Query", "Playwright e2e"], "client", "changed"),
         node("api", "Go API", ["bcrypt, pinned JWT", "keyset pagination", "metrics endpoint"], "svc", "changed"),
         node("pg", "PostgreSQL", ["users, posts, outbox", "post + outbox row in 1 txn"], "db"),
         node("es", "Elasticsearch", ["derived search index", "external versions, tombstones"], "db"),
         node("media", "Media store", ["disk or GCS, orphan sweeper"], "db", "changed"),
         node("indexer", "Outbox worker", ["_bulk reindex (134 s -> 4.7 s)", "tombstone purge job"], "svc", "changed"),
         node("prom", "Prometheus", ["metrics, alerts, runbook"], "svc", "new"),
         edge("client", "api", "REST"),
         edge("api", "pg", "write: post + outbox", s_off=-45, lx=455, ly=150),
         edge("api", "es", "search", s_off=30, lx=455, ly=360),
         edge("api", "media", "upload", s_off=55, lx=455, ly=500),
         edge("pg", "indexer", "poll outbox", s_off=15, d_off=-18),
         edge("indexer", "es", "index (versioned)", style="async", d_off=-10, lx=760, ly=330),
         edge("prom", "api", "scrape", style="newasync", s_side="t", d_side="b", lx=360, ly=398),
     ])

# ---- Step 3: personalised feed + LLM ----
step(3, "feed-llm", "Personalised feed and an LLM writing assistant",
     "Two-stage recommender on Elasticsearch kNN with a bulkhead; LLM assistant behind timeouts and limits.", [
         node("client", "Next.js web", ["For you tab, optimistic likes", "Improve with AI"], "client", "changed"),
         node("api", "Go API", ["two-stage feed:", "kNN -> ranker -> diversity", "feed bulkhead", "LLM limits, cost metrics"], "svc", "changed"),
         node("pg", "PostgreSQL", ["users, posts, likes, outbox", "ranking signals"], "db", "changed"),
         node("es", "Elasticsearch", ["+ post embeddings, kNN", "blue/green rebuilds via alias"], "db", "changed"),
         node("llm", "LLM API", ["draft assistant (optional)"], "ext", "new"),
         node("media", "Media store", ["disk or GCS"], "db"),
         node("indexer", "Outbox worker", ["embeds + indexes posts"], "svc", "changed"),
         node("prom", "Prometheus", ["metrics, alerts"], "svc"),
         edge("client", "api", "REST"),
         edge("api", "pg", "write + signals", s_off=-45, lx=455, ly=150),
         edge("api", "es", "search + kNN", s_off=30, lx=455, ly=360),
         edge("api", "llm", "rewrite", style="newasync", s_off=45, lx=455, ly=430),
         edge("api", "media", "upload", s_off=60, lx=455, ly=520),
         edge("pg", "indexer", "poll outbox", s_off=15, d_off=-18),
         edge("indexer", "es", "index + embed", style="async", d_off=-10, lx=760, ly=330),
         edge("prom", "api", "scrape", style="async", s_side="t", d_side="b", lx=360, ly=398),
     ])

# ---- Step 4: follows + hybrid fan-out ----
step(4, "timelines", "Follow graph and hybrid fan-out home timelines",
     "Regular authors are pushed to followers' Redis timelines; celebrities are pulled at read time.", [
         node("client", "Next.js web", ["Following tab, follow buttons"], "client", "changed"),
         node("api", "Go API", ["home timeline: Redis range", "+ pull celebrities from PG", "two-stage feed, bulkhead"], "svc", "changed"),
         node("pg", "PostgreSQL", ["+ follows, follower counts", "posts, likes, outbox"], "db", "changed"),
         node("redis", "Redis", ["home timelines (sorted sets)", "capped, expire when unread"], "db", "new"),
         node("es", "Elasticsearch", ["search + kNN"], "db"),
         node("llm", "LLM API", ["draft assistant"], "ext"),
         node("media", "Media store", ["disk or GCS"], "db"),
         node("indexer", "Outbox worker", ["index AND fan-out", "in one loop (coupled)"], "svc", "changed"),
         node("prom", "Prometheus", ["+ fan-out metrics"], "svc"),
         edge("client", "api", "REST"),
         edge("api", "pg", "write, pull celebs", s_off=-45, lx=455, ly=150),
         edge("api", "redis", "timeline", style="new", s_off=-5, lx=455, ly=268),
         edge("api", "es", "search + kNN", s_off=30, lx=455, ly=360),
         edge("pg", "indexer", "poll outbox", s_off=15, d_off=-18),
         edge("indexer", "redis", "fan-out (push)", style="newasync", d_off=0, lx=760, ly=262),
         edge("indexer", "es", "index", style="async", d_off=-10, lx=760, ly=356),
         edge("prom", "api", "scrape", style="async", s_side="t", d_side="b", lx=360, ly=398),
     ])

# ---- Step 5: CDC + Kafka ----
step(5, "cdc-kafka", "CDC from the WAL into Kafka consumer groups",
     "One job per consumer group, so a slow or failing job only delays itself. The polling worker is retired.", [
         node("client", "Next.js web", [], "client"),
         node("api", "Go API", ["home timeline, feed", "writes post + outbox"], "svc"),
         node("pg", "PostgreSQL", ["posts, follows, likes, outbox", "publication + replication slot"], "db", "changed"),
         node("redis", "Redis", ["home timelines"], "db"),
         node("es", "Elasticsearch", ["search + kNN"], "db"),
         node("llm", "LLM API", ["draft assistant"], "ext"),
         node("media", "Media store", ["disk or GCS"], "db"),
         node("relay", "CDC relay", ["logical decoding", "confirm LSN after publish"], "svc", "new"),
         node("indexer", "Polling worker", ["kept only for comparison"], "svc", "removed", at=(790, 440, 190, 60)),
         node("kafka", "Kafka (Redpanda)", ["post-events, 12 partitions"], "queue", "new"),
         node("c_stats", "post-stats", ["idempotent, replays history"], "svc", "new"),
         node("c_fan", "timeline-fanout", ["push to followers"], "svc", "new"),
         node("c_search", "search-indexer", ["external versions"], "svc", "new"),
         node("prom", "Prometheus", ["+ consumer lag, slot WAL alerts"], "svc", "changed"),
         edge("client", "api", "REST"),
         edge("api", "pg", "write", s_off=-45, lx=455, ly=150),
         edge("api", "redis", "timeline", s_off=-5, lx=455, ly=268),
         edge("api", "es", "search", s_off=30, lx=455, ly=360),
         edge("pg", "relay", "WAL", style="stream", s_off=-10),
         edge("relay", "kafka", "publish", style="stream"),
         bus(["c_stats", "c_fan", "c_search"], "new"),
         edge("c_stats", "pg", "counts", style="newasync", d_off=28, lx=900, ly=168),
         edge("c_fan", "redis", "push", style="newasync", lx=900, ly=246),
         edge("c_search", "es", "index", style="newasync", lx=900, ly=362),
         edge("prom", "api", "scrape", style="async", s_side="t", d_side="b", lx=360, ly=398),
     ])

POS["indexer"] = (790, 190, 190, 76)

# ---- Step 6: read replica + tokens ----
step(6, "read-replica", "Read replica with consistency tokens",
     "Staleness-tolerant reads go to a streaming replica; an LSN token keeps read-your-writes exact.", [
         node("client", "Next.js web", ["sends X-Consistency-Token"], "client", "changed"),
         node("api", "Go API", ["routes reads by token:", "replica if caught up,", "else primary"], "svc", "changed"),
         node("pg", "PostgreSQL primary", ["writes, cache fills"], "db", "changed"),
         node("replica", "Streaming replica", ["staleness-tolerant reads"], "db", "new"),
         node("redis", "Redis", ["home timelines"], "db"),
         node("es", "Elasticsearch", ["search + kNN"], "db"),
         node("llm", "LLM API", ["draft assistant"], "ext"),
         node("media", "Media store", ["disk or GCS"], "db"),
         node("relay", "CDC relay", ["logical decoding"], "svc"),
         node("kafka", "Kafka (Redpanda)", ["post-events, 12 partitions"], "queue"),
         node("c_stats", "post-stats", [], "svc"),
         node("c_fan", "timeline-fanout", [], "svc"),
         node("c_search", "search-indexer", [], "svc"),
         node("prom", "Prometheus", ["+ replica lag, read routes"], "svc", "changed"),
         edge("client", "api", "REST + token"),
         edge("api", "pg", "write", s_off=-50, lx=455, ly=150),
         edge("api", "replica", "read", style="new", s_off=-25, lx=455, ly=212),
         edge("api", "redis", "timeline", s_off=0, lx=455, ly=288),
         edge("api", "es", "search", s_off=30, lx=455, ly=370),
         edge("pg", "replica", "", style="newasync", s_side="b", d_side="t"),
         edge("pg", "relay", "WAL", style="stream", s_off=-10),
         edge("relay", "kafka", "publish", style="stream"),
         bus(["c_stats", "c_fan", "c_search"]),
         edge("c_stats", "pg", "", style="async", d_off=28),
         edge("c_fan", "redis", "", style="async"),
         edge("c_search", "es", "", style="async"),
         edge("prom", "api", "scrape", style="async", s_side="t", d_side="b", lx=360, ly=398),
     ])

# ---- Step 7: hot keys ----
step(7, "hot-keys", "Hot keys: sharded counters and a lease-protected post cache",
     "Viral posts no longer queue on one row or stampede the database; a CDC group invalidates the cache.", [
         node("client", "Next.js web", [], "client"),
         node("api", "Go API", ["post cache: singleflight,", "leases, XFetch", "token-routed reads"], "svc", "changed"),
         node("pg", "PostgreSQL primary", ["+ sharded like counters", "(16 rows per post)"], "db", "changed"),
         node("replica", "Streaming replica", [], "db"),
         node("redis", "Redis", ["home timelines", "+ hot-post cache + leases"], "db", "changed"),
         node("es", "Elasticsearch", ["search + kNN"], "db"),
         node("llm", "LLM API", ["draft assistant"], "ext"),
         node("media", "Media store", ["disk or GCS"], "db"),
         node("relay", "CDC relay", ["logical decoding"], "svc"),
         node("kafka", "Kafka (Redpanda)", ["post-events, 12 partitions"], "queue"),
         node("c_stats", "post-stats", [], "svc"),
         node("c_fan", "timeline-fanout", [], "svc"),
         node("c_cache", "cache-invalidation", ["second delete path"], "svc", "new"),
         node("c_search", "search-indexer", [], "svc"),
         node("prom", "Prometheus", ["+ cache hit / lease metrics"], "svc", "changed"),
         edge("client", "api", "REST + token"),
         edge("api", "pg", "write", s_off=-50, lx=455, ly=150),
         edge("api", "replica", "read", s_off=-25, lx=455, ly=212),
         edge("api", "redis", "cache + timeline", style="new", s_off=0, lx=455, ly=288),
         edge("api", "es", "search", s_off=30, lx=455, ly=370),
         edge("pg", "replica", "", style="async", s_side="b", d_side="t"),
         edge("pg", "relay", "WAL", style="stream", s_off=-10),
         edge("relay", "kafka", "publish", style="stream"),
         bus(["c_stats", "c_fan", "c_cache", "c_search"]),
         edge("c_stats", "pg", "", style="async", d_off=28),
         edge("c_fan", "redis", "", style="async", d_off=-15),
         edge("c_cache", "redis", "invalidate", style="newasync", d_off=15, lx=900, ly=300),
         edge("c_search", "es", "", style="async"),
         edge("prom", "api", "scrape", style="async", s_side="t", d_side="b", lx=360, ly=398),
     ])

# ---- Step 8: sharding ----
step(8, "sharding", "Sharded post store with online resharding",
     "64 logical shards by author; fenced online moves between PostgreSQL nodes. Built and measured as a subsystem.", [
         node("client", "Next.js web", [], "client"),
         node("api", "Go API", ["post cache, token routing", "(still on one primary)"], "svc"),
         node("pg", "PostgreSQL primary", ["sharded like counters"], "db"),
         node("replica", "Streaming replica", [], "db"),
         node("redis", "Redis", ["timelines + hot-post cache"], "db"),
         node("es", "Elasticsearch", ["search + kNN"], "db"),
         node("llm", "LLM API", ["draft assistant"], "ext"),
         node("media", "Media store", ["disk or GCS"], "db"),
         node("relay", "CDC relay", [], "svc"),
         node("kafka", "Kafka (Redpanda)", [], "queue"),
         node("c_stats", "post-stats", [], "svc"),
         node("c_fan", "timeline-fanout", [], "svc"),
         node("c_cache", "cache-invalidation", [], "svc"),
         node("c_search", "search-indexer", [], "svc"),
         node("prom", "Prometheus", [], "svc"),
         node("shards", "Sharded post store (subsystem)", ["router -> 64 logical shards -> nodes N1..N4, ownership checked on the node",
                                                            "copy rounds from snapshot xmin, per-shard fence, then hand-over"], "sub", "new"),
         edge("client", "api", "REST + token"),
         edge("api", "pg", "write", s_off=-50, lx=455, ly=150),
         edge("api", "replica", "read", s_off=-25, lx=455, ly=212),
         edge("api", "redis", "", s_off=0),
         edge("api", "es", "", s_off=30),
         edge("pg", "replica", "", style="async", s_side="b", d_side="t"),
         edge("pg", "relay", "", style="stream", s_off=-10),
         edge("relay", "kafka", "", style="stream"),
         bus(["c_stats", "c_fan", "c_cache", "c_search"]),
         edge("c_stats", "pg", "", style="async", d_off=28),
         edge("c_fan", "redis", "", style="async", d_off=-15),
         edge("c_cache", "redis", "", style="async", d_off=15),
         edge("c_search", "es", "", style="async"),
         edge("prom", "api", "", style="async", s_side="t", d_side="b"),
     ])

# ---- Step 9: multi-region + SLOs ----
step(9, "multi-region-slo", "Two regions, one-round-trip writes and SLOs",
     "Cross-region writes cost one round trip; failover is fenced by a leader lease; alerts follow error budgets.", [
         node("client", "Next.js web", [], "client"),
         node("api", "Go API", ["post + outbox + token", "in 1 round trip (pipelined)", "post cache, token routing"], "svc", "changed"),
         node("pg", "PostgreSQL primary", ["sharded like counters"], "db"),
         node("replica", "Streaming replica", [], "db"),
         node("redis", "Redis", ["timelines + hot-post cache"], "db"),
         node("es", "Elasticsearch", ["search + kNN"], "db"),
         node("llm", "LLM API", ["draft assistant"], "ext"),
         node("media", "Media store", ["disk or GCS"], "db"),
         node("relay", "CDC relay", ["clean shutdown mid-publish"], "svc", "changed"),
         node("kafka", "Kafka (Redpanda)", [], "queue"),
         node("c_stats", "post-stats", [], "svc"),
         node("c_fan", "timeline-fanout", [], "svc"),
         node("c_cache", "cache-invalidation", [], "svc"),
         node("c_search", "search-indexer", [], "svc"),
         node("prom", "Prometheus", ["SLOs + burn-rate alerts", "tested with promtool in CI"], "svc", "changed"),
         node("shards", "Sharded post store (subsystem)", ["64 logical shards, fenced online moves"], "sub"),
         node("regions", "Two-region drill (subsystem)", ["US primary -> EU replica over 70 ms WAN, switchable VIP",
                                                         "leader lease + self-fencing on failover"], "sub", "new"),
         edge("client", "api", "REST + token"),
         edge("api", "pg", "write (1 RTT)", style="new", s_off=-50, lx=455, ly=150),
         edge("api", "replica", "read", s_off=-25, lx=455, ly=212),
         edge("api", "redis", "", s_off=0),
         edge("api", "es", "", s_off=30),
         edge("pg", "replica", "", style="async", s_side="b", d_side="t"),
         edge("pg", "relay", "", style="stream", s_off=-10),
         edge("relay", "kafka", "", style="stream"),
         bus(["c_stats", "c_fan", "c_cache", "c_search"]),
         edge("c_stats", "pg", "", style="async", d_off=28),
         edge("c_fan", "redis", "", style="async", d_off=-15),
         edge("c_cache", "redis", "", style="async", d_off=15),
         edge("c_search", "es", "", style="async"),
         edge("prom", "api", "", style="async", s_side="t", d_side="b"),
     ])


if __name__ == "__main__":
    os.makedirs(OUT, exist_ok=True)
    for n, slug, title, subtitle, body in STEPS:
        path = os.path.join(OUT, f"step-{n}-{slug}.svg")
        with open(path, "w") as f:
            f.write(svg(n, title, subtitle, body))
        print(path)
