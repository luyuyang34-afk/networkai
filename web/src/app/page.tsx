"use client";

import Link from "next/link";
import { api } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { Composer, PostList } from "@/components/posts";
import { Card } from "@/components/ui";

export default function FeedPage() {
  const { username, ready } = useAuth();
  return (
    <div className="space-y-4">
      {ready &&
        (username ? (
          <Composer />
        ) : (
          <Card className="text-sm text-muted-foreground">
            <Link href="/login" className="text-primary hover:underline">Log in</Link> to post.
          </Card>
        ))}
      <PostList
        queryKey={["posts", "feed"]}
        fetchPage={(cursor) => api.listPosts({ cursor })}
        empty="No posts yet. Be the first!"
      />
    </div>
  );
}
