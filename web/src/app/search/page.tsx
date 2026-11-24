"use client";

import { Suspense, useEffect, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { api } from "@/lib/api";
import { PostList } from "@/components/posts";
import { Input } from "@/components/ui";

function SearchInner() {
  const params = useSearchParams();
  const router = useRouter();
  const q = params.get("q") ?? "";
  const type = params.get("type") ?? "";
  const [text, setText] = useState(q);

  // Debounce typing into the URL; the URL is the source of truth, so results
  // are shareable and survive a reload.
  useEffect(() => {
    const t = setTimeout(() => {
      if (text.trim() === q) return;
      const next = new URLSearchParams(params);
      if (text.trim()) next.set("q", text.trim());
      else next.delete("q");
      router.replace(`/search?${next}`);
    }, 300);
    return () => clearTimeout(t);
  }, [text, q, params, router]);

  const setType = (t: string) => {
    const next = new URLSearchParams(params);
    if (t) next.set("type", t);
    else next.delete("type");
    router.replace(`/search?${next}`);
  };

  return (
    <div className="space-y-4">
      <Input autoFocus aria-label="Search posts" placeholder="Search posts…" value={text} onChange={(e) => setText(e.target.value)} />
      <div className="flex gap-2 text-sm">
        {[["", "All"], ["image", "Images"], ["video", "Videos"], ["none", "Text"]].map(([v, label]) => (
          <button
            key={v}
            onClick={() => setType(v)}
            className={type === v ? "font-medium text-foreground" : "text-muted-foreground hover:text-foreground"}
          >
            {label}
          </button>
        ))}
      </div>
      <PostList
        enabled={q !== ""}
        // Search results are a different cache entry from the feed (key
        // starts with "posts" so edits/deletes still update them).
        queryKey={["posts", "search", { q, type }]}
        fetchPage={(cursor) => api.searchPosts({ q, type: type || undefined, cursor })}
        empty={`No posts match “${q}”.`}
      />
    </div>
  );
}

export default function SearchPage() {
  return (
    <Suspense>
      <SearchInner />
    </Suspense>
  );
}
