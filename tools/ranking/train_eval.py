#!/usr/bin/env python3
"""Train the feed models offline and evaluate them against baselines.

    python3 tools/ranking/train_eval.py --data data/synth --models models --report docs/ranking-eval.md

Produces:
  models/lsa.json     text embedding model (TF-IDF + truncated SVD, "LSA")
  models/ranker.json  logistic-regression ranker (feature normalisation + weights)
  a Markdown report with nDCG@10 for baselines, the ranker and ablations.

Protocol (time split, no peeking):
  * Embeddings: fit on post text only (no labels).
  * Ranker: trained on impressions from days 4-24. Every feature for an
    impression on day d uses only interactions from before day d.
  * Test: impressions from days 25-29. For each (user, day) with at least one
    like, rank that day's impressions and compute nDCG@10 with likes as
    relevance. Same candidates for every method, so only the ordering differs.

Only observable data is used: post text, timestamps, the interaction log.
The generator's hidden topic/quality/interest arrays are never read here.

Pure numpy (no scikit-learn): the build environment has no ML packages, and
the maths is small enough to show in full.
"""
import argparse
import json
import os
import re
from collections import defaultdict

import numpy as np
import pandas as pd

TOKEN = re.compile(r"[a-z0-9]+")
DIM = 64
TRAIN_FIRST_DAY, TRAIN_LAST_DAY, TEST_FIRST_DAY = 4, 24, 25
FEATURES = ["cosine", "log_likes", "freshness", "author_affinity", "has_history"]


def tokenize(text):
    return TOKEN.findall(text.lower())


# ---------------------------------------------------------------- embeddings
def fit_lsa(texts, min_df=5, dim=DIM):
    """TF-IDF (sublinear tf, smooth idf) + truncated SVD -> dense embeddings.

    The Go service (internal/embed) reproduces transform() exactly from the
    exported vocabulary, idf and components; a parity test checks this.
    """
    df = defaultdict(int)
    docs = [tokenize(t) for t in texts]
    for toks in docs:
        for w in set(toks):
            df[w] += 1
    vocab = sorted(w for w, c in df.items() if c >= min_df)
    index = {w: i for i, w in enumerate(vocab)}
    n = len(docs)
    idf = np.log((1 + n) / (1 + np.array([df[w] for w in vocab]))) + 1.0

    X = tfidf_matrix(docs, index, idf)
    # Truncated SVD via the eigendecomposition of X^T X (V x V, small here).
    # For a real vocabulary (50k+ terms) use randomized SVD on a sparse matrix.
    gram = X.T @ X
    vals, vecs = np.linalg.eigh(gram)
    order = np.argsort(vals)[::-1][:dim]
    components = vecs[:, order].T  # dim x V
    # Fix sign so the export is deterministic.
    signs = np.sign(components[np.arange(dim), np.abs(components).argmax(axis=1)])
    components *= signs[:, None]
    explained = vals[order].sum() / vals.sum()
    return {"vocab": vocab, "idf": idf.tolist(), "components": components.tolist(),
            "dim": dim, "explained_variance": float(explained)}


def tfidf_matrix(docs, index, idf):
    X = np.zeros((len(docs), len(index)), dtype=np.float32)
    for r, toks in enumerate(docs):
        counts = defaultdict(int)
        for w in toks:
            if w in index:
                counts[index[w]] += 1
        for c, k in counts.items():
            X[r, c] = (1 + np.log(k)) * idf[c]
        norm = np.linalg.norm(X[r])
        if norm > 0:
            X[r] /= norm
    return X


def embed(model, texts):
    index = {w: i for i, w in enumerate(model["vocab"])}
    X = tfidf_matrix([tokenize(t) for t in texts], index, np.array(model["idf"]))
    E = X @ np.array(model["components"], dtype=np.float32).T
    norms = np.linalg.norm(E, axis=1, keepdims=True)
    return E / np.where(norms == 0, 1, norms)


# ---------------------------------------------------------------- features
def build_examples(posts, inter, E):
    """One row per (user, post, day) impression with features from the past only."""
    post_idx = {pid: i for i, pid in enumerate(posts["id"])}
    created_day = posts["created_day"].to_numpy()
    author = posts["author_id"].to_numpy()

    views = inter[inter.kind == "view"].copy()
    likes = inter[inter.kind == "like"].copy()
    views["p"] = views.post_id.map(post_idx)
    likes["p"] = likes.post_id.map(post_idx)
    liked = set(zip(likes.user_id, likes.p, likes.day))

    likes_by_day = [likes[likes.day == d] for d in range(int(inter.day.max()) + 1)]

    n_users = int(inter.user_id.max()) + 1
    user_sum = np.zeros((n_users, E.shape[1]), dtype=np.float32)  # sum of liked embeddings
    user_cnt = np.zeros(n_users)
    post_likes = np.zeros(len(posts))
    post_views = np.zeros(len(posts))
    affinity = defaultdict(int)  # (user, author) -> likes before today
    views_by_day = [views[views.day == d] for d in range(int(inter.day.max()) + 1)]

    rows = []
    for d in range(int(inter.day.max()) + 1):
        if d >= TRAIN_FIRST_DAY:
            v = views[views.day == d]
            u = v.user_id.to_numpy()
            p = v.p.to_numpy()
            has = user_cnt[u] > 0
            uvec = user_sum[u] / np.maximum(user_cnt[u], 1)[:, None]
            un = np.linalg.norm(uvec, axis=1)
            cos = np.where(un > 0, (uvec * E[p]).sum(axis=1) / np.where(un > 0, un, 1), 0.0)
            age_h = (d + 0.5 - created_day[p]) * 24  # impression at mid-day on average
            aff = np.array([affinity[(uu, author[pp])] for uu, pp in zip(u, p)])
            label = np.array([(uu, pp, d) in liked for uu, pp in zip(u, p)], dtype=np.float32)
            rows.append(pd.DataFrame({
                "user": u, "post": p, "day": d, "label": label,
                "cosine": cos, "log_likes": np.log1p(post_likes[p]),
                "freshness": np.exp(-np.maximum(age_h, 0) / 24.0),
                "author_affinity": np.log1p(aff), "has_history": has.astype(np.float32),
                # Experiment only (not deployed): needs impression logging online.
                "like_rate": (post_likes[p] + 1) / (post_views[p] + 40),
                "created": created_day[p],
            }))
        # Update state with today's likes so tomorrow sees them.
        for uu, pp in zip(likes_by_day[d].user_id.to_numpy(), likes_by_day[d].p.to_numpy()):
            user_sum[uu] += E[pp]
            user_cnt[uu] += 1
            post_likes[pp] += 1
            affinity[(uu, author[pp])] += 1
        np.add.at(post_views, views_by_day[d].p.to_numpy(), 1)
    return pd.concat(rows, ignore_index=True)


# ---------------------------------------------------------------- model
def train_logreg(X, y, l2=1e-3, lr=0.5, epochs=300):
    """Batch gradient descent on standardised features, with balanced class
    weights (likes are ~2% of impressions)."""
    mean, std = X.mean(axis=0), X.std(axis=0) + 1e-9
    Z = (X - mean) / std
    pos = y.mean()
    sw = np.where(y == 1, 0.5 / pos, 0.5 / (1 - pos))
    w = np.zeros(Z.shape[1])
    b = 0.0
    for _ in range(epochs):
        p = 1 / (1 + np.exp(-(Z @ w + b)))
        g = (p - y) * sw
        w -= lr * (Z.T @ g / len(y) + l2 * w)
        b -= lr * g.mean()
    return {"mean": mean, "std": std, "w": w, "b": b}


def score(model, X):
    return ((X - model["mean"]) / model["std"]) @ model["w"] + model["b"]


# ---------------------------------------------------------------- eval
def ndcg_at_k(groups, scores, k=10):
    """Mean nDCG@k over groups. Ties broken by row order, which is randomised."""
    total, n = 0.0, 0
    disc = 1 / np.log2(np.arange(2, k + 2))
    for idx in groups:
        rel = LABELS[idx]
        if rel.sum() == 0:
            continue
        order = np.argsort(-scores[idx], kind="stable")[:k]
        dcg = (rel[order] * disc[: len(order)]).sum()
        ideal = np.sort(rel)[::-1][:k]
        idcg = (ideal * disc[: len(ideal)]).sum()
        total += dcg / idcg
        n += 1
    return total / n, n


LABELS = None


def main():
    global LABELS
    ap = argparse.ArgumentParser()
    ap.add_argument("--data", default="data/synth")
    ap.add_argument("--models", default="models")
    ap.add_argument("--report", default="docs/ranking-eval.md")
    ap.add_argument("--seed", type=int, default=11)
    a = ap.parse_args()
    rng = np.random.default_rng(a.seed)

    posts = pd.read_csv(f"{a.data}/posts.csv")
    inter = pd.read_csv(f"{a.data}/interactions.csv")
    t0 = pd.Timestamp("2026-10-01T00:00:00Z")
    posts["created_day"] = (pd.to_datetime(posts.created_at) - t0).dt.total_seconds() / 86400
    inter["day"] = ((pd.to_datetime(inter.created_at) - t0).dt.total_seconds() // 86400).astype(int)

    print("fitting LSA embeddings ...")
    lsa = fit_lsa(posts.message.tolist())
    E = embed(lsa, posts.message.tolist()).astype(np.float32)

    print("building features (past-only) ...")
    ex = build_examples(posts, inter, E)
    ex = ex.sample(frac=1.0, random_state=a.seed).reset_index(drop=True)  # random tie-break order
    train = ex[(ex.day >= TRAIN_FIRST_DAY) & (ex.day <= TRAIN_LAST_DAY)]
    test = ex[ex.day >= TEST_FIRST_DAY].reset_index(drop=True)

    print(f"train impressions={len(train)} likes={int(train.label.sum())}; "
          f"test impressions={len(test)} likes={int(test.label.sum())}")
    model = train_logreg(train[FEATURES].to_numpy(), train.label.to_numpy())

    LABELS = test.label.to_numpy()
    groups = list(test.groupby(["user", "day"]).indices.values())
    Xt = test[FEATURES].to_numpy()

    results = {}
    results["Random"] = ndcg_at_k(groups, rng.random(len(test)))
    results["Chronological (newest first)"] = ndcg_at_k(groups, test.created.to_numpy())
    results["Popularity (most liked)"] = ndcg_at_k(groups, test.log_likes.to_numpy() + 1e-6 * test.created.to_numpy())
    results["Embedding similarity only"] = ndcg_at_k(groups, test.cosine.to_numpy() + 1e-6 * test.created.to_numpy())
    results["Ranker (all features)"] = ndcg_at_k(groups, score(model, Xt))

    # Reference ceiling (analysis only, clearly not a deployable model): score
    # with the generator's true like-probability, read from the hidden arrays.
    interests = np.load(f"{a.data}/hidden_interests.npy")
    topics = np.load(f"{a.data}/hidden_post_topics.npy")
    quality = np.load(f"{a.data}/hidden_quality.npy")
    tu, tp = test.user.to_numpy() - 1, test.post.to_numpy()
    match = (topics[tp] * interests[tu]).sum(axis=1)
    age_days = test.day.to_numpy() + 0.5 - test.created.to_numpy()
    oracle = ndcg_at_k(groups, 5.0 * match + 2.5 * quality[tp] - 0.6 * age_days)[0]

    exp_feats = FEATURES + ["like_rate"]
    m_exp = train_logreg(train[exp_feats].to_numpy(), train.label.to_numpy())
    with_ctr = ndcg_at_k(groups, score(m_exp, test[exp_feats].to_numpy()))[0]

    ablations = {}
    for i, f in enumerate(FEATURES):
        keep = [j for j in range(len(FEATURES)) if j != i]
        m = train_logreg(train[FEATURES].to_numpy()[:, keep], train.label.to_numpy())
        ablations[f] = ndcg_at_k(groups, score(m, Xt[:, keep]))[0]

    # Cold-start slice: users with no likes before the test day.
    cold = [g for g in groups if test.has_history.to_numpy()[g[0]] == 0]
    cold_chrono = ndcg_at_k(cold, test.created.to_numpy())[0] if cold else float("nan")
    cold_ranker = ndcg_at_k(cold, score(model, Xt))[0] if cold else float("nan")

    # ---- export models
    os.makedirs(a.models, exist_ok=True)
    with open(f"{a.models}/lsa.json", "w") as f:
        json.dump({**lsa, "components": np.round(np.array(lsa["components"]), 6).tolist()}, f)
    with open(f"{a.models}/ranker.json", "w") as f:
        json.dump({"features": FEATURES, "mean": model["mean"].tolist(), "std": model["std"].tolist(),
                   "weights": model["w"].tolist(), "bias": float(model["b"])}, f, indent=1)
    # Parity fixture: Go must embed these texts to the same vectors.
    sample = posts.message.head(20).tolist() + ["Running coffee", "", "unknownword"]
    with open(f"{a.models}/parity.json", "w") as f:
        json.dump({"texts": sample, "embeddings": np.round(embed(
            {**lsa, "components": np.round(np.array(lsa["components"]), 6).tolist()}, sample), 6).tolist()}, f)

    # ---- report
    base = results["Chronological (newest first)"][0]
    ours = results["Ranker (all features)"][0]
    n_groups = results["Ranker (all features)"][1]
    lines = [
        "# Feed ranking: offline evaluation",
        "",
        "Generated by `tools/ranking/train_eval.py`. Data is **synthetic** (see",
        "[ranking.md](ranking.md) for why and for what that does and does not show).",
        "",
        f"- Posts: {len(posts):,}. Embedding: LSA, {DIM} dimensions, vocabulary {len(lsa['vocab'])} terms, "
        f"{lsa['explained_variance']:.0%} of TF-IDF variance kept.",
        f"- Train: days {TRAIN_FIRST_DAY}-{TRAIN_LAST_DAY}, {len(train):,} impressions, {int(train.label.sum()):,} likes.",
        f"- **Held-out test: days {TEST_FIRST_DAY}-29, {len(test):,} impressions, {int(test.label.sum()):,} likes**, "
        f"{n_groups:,} (user, day) ranking lists with at least one like.",
        "- Task: re-rank each user's daily impressions; relevance = liked. Metric: mean nDCG@10.",
        "",
        "| Method | nDCG@10 | vs. chronological |",
        "|---|---|---|",
    ]
    for name, (v, _) in results.items():
        lines.append(f"| {name} | {v:.4f} | {100 * (v / base - 1):+.1f}% |")
    gap = oracle - base
    lines += [
        "",
        f"**Reference ceiling** (analysis only): ranking by the generator's true like-probability "
        f"scores {oracle:.4f}. The ranker closes **{100 * (ours - base) / gap:.0f}%** of the gap between "
        "chronological and this ceiling. The ceiling reads hidden variables no real system has.",
        "",
        "## Experiment: impression logging",
        "",
        f"Adding a smoothed like-rate feature, likes / impressions, gives nDCG@10 **{with_ctr:.4f}** "
        f"({100 * (with_ctr / ours - 1):+.1f}% over the deployed ranker). It needs impressions to be "
        "logged online, which the API does not do yet, so it is not deployed. "
        + ("The gain is small on this data, so impression logging is not a priority for ranking quality "
           "alone (it is still the standard source of negative training examples)."
           if with_ctr / ours - 1 < 0.02 else
           "The gain is worth the engineering: impression logging is the next ranking task."),
        "",
        "## Ablation: drop one feature, retrain",
        "",
        "| Feature removed | nDCG@10 | change vs. full ranker |",
        "|---|---|---|",
    ]
    for f, v in ablations.items():
        lines.append(f"| `{f}` | {v:.4f} | {100 * (v / ours - 1):+.1f}% |")
    lines += [
        "",
        "## Cold-start users (no likes before the test day)",
        "",
        f"{len(cold):,} lists. Chronological {cold_chrono:.4f}, ranker {cold_ranker:.4f} "
        f"({100 * (cold_ranker / cold_chrono - 1):+.1f}%). Without history the ranker can only use "
        "freshness and engagement.",
        "",
        "## Learned weights (standardised features)",
        "",
        "| Feature | Weight |",
        "|---|---|",
    ] + [f"| `{f}` | {w:+.3f} |" for f, w in zip(FEATURES, model["w"])]
    os.makedirs(os.path.dirname(a.report) or ".", exist_ok=True)
    with open(a.report, "w") as f:
        f.write("\n".join(lines) + "\n")
    print("\n".join(lines))


if __name__ == "__main__":
    main()
