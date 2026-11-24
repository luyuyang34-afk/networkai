"use client";

import { use } from "react";
import { api } from "@/lib/api";
import { PostList } from "@/components/posts";

export default function ProfilePage({ params }: { params: Promise<{ username: string }> }) {
  const { username } = use(params);
  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">@{username}</h1>
      <PostList
        queryKey={["posts", "feed", { author: username }]}
        fetchPage={(cursor) => api.listPosts({ author: username, cursor })}
        empty="No posts yet."
      />
    </div>
  );
}
