# Asset Cache

ODAC's proxy includes a **zero-config, adaptive, in-memory cache for static assets** (CSS, JS, fonts, images). Unlike the [Page Cache](01-page-cache.md), it requires **no opt-in and no headers** — it just works. The cache automatically adapts to the machine it runs on: a 64 GB server gets generous caching, a 512 MB VPS gets conservative caching, and if memory runs low the cache disables itself entirely.

## How it works

When a request comes in for a static asset, ODAC serves it from memory if it's cached. Otherwise it proxies to your app, and — **only if the asset is requested often enough** — stores the response for future requests. Popular assets stay hot in memory; rarely-requested files are never admitted, so a one-off request can't pollute the cache.

Like the page cache, it uses **stale-while-revalidate**: a cached asset is served instantly while ODAC checks the backend asynchronously (throttled to once per second per asset) to see if it changed.

## What gets cached

Caching is limited to static asset file extensions:

```
.avif  .bmp  .css  .eot  .gif  .ico  .jpeg  .jpg  .js
.map   .mjs  .otf  .png  .svg  .ttf  .webp  .woff  .woff2
```

A response is cached only when **all** of the following hold:

| Condition | Detail |
|---|---|
| **Method** | `GET` or `HEAD` only |
| **No query string** | Requests with `?...` are skipped (often carry tokens / cache-busters) |
| **No `Authorization`** | Authenticated requests bypass the cache |
| **Status** | `200 OK` only |
| **Content-Type** | Must match a static asset type (validated against the extension) |
| **No `Set-Cookie`** | Per-user / session responses are never cached |
| **`Cache-Control`** | Must not contain `no-store` or `private` |
| **`Vary`** | Must not contain `*`, `Cookie`, or `Authorization` |
| **Size** | Maximum 5 MB per asset |

## Admission control — only popular assets are cached

ODAC tracks the request frequency of every asset URL, **even before it's cached**. An asset is admitted only when its score clears a threshold:

```
score = request_frequency (req/sec) × asset_priority
```

Asset priority reflects how much each file type affects perceived page load:

| Priority | Asset types | Why |
|---|---|---|
| **3.0** | `.css`, `.js`, `.mjs` | Render-blocking — the browser can't paint without them |
| **2.5** | `.woff`, `.woff2`, `.ttf`, `.eot`, `.otf` | Fonts cause layout shift (FOUT/FOIT) if slow |
| **2.0** | `.svg`, `.ico` | Visible but non-blocking |
| **1.0** | `.webp`, `.avif`, `.png`, `.jpg`, `.jpeg`, `.gif`, `.bmp` | Images — browser can render progressively |
| **0.3** | `.map` | Source maps — developer tooling, not user-facing |

Under normal conditions an asset is admitted at roughly **1 request per 5 seconds** (a frequency of `0.2`). Because priority is a multiplier, a render-blocking CSS file requested once a second easily beats a frequently-fetched source map. This is also how eviction is ordered: the lowest-scoring entries are dropped first.

## Memory-aware: it adapts to your server

ODAC continuously receives the host's memory state and recalculates its limits in near-real-time, so the cache scales with actual free RAM:

- **Max cache size** = up to **15% of *available* (free) RAM** (minimum 16 MB while enabled).
  - A 64 GB machine with 50 GB free → up to ~7.5 GB cache.
  - A 512 MB VPS with 200 MB free → up to ~30 MB cache.
- **Memory pressure** — when free RAM drops below **30%**, ODAC caches only *hot* assets (high score) and aggressively evicts low-value entries.
- **Memory critical** — when free RAM drops below **20%**, the cache **disables itself entirely and purges everything**, automatically re-enabling once memory recovers.

You never configure any of this — it's fully automatic.

## Eviction

A background task runs every 10 seconds and removes:

- **Cold entries** — any asset not requested within the last **2 minutes** (each hit resets this timer).
- **Low-value entries under pressure** — when memory is tight, anything below the hot-asset threshold.
- **Lowest-score entries first** — when the cache needs room for a new asset, it evicts by `frequency × priority`, dropping the least valuable assets.

## Cache hit headers

When an asset is served from cache, ODAC sets:

```http
X-Odac-Cache: HIT
Age: <seconds since cached>
Server: ODAC
```

Conditional requests are supported — if your `If-None-Match` (ETag) or `If-Modified-Since` matches the cached entry, ODAC returns `304 Not Modified` without sending the body.

## Auto-refresh & purge

- **Changed assets are picked up automatically.** During background revalidation, if the backend returns updated content (a `200` instead of `304`), ODAC replaces the cached copy. You don't need to fingerprint or version filenames for the cache to stay correct.
- **Deploys and restarts purge the cache** for the affected domain automatically, so fresh assets are served immediately after you ship.

## Relationship to the Page Cache

| | Asset Cache | [Page Cache](01-page-cache.md) |
|---|---|---|
| **Opt-in?** | No — fully automatic | Yes — via `X-Odac-Cache` header |
| **Caches** | Static files (CSS, JS, fonts, images) | HTML page responses |
| **Admission** | Request frequency + priority | App-specified TTL + stability check |
| **Shared memory budget** | Yes — both draw from the same adaptive memory pool | Yes |
