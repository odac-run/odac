# Page Cache

ODAC's built-in proxy includes an in-memory page cache that can dramatically reduce backend load and improve response times for high-traffic pages. Caching is **completely opt-in** — no page is ever cached unless your app explicitly requests it.

## How it works

When your app sends a response with the `X-Odac-Cache: <seconds>` header, ODAC caches that page's HTML in memory and serves subsequent requests directly from the proxy — without hitting your backend.

To protect against caching dynamic content (e.g. pages with CSRF tokens or per-user data), ODAC uses a **stability check**: a page is only promoted to cache after two consecutive requests return identical content. On the first request, ODAC fetches from your backend and stores a fingerprint. On the second request, if the content matches, the page is marked stable and served from cache from that point on.

## Enabling cache from your app

Send the `X-Odac-Cache` response header with the TTL in seconds:

```http
X-Odac-Cache: 3600
```

### Node.js (plain)

```js
res.setHeader('X-Odac-Cache', '3600')
res.send(html)
```

### ODAC.JS framework

```js
Odac.cache(3600)
```

## Cache behavior

| Behavior | Detail |
|---|---|
| **Opt-in only** | Pages without `X-Odac-Cache` are never cached |
| **Stability guard** | Only served from cache after two consecutive identical responses |
| **Stale-while-revalidate** | Cached response is served instantly; backend is re-checked asynchronously (at most once per second per entry) |
| **TTL** | Set by your app via `X-Odac-Cache: <seconds>`. Entry is hard-expired and removed after TTL |
| **Max body size** | 2 MB — larger responses are not cached |
| **Methods** | Only `GET` and `HEAD` requests |
| **Query strings** | Requests with query parameters (`?foo=bar`) are never served from cache |
| **Authorization** | Requests with an `Authorization` header bypass the cache |

## Automatic protection & refresh

ODAC's page cache is designed to be safe by default. Several mechanisms automatically prevent stale or per-user content from ever being served to the wrong visitor:

### Per-user / dynamic content is never cached

The stability guard is what protects pages that differ between visitors. If two consecutive responses for the same URL produce **different** content (for example a per-user greeting, a unique CSRF token, or any randomized markup), ODAC concludes the page is dynamic, discards the candidate entry, and **never promotes it to cache**. Such pages always hit your backend, even if they send `X-Odac-Cache`.

### Cookies do not disable caching — they are stripped

Setting a cookie (`Set-Cookie`) does **not** turn caching off. Session cookies are common on otherwise-static landing pages, so ODAC keeps caching the page but **removes the `Set-Cookie` header from the cached copy** — a cookie is never replayed to a different visitor. If a page genuinely must not be cached, omit `X-Odac-Cache` or send `Cache-Control: no-store` / `private`.

### Pages auto-refresh when they change

ODAC doesn't blindly wait for the full TTL to expire. While serving a cached page it runs a background conditional revalidation (throttled to once per second per entry). If the backend returns **changed content**, ODAC replaces the entry and resets its stability — so updated pages are picked up quickly without waiting out the remaining TTL. (The TTL still acts as a hard upper bound on staleness.)

### Caching can be withdrawn at runtime

If, on a later revalidation, your backend **stops sending `X-Odac-Cache`** (or switches to `Cache-Control: no-store` / `private`), ODAC automatically purges the cached entry. You can turn caching off for a page simply by no longer sending the header.

## Cache hit headers

When a response is served from cache, ODAC sets the following headers on the response:

```http
X-Odac-Cache: HIT
Age: <seconds since cached>
Server: ODAC
```

You can use `X-Odac-Cache: HIT` to confirm a page is being served from cache (e.g. in browser DevTools or `curl -I`).

## Vary support

If your app sends a `Vary` response header, ODAC uses it to store separate cache variants for the same URL. For example, you can cache different responses for AJAX vs. full-page requests:

```http
Vary: X-Requested-With
```

Only the following `Vary` fields are supported. Responses with any other `Vary` field are not cached:

- `Accept`
- `X-Odac`
- `X-Requested-With`

> `Accept-Encoding` is always ignored — ODAC caches the raw body and compresses on serve.

## What is and isn't cached

ODAC intentionally strips `Set-Cookie` from cached entries — cookies are never replayed from cache. Other safe headers are preserved: `Cache-Control`, `Content-Type`, `ETag`, `Last-Modified`, `Link`, `Vary`, `X-Robots-Tag`.

Responses are **not cached** if:
- Status code is not `200 OK`
- `Cache-Control` contains `no-store` or `private`
- Body is empty or exceeds 2 MB
- The proxy is running low on shared cache memory

## Automatic cache purge

ODAC automatically purges the page cache for a domain after every app **deploy** or **restart**. You don't need to do anything — stale cached pages are cleared before fresh traffic arrives.
