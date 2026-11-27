"use client";

import { use } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { PostList } from "@/components/posts";
import { Button } from "@/components/ui";

function FollowBar({ username }: { username: string }) {
  const { username: me } = useAuth();
  const qc = useQueryClient();
  const profile = useQuery({ queryKey: ["profile", username, me], queryFn: () => api.profile(username) });
  const toggle = useMutation({
    mutationFn: (follow: boolean) => (follow ? api.follow(username) : api.unfollow(username)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["profile", username] });
      // The server rebuilds the follower's timeline; refetch ours.
      qc.invalidateQueries({ queryKey: ["posts", "timeline"] });
    },
  });
  const p = profile.data;
  return (
    <div className="flex items-center gap-4 text-sm text-muted-foreground">
      <span data-testid="followers">{p ? p.follower_count : "–"} followers</span>
      <span>{p ? p.following_count : "–"} following</span>
      {me && p && me !== username && (
        <Button
          size="sm"
          variant={p.followed_by_me ? "outline" : "default"}
          disabled={toggle.isPending}
          onClick={() => toggle.mutate(!p.followed_by_me)}
        >
          {p.followed_by_me ? "Unfollow" : "Follow"}
        </Button>
      )}
    </div>
  );
}

export default function ProfilePage({ params }: { params: Promise<{ username: string }> }) {
  const { username } = use(params);
  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">@{username}</h1>
      <FollowBar username={username} />
      <PostList
        queryKey={["posts", "feed", { author: username }]}
        fetchPage={(cursor) => api.listPosts({ author: username, cursor })}
        empty="No posts yet."
      />
    </div>
  );
}
