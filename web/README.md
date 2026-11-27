# NetworkAI web

Next.js (App Router) + TypeScript + Tailwind CSS + TanStack Query.

```bash
npm ci
npm run dev                  # http://localhost:3000, expects the API on :8080 (make up)
npm run lint && npm run typecheck && npm run build
npm run test:e2e             # Playwright, against the real API stack
```

`NEXT_PUBLIC_API_URL` overrides the API base URL (default `http://localhost:8080`).

Structure:

- `src/lib/api.ts`: typed API client. It is the one place that attaches the
  JWT, parses the API's `{error: {code, message}, request_id}` shape, and
  logs the user out on a 401.
- `src/lib/auth.tsx`: auth context (token in localStorage; see the trade-off note in the file).
- `src/components/posts.tsx`: composer, post card (inline edit, optimistic
  delete) and the infinite-scroll list built on `useInfiniteQuery` with
  cursor pagination.
- `src/components/ui.tsx`: shadcn/ui-style primitives (cva + tailwind-merge).
- `e2e/`: Playwright tests that drive the real UI against the real API.
