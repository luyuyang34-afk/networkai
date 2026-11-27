#!/usr/bin/env python3
"""Synthetic social-network generator for load tests and offline ranking eval.

Why synthetic: no public interaction dataset was reachable from the build
environment (see docs/ranking.md). The generator is deliberately *structured*
so that ranking is a real learning problem rather than noise:

  * 20 topics, each with its own vocabulary. A post is about 1-2 topics and
    its text is mostly drawn from those topics' words.
  * Each post has a hidden quality score. Engaging posts get more likes from
    everyone.
  * Each user has a sparse hidden interest distribution over topics.
  * Users see impressions over 30 days. Like probability depends on
    interest x topic match, quality, and recency (people engage with fresh
    posts).

Rankers only ever see the observable data: post text, timestamps, and the
interaction log. They never see the hidden quality or interest vectors.
Evaluation uses a time split: train on days 0-24, test on days 25-29.

Output: CSV files with a header row, written to --out.
"""
import argparse
import csv
import os
import uuid

import numpy as np

TOPICS = {
    "running":     "run running marathon pace trail sneakers miles race training sprint jog stride tempo intervals finish",
    "coffee":      "coffee espresso latte beans roast barista brew cafe pourover crema arabica cappuccino grinder mug",
    "travel":      "travel flight passport hotel beach itinerary backpack airport island journey tour visa hostel explore",
    "cooking":     "recipe cooking bake oven garlic pasta dough sauce kitchen roast spice simmer skillet dinner flavor",
    "music":       "music guitar concert album song vinyl band playlist melody chords drums lyrics festival jazz piano",
    "photography": "photo camera lens aperture shutter portrait landscape exposure film editing tripod golden light frame",
    "books":       "book novel reading author chapter library fiction memoir poetry paperback bookstore plot series classic",
    "gaming":      "game gaming console controller level boss multiplayer quest speedrun strategy rpg arcade patch indie",
    "fitness":     "gym workout squat deadlift protein reps strength cardio stretch yoga core muscle routine bench",
    "tech":        "code software startup laptop programming database cloud api deploy server bug release kubernetes golang",
    "ai":          "model neural training dataset transformer embedding inference prompt llm agent gpu benchmark finetune",
    "hiking":      "hiking trail summit mountain forest camping backpacking ridge waterfall elevation boots wilderness",
    "cycling":     "bike cycling ride saddle helmet gravel climb pedal peloton commute wheels derailleur",
    "film":        "movie film director cinema scene trailer actor screenplay festival documentary sequel cinematography",
    "gardening":   "garden plants seeds soil tomatoes compost bloom harvest succulents pruning greenhouse herbs",
    "pets":        "dog cat puppy kitten leash vet treats adoption walk paws grooming shelter",
    "finance":     "budget savings invest stocks index fund retirement interest mortgage portfolio dividend",
    "fashion":     "outfit style jacket vintage thrift sneakers denim wardrobe tailoring streetwear",
    "science":     "physics chemistry experiment lab telescope research hypothesis molecule biology quantum",
    "food":        "ramen tacos sushi pizza burger brunch dumplings noodles bakery restaurant street food dessert",
}
FILLER = ("today really just finally new great first best love little week weekend morning "
          "night trying think amazing favorite another quick notes my our this that with about").split()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--users", type=int, default=5000)
    ap.add_argument("--posts", type=int, default=100_000)
    ap.add_argument("--days", type=int, default=30)
    ap.add_argument("--impressions-per-user-day", type=int, default=20)
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--out", default="data/synth")
    a = ap.parse_args()

    rng = np.random.default_rng(a.seed)
    names = list(TOPICS)
    vocab = [TOPICS[t].split() for t in names]
    T = len(names)
    os.makedirs(a.out, exist_ok=True)

    # ---- users: sparse interests (Dirichlet with small alpha)
    interests = rng.dirichlet(np.full(T, 0.15), size=a.users)
    # Creators lean to 1-2 topics too.
    with open(f"{a.out}/users.csv", "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["id", "username"])
        for i in range(a.users):
            w.writerow([i + 1, f"user_{i + 1:05d}"])
    np.save(f"{a.out}/hidden_interests.npy", interests)  # for analysis only; never used by rankers

    # ---- posts
    base = np.datetime64("2026-10-01T00:00:00")
    created_sec = np.sort(rng.integers(0, a.days * 86400, size=a.posts))
    authors = rng.integers(1, a.users + 1, size=a.posts)
    quality = rng.beta(2, 5, size=a.posts)  # most posts mediocre, a few great
    post_topics = np.zeros((a.posts, T))
    post_ids = [str(uuid.UUID(bytes=rng.bytes(16), version=4)) for _ in range(a.posts)]
    with open(f"{a.out}/posts.csv", "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["id", "author_id", "message", "created_at"])
        for i in range(a.posts):
            # topic drawn from the author's interests: creators post what they like
            t1 = rng.choice(T, p=interests[authors[i] - 1])
            ts = [t1] if rng.random() < 0.7 else [t1, int(rng.integers(T))]
            for t in ts:
                post_topics[i, t] += 1.0 / len(ts)
            n = int(rng.integers(8, 26))
            words = []
            for _ in range(n):
                if rng.random() < 0.35:
                    words.append(FILLER[rng.integers(len(FILLER))])
                else:
                    tv = vocab[ts[rng.integers(len(ts))]]
                    words.append(tv[rng.integers(len(tv))])
            msg = " ".join(words).capitalize()
            ts_str = str(base + np.timedelta64(int(created_sec[i]), "s")) + "Z"
            w.writerow([post_ids[i], authors[i], msg, ts_str])
    np.save(f"{a.out}/hidden_post_topics.npy", post_topics)
    np.save(f"{a.out}/hidden_quality.npy", quality)

    # ---- interactions: impressions sampled from recent posts, likes from a
    # logistic model of interest match, quality and freshness.
    created_day = created_sec / 86400.0
    n_like = n_view = 0
    with open(f"{a.out}/interactions.csv", "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["user_id", "post_id", "kind", "created_at"])
        for day in range(1, a.days):
            # candidate pool: posts from the last 3 days
            lo = np.searchsorted(created_day, day - 3)
            hi = np.searchsorted(created_day, day)
            if hi - lo < 10:
                continue
            active = rng.random(a.users) < 0.6
            for u in np.nonzero(active)[0]:
                imp = rng.integers(lo, hi, size=a.impressions_per_user_day)
                match = post_topics[imp] @ interests[u]  # 0..1
                age = day - created_day[imp]  # 0..3 days
                logit = -4.2 + 5.0 * match + 2.5 * quality[imp] - 0.6 * age
                p = 1 / (1 + np.exp(-logit))
                liked = rng.random(len(imp)) < p
                sec = day * 86400 + rng.integers(0, 86400, size=len(imp))
                for j, pi in enumerate(imp):
                    ts_str = str(base + np.timedelta64(int(sec[j]), "s")) + "Z"
                    w.writerow([u + 1, post_ids[pi], "view", ts_str])
                    n_view += 1
                    if liked[j]:
                        w.writerow([u + 1, post_ids[pi], "like", ts_str])
                        n_like += 1
    print(f"users={a.users} posts={a.posts} views={n_view} likes={n_like} "
          f"like_rate={n_like / max(n_view, 1):.3f} -> {a.out}")


if __name__ == "__main__":
    main()
