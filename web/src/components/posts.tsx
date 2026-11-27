"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import {
  type InfiniteData,
  type QueryKey,
  useInfiniteQuery,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";
import { Heart, ImagePlus, Loader2, Pencil, Trash2, X } from "lucide-react";
import { ApiError, api, type Page, type Post } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { timeAgo } from "@/lib/utils";
import { Alert, Button, Card, Textarea } from "./ui";

const MAX_CHARS = 5000;

type PostPages = InfiniteData<Page<Post>, string | undefined>;

/** Apply fn to every cached copy of a post (feed, profile, search pages). */
function updateCachedPost(qc: ReturnType<typeof useQueryClient>, id: string, fn: (p: Post) => Post | null) {
  qc.setQueriesData<PostPages>({ queryKey: ["posts"] }, (data) =>
    data
      ? {
          ...data,
          pages: data.pages.map((pg) => ({
            ...pg,
            items: pg.items.flatMap((p) => (p.id === id ? (fn(p) ?? []) : [p])),
          })),
        }
      : data,
  );
}

export function errorText(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  return "Something went wrong.";
}

// ---------- composer ----------

export function Composer() {
  const qc = useQueryClient();
  const [message, setMessage] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);

  const create = useMutation({
    mutationFn: () => api.createPost(message.trim(), file),
    onSuccess: (post) => {
      setMessage("");
      setFile(null);
      if (fileRef.current) fileRef.current.value = "";
      // Show the new post immediately, but only in the feeds it belongs to:
      // the global feed and the author's own profile. (A prefix match on
      // ["posts","feed"] would also insert it into other users' profiles.)
      // Then refetch those feeds in the background.
      const prepend = (data: PostPages | undefined) =>
        data
          ? { ...data, pages: [{ ...data.pages[0], items: [post, ...data.pages[0].items] }, ...data.pages.slice(1)] }
          : data;
      qc.setQueryData<PostPages>(["posts", "feed"], prepend);
      qc.setQueryData<PostPages>(["posts", "feed", { author: post.author }], prepend);
      qc.invalidateQueries({ queryKey: ["posts", "feed"] });
    },
  });

  // Writing assistant: the suggestion is shown next to the draft and only
  // replaces it when the user accepts, so a bad or failed suggestion never
  // costs them their text.
  const [suggestion, setSuggestion] = useState<string | null>(null);
  const assist = useMutation({
    mutationFn: (mode: "polish" | "shorten" | "hashtags") => api.rewrite(mode, message),
    onSuccess: (r) => setSuggestion(r.suggestion),
  });

  const tooLong = message.length > MAX_CHARS;
  const empty = message.trim() === "" && !file;
  return (
    <Card>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!empty && !tooLong) create.mutate();
        }}
        className="space-y-3"
      >
        <Textarea
          aria-label="New post"
          placeholder="What's on your mind?"
          value={message}
          onChange={(e) => setMessage(e.target.value)}
        />
        {file && (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <span className="truncate">{file.name}</span>
            <Button type="button" variant="ghost" size="icon" aria-label="Remove file" onClick={() => setFile(null)}>
              <X className="h-4 w-4" />
            </Button>
          </div>
        )}
        {suggestion !== null && (
          <div data-testid="suggestion" className="space-y-2 rounded-md border border-border bg-muted p-3 text-sm">
            <p className="whitespace-pre-wrap">{suggestion}</p>
            <div className="flex justify-end gap-2">
              <Button type="button" variant="outline" size="sm" onClick={() => setSuggestion(null)}>Discard</Button>
              <Button type="button" size="sm" onClick={() => { setMessage(suggestion); setSuggestion(null); }}>Use this</Button>
            </div>
          </div>
        )}
        {assist.isError && <Alert>{errorText(assist.error)}</Alert>}
        {create.isError && <Alert>{errorText(create.error)}</Alert>}
        <div className="flex items-center gap-2">
          <input
            ref={fileRef}
            type="file"
            accept="image/*,video/*"
            className="hidden"
            onChange={(e) => setFile(e.target.files?.[0] ?? null)}
          />
          <Button type="button" variant="ghost" size="icon" aria-label="Attach media" onClick={() => fileRef.current?.click()}>
            <ImagePlus className="h-4 w-4" />
          </Button>
          <select
            aria-label="Improve with AI"
            className="h-8 rounded-md border border-border bg-card px-2 text-xs text-muted-foreground"
            value=""
            disabled={message.trim() === "" || assist.isPending}
            onChange={(e) => e.target.value && assist.mutate(e.target.value as "polish" | "shorten" | "hashtags")}
          >
            <option value="">{assist.isPending ? "Thinking…" : "✨ Improve"}</option>
            <option value="polish">Polish</option>
            <option value="shorten">Shorten</option>
            <option value="hashtags">Add hashtags</option>
          </select>
          <span className={tooLong ? "text-xs text-destructive" : "text-xs text-muted-foreground"}>
            {message.length}/{MAX_CHARS}
          </span>
          <Button type="submit" className="ml-auto" disabled={empty || tooLong || create.isPending}>
            {create.isPending && <Loader2 className="h-4 w-4 animate-spin" />}
            Post
          </Button>
        </div>
      </form>
    </Card>
  );
}

// ---------- single post ----------

export function PostCard({ post }: { post: Post }) {
  const { username } = useAuth();
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(post.message);
  const mine = username === post.author;

  const update = useMutation({
    mutationFn: () => api.updatePost(post.id, draft.trim()),
    onSuccess: (updated) => {
      updateCachedPost(qc, post.id, () => updated);
      setEditing(false);
    },
  });

  // Optimistic like: flip immediately everywhere the post is cached; on
  // failure, refetch to restore the server's truth.
  const like = useMutation({
    mutationFn: (next: boolean) => (next ? api.like(post.id) : api.unlike(post.id)),
    onMutate: (next) =>
      updateCachedPost(qc, post.id, (p) => ({ ...p, liked: next, like_count: p.like_count + (next ? 1 : -1) })),
    onError: () => qc.invalidateQueries({ queryKey: ["posts"] }),
  });

  // Optimistic delete: hide immediately, restore by refetching on failure.
  const remove = useMutation({
    mutationFn: () => api.deletePost(post.id),
    onMutate: () => updateCachedPost(qc, post.id, () => null),
    onError: () => qc.invalidateQueries({ queryKey: ["posts"] }),
  });

  return (
    <Card data-testid="post" className="space-y-2">
      <div className="flex items-center gap-2 text-sm">
        <Link href={`/u/${post.author}`} className="font-medium hover:underline">
          @{post.author}
        </Link>
        <span className="text-muted-foreground" title={new Date(post.created_at).toLocaleString()}>
          · {timeAgo(post.created_at)}
          {post.updated_at !== post.created_at && " · edited"}
        </span>
        {mine && !editing && (
          <div className="ml-auto flex">
            <Button variant="ghost" size="icon" aria-label="Edit post" onClick={() => setEditing(true)}>
              <Pencil className="h-4 w-4" />
            </Button>
            <Button
              variant="destructive"
              size="icon"
              aria-label="Delete post"
              onClick={() => confirm("Delete this post?") && remove.mutate()}
            >
              <Trash2 className="h-4 w-4" />
            </Button>
          </div>
        )}
      </div>

      {editing ? (
        <div className="space-y-2">
          <Textarea aria-label="Edit post" value={draft} onChange={(e) => setDraft(e.target.value)} />
          {update.isError && <Alert>{errorText(update.error)}</Alert>}
          <div className="flex justify-end gap-2">
            <Button variant="outline" size="sm" onClick={() => { setEditing(false); setDraft(post.message); }}>
              Cancel
            </Button>
            <Button size="sm" disabled={update.isPending || draft.length > MAX_CHARS} onClick={() => update.mutate()}>
              Save
            </Button>
          </div>
        </div>
      ) : (
        post.message && <p className="whitespace-pre-wrap break-words">{post.message}</p>
      )}

      {post.media_url && post.media_type === "image" && (
        // eslint-disable-next-line @next/next/no-img-element -- user media from the API host
        <img src={post.media_url} alt="" loading="lazy" className="max-h-96 rounded-md border border-border" />
      )}
      {post.media_url && post.media_type === "video" && (
        <video src={post.media_url} controls preload="metadata" className="max-h-96 rounded-md" />
      )}

      <div className="flex items-center gap-1 text-sm text-muted-foreground">
        <Button
          variant="ghost"
          size="icon"
          aria-label={post.liked ? "Unlike" : "Like"}
          aria-pressed={post.liked}
          disabled={!username || like.isPending}
          onClick={() => like.mutate(!post.liked)}
        >
          <Heart className={post.liked ? "h-4 w-4 fill-red-500 text-red-500" : "h-4 w-4"} />
        </Button>
        <span data-testid="like-count">{post.like_count}</span>
      </div>
    </Card>
  );
}

// ---------- infinite list ----------

export function PostList({
  queryKey,
  fetchPage,
  empty,
  enabled = true,
}: {
  queryKey: QueryKey;
  fetchPage: (cursor?: string) => Promise<Page<Post>>;
  empty: React.ReactNode;
  enabled?: boolean;
}) {
  const q = useInfiniteQuery({
    queryKey,
    queryFn: ({ pageParam }) => fetchPage(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor || undefined,
    enabled,
  });

  // Load the next page when the sentinel scrolls into view.
  const sentinel = useRef<HTMLDivElement>(null);
  const { hasNextPage, isFetchingNextPage, fetchNextPage } = q;
  useEffect(() => {
    const el = sentinel.current;
    if (!el || !hasNextPage) return;
    const io = new IntersectionObserver((entries) => {
      if (entries[0].isIntersecting && !isFetchingNextPage) fetchNextPage();
    }, { rootMargin: "400px" });
    io.observe(el);
    return () => io.disconnect();
  }, [hasNextPage, isFetchingNextPage, fetchNextPage]);

  if (!enabled) return null;
  if (q.isPending) return <p className="text-sm text-muted-foreground">Loading…</p>;
  if (q.isError) {
    const code = q.error instanceof ApiError ? q.error.code : "";
    return (
      <Alert>
        {code === "search_unavailable"
          ? "Search is temporarily unavailable. The feed and posting still work."
          : code === "feed_unavailable" || code === "feed_busy"
            ? "Recommendations are temporarily unavailable. The Latest tab still works."
            : errorText(q.error)}
      </Alert>
    );
  }
  const posts = q.data.pages.flatMap((p) => p.items);
  if (posts.length === 0) return <p className="text-sm text-muted-foreground">{empty}</p>;
  return (
    <div className="space-y-3">
      {posts.map((p) => (
        <PostCard key={p.id} post={p} />
      ))}
      <div ref={sentinel} />
      {isFetchingNextPage && <p className="text-center text-sm text-muted-foreground">Loading more…</p>}
    </div>
  );
}
