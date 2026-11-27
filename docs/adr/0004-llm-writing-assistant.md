# ADR 0004: Treat the LLM as an unreliable, metered dependency

- Status: accepted
- Date: 2026-11-23

## Context

The post editor offers "Improve with AI": polish, shorten, or add hashtags to
a draft. The model is called over the network (OpenAI-compatible Chat
Completions API). Compared with every other dependency, this one is:

- **slow**: seconds, not milliseconds;
- **unreliable**: rate limits (429), overload (5xx), timeouts;
- **metered**: every call costs money, proportional to tokens;
- **an injection surface**: user text goes into a prompt.

## Decision

`internal/assist`:

1. **Isolation.** Only `POST /assist/rewrite` touches the LLM. Posting, feeds
   and search never wait on it.
2. **Hard timeout** of 8 s per request, and **one retry** only for 429/5xx,
   honouring `Retry-After`. 4xx responses (bad key, bad request) fail fast.
3. **Graceful degradation.** Every failure returns
   `503 assist_unavailable`. The editor keeps the draft and shows the
   suggestion beside it, never replacing the draft until the user accepts.
4. **Cost controls.**
   - a per-user token bucket (20/min by default) applied *before* calling
     the provider;
   - input capped at 2000 characters;
   - output capped with `max_completion_tokens` per mode (150–400).

   Invalid and rate-limited requests never reach the provider (tested).
5. **Accounting.** Each response reports token usage, latency, and cost
   computed from configured per-token prices (`LLM_PRICE_IN_PER_M`,
   `LLM_PRICE_OUT_PER_M`). Prices are configuration, not code, because they
   change. Prometheus exports requests by outcome, tokens by direction,
   total cost and latency. Average cost per request is
   `rate(networkai_llm_cost_usd_total[1h]) / rate(networkai_llm_requests_total{outcome="ok"}[1h])`.
6. **Prompt-injection mitigation.**
   - The draft is wrapped in `<draft>…</draft>`.
   - The system prompt says to treat it as text, never as instructions.
   - A `</draft>` typed inside the draft is neutralised so it cannot close
     the block early.

   This reduces the risk but does not eliminate it. The output is only ever
   shown to the same user as a suggestion, so a successful injection can
   only affect that user's own draft.
7. **Provider-agnostic.** Behind a `Provider` interface with a configurable
   base URL, any OpenAI-compatible endpoint works. Tests use a local fake
   server.

## Consequences

- The feature is off unless `OPENAI_API_KEY` is set. The editor hides
  nothing: requests get a clear 503 and the UI says so.
- The rate limit is per API replica (in-memory). A global limit would need
  shared state such as Redis.
- **Not yet measured:** real latency, real token counts and real cost per
  request. The build environment could not reach the provider (see
  docs/bottlenecks.md). The instrumentation is in place, so the numbers come
  from `/metrics` as soon as a key is configured.
