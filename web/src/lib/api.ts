// Typed client for the Go API. Every request goes through `request`, which is
// the single place that attaches the token, parses the API's error shape and
// reacts to an expired session.

export const API_URL = process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";

export type Post = {
  id: string;
  author: string;
  message: string;
  media_type: "none" | "image" | "video";
  media_url?: string;
  created_at: string;
  updated_at: string;
  like_count: number;
  liked: boolean;
};

export type Page<T> = { items: T[]; next_cursor?: string };

/** Mirrors the server's {"error": {"code", "message"}, "request_id"} body. */
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public requestId?: string,
  ) {
    super(message);
  }
}

const TOKEN_KEY = "networkai.token";

export const tokenStore = {
  get(): string | null {
    try {
      return localStorage.getItem(TOKEN_KEY);
    } catch {
      return null;
    }
  },
  set(t: string) {
    try {
      localStorage.setItem(TOKEN_KEY, t);
    } catch {}
  },
  clear() {
    try {
      localStorage.removeItem(TOKEN_KEY);
    } catch {}
  },
};

// Set by AuthProvider so an expired token anywhere logs the user out once.
let onUnauthorized: (() => void) | null = null;
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}

async function request<T>(path: string, init: RequestInit & { auth?: boolean } = {}): Promise<T> {
  const headers = new Headers(init.headers);
  const token = tokenStore.get();
  if (init.auth && token) headers.set("Authorization", `Bearer ${token}`);
  if (init.body && !(init.body instanceof FormData)) headers.set("Content-Type", "application/json");

  let res: Response;
  try {
    res = await fetch(`${API_URL}${path}`, { ...init, headers });
  } catch {
    throw new ApiError(0, "network", "Can't reach the server. Check your connection.");
  }
  if (res.status === 204) return undefined as T;

  const body = await res.json().catch(() => null);
  if (!res.ok) {
    const err = new ApiError(
      res.status,
      body?.error?.code ?? "unknown",
      body?.error?.message ?? res.statusText,
      body?.request_id,
    );
    if (res.status === 401 && init.auth) onUnauthorized?.();
    throw err;
  }
  return body as T;
}

export const api = {
  signup: (username: string, password: string) =>
    request<{ id: number; username: string }>("/users", {
      method: "POST",
      body: JSON.stringify({ username, password }),
    }),
  login: (username: string, password: string) =>
    request<{ token: string; expires_at: string }>("/auth/login", {
      method: "POST",
      body: JSON.stringify({ username, password }),
    }),

  // Read endpoints send the token when present so the server can fill `liked`.
  listPosts: (p: { author?: string; cursor?: string; limit?: number }) => {
    const q = new URLSearchParams({ limit: String(p.limit ?? 20) });
    if (p.author) q.set("author", p.author);
    if (p.cursor) q.set("cursor", p.cursor);
    return request<Page<Post>>(`/posts?${q}`, { auth: true });
  },
  searchPosts: (p: { q: string; type?: string; cursor?: string }) => {
    const q = new URLSearchParams({ q: p.q, limit: "20" });
    if (p.type) q.set("type", p.type);
    if (p.cursor) q.set("cursor", p.cursor);
    return request<Page<Post>>(`/posts/search?${q}`, { auth: true });
  },
  rewrite: (mode: "polish" | "shorten" | "hashtags", draft: string) =>
    request<{ suggestion: string; model: string; latency_ms: number }>("/assist/rewrite", {
      method: "POST",
      body: JSON.stringify({ mode, draft }),
      auth: true,
    }),
  forYou: () => request<Page<Post>>(`/feed?limit=30`, { auth: true }),
  like: (id: string) => request<void>(`/posts/${id}/like`, { method: "POST", auth: true }),
  unlike: (id: string) => request<void>(`/posts/${id}/like`, { method: "DELETE", auth: true }),
  createPost: (message: string, file?: File | null) => {
    const fd = new FormData();
    fd.set("message", message);
    if (file) fd.set("media_file", file);
    return request<Post>("/posts", { method: "POST", body: fd, auth: true });
  },
  updatePost: (id: string, message: string) =>
    request<Post>(`/posts/${id}`, { method: "PATCH", body: JSON.stringify({ message }), auth: true }),
  deletePost: (id: string) => request<void>(`/posts/${id}`, { method: "DELETE", auth: true }),
};

/** Decode the username from a JWT without verifying it (display only; the server verifies). */
export function usernameFromToken(token: string | null): string | null {
  if (!token) return null;
  try {
    const payload = JSON.parse(atob(token.split(".")[1].replace(/-/g, "+").replace(/_/g, "/")));
    if (typeof payload.exp === "number" && payload.exp * 1000 < Date.now()) return null;
    return typeof payload.username === "string" ? payload.username : null;
  } catch {
    return null;
  }
}
