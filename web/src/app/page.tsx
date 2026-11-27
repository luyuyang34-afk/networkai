"use client";

import { useState } from "react";
import Link from "next/link";
import { api } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { Composer, PostList } from "@/components/posts";
import { Card } from "@/components/ui";
import { cn } from "@/lib/utils";

export default function FeedPage() {
  const { username, ready } = useAuth();
  const [tab, setTab] = useState<"latest" | "following" | "foryou">("latest");
  const showForYou = tab === "foryou" && !!username;
  return (
    <div className="space-y-4">
      {ready &&
        (username ? (
          <Composer />
        ) : (
          <Card className="text-sm text-muted-foreground">
            <Link href="/login" className="text-primary hover:underline">Log in</Link> to post and get recommendations.
          </Card>
        ))}
      {username && (
        <div role="tablist" className="flex gap-4 border-b border-border text-sm">
          {(["latest", "following", "foryou"] as const).map((t) => (
            <button
              key={t}
              role="tab"
              aria-selected={tab === t}
              onClick={() => setTab(t)}
              className={cn(
                "-mb-px border-b-2 pb-2",
                tab === t ? "border-primary font-medium" : "border-transparent text-muted-foreground",
              )}
            >
              {t === "latest" ? "Latest" : t === "following" ? "Following" : "For you"}
            </button>
          ))}
        </div>
      )}
      {tab === "following" && username ? (
        <PostList
          queryKey={["posts", "timeline"]}
          fetchPage={(cursor) => api.timeline(cursor)}
          empty="Follow people to see their posts here."
        />
      ) : showForYou ? (
        <PostList
          // One ranked page; the infinite list stops because there is no cursor.
          queryKey={["posts", "foryou"]}
          fetchPage={() => api.forYou()}
          empty="Like a few posts and recommendations will show up here."
        />
      ) : (
        <PostList
          queryKey={["posts", "feed"]}
          fetchPage={(cursor) => api.listPosts({ cursor })}
          empty="No posts yet. Be the first!"
        />
      )}
    </div>
  );
}
