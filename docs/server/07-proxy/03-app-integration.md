# Building an ODAC-Compatible App

Any app ODAC runs gets traffic from the ODAC proxy without changes: listen on `PORT` and you are done. This page is for developers who want the rest: a faster unix socket path from the proxy, and a way to know for certain that a request really came through the proxy, so the visitor's IP address and HTTPS state can be trusted.

The ODAC framework does all of this automatically. Everything below is what it does, written down so an app in any language can do the same.

### Environment

ODAC sets these variables when a container starts:

| Variable | When | Meaning |
|----------|------|---------|
| `PORT` | Always | The TCP port to listen on. Always open it, even when you also serve the socket. |
| `ODAC_APP` | Always | `true`. Tells the app it runs under an ODAC server. |
| `ODAC_PROXY_KEY` | Always | Secret the proxy signs the connection headers with. Treat it like a password. |
| `ODAC_PROXY_SOCKET` | Bridge network apps on Linux servers | Path of a unix socket the app should serve besides `PORT`, for example `/odac-proxy/http.sock`. |
| `ODAC_CLIENT_CERT_KEY` | Apps with a [client certificate](../06-domain/04-client-certificates.md) mode | Same value as `ODAC_PROXY_KEY`, kept under the name the client certificate headers were first documented with. |

The variables reach a container only when it starts. After an ODAC update, an app that was already running has no `ODAC_PROXY_KEY` until its next restart or deploy.

### Serving the Unix Socket

When `ODAC_PROXY_SOCKET` is set, ODAC has mounted an empty directory for your container and will send requests to a socket at that path once one exists. Over the socket, requests skip the TCP stack between the proxy and your app, and nothing but the proxy can connect to it.

1. **Serve plain HTTP/1.1 on it**, the same app you serve on `PORT`. WebSockets work the same way.
2. **Open it before the TCP port.** The proxy switches to the socket only if it exists when it picks up your container: after a deploy's health check, or within a minute of a plain start. An app that opens the socket later stays on TCP until the next deploy.
3. **Keep `PORT` open.** ODAC's health check and port detection use TCP, and the proxy falls back to TCP on any request it cannot send over the socket.
4. **Clear a leftover file at startup.** A restarted container keeps the previous run's socket file, and listening fails while it is there. Remove it if it is a socket nothing answers on. If it is a symlink, remove the link itself and never follow it. Leave any other kind of file alone and serve TCP only.
5. **Never fail the app over the socket.** If it cannot be created, log the reason and keep serving TCP.

Any file mode works: the proxy connects as root. The proxy also refuses to follow a symlink at the path or to connect to anything that is not a socket, and uses TCP instead.

Node.js:

```js
const http = require('http')
const fs = require('fs')

const app = (req, res) => res.end('hello')
const socketPath = process.env.ODAC_PROXY_SOCKET

if (socketPath) {
  try {
    const stat = fs.lstatSync(socketPath)
    if (stat.isSocket() || stat.isSymbolicLink()) fs.unlinkSync(socketPath)
  } catch {}
  http.createServer(app).listen(socketPath).on('error', err => console.error('proxy socket:', err.message))
}
http.createServer(app).listen(process.env.PORT)
```

Go:

```go
handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) })

if path := os.Getenv("ODAC_PROXY_SOCKET"); path != "" {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&(fs.ModeSocket|fs.ModeSymlink) != 0 {
		os.Remove(path)
	}
	if ln, err := net.Listen("unix", path); err == nil {
		go http.Serve(ln, handler)
	} else {
		log.Printf("proxy socket: %v", err)
	}
}
log.Fatal(http.ListenAndServe(":"+os.Getenv("PORT"), handler))
```

Python, with gunicorn binding both:

```sh
gunicorn app:app --bind "0.0.0.0:$PORT" ${ODAC_PROXY_SOCKET:+--bind "unix:$ODAC_PROXY_SOCKET"}
```

Use gunicorn's `uvicorn.workers.UvicornWorker` for an ASGI app: plain `uvicorn` binds either a port or a socket, not both.

These examples remove a leftover socket without checking whether something answers on it. That is enough when one process owns the socket. If several processes in your container could serve it, connect first and remove the file only when the connection is refused.

### Headers From the Proxy

The proxy sets these on every request it forwards, and removes any copy the client sent:

| Header | Value |
|--------|-------|
| `X-Odac-Connection-RemoteAddress` | The visitor's IP address |
| `X-Odac-Connection-Ssl` | `true` when the visitor used HTTPS, absent otherwise |
| `X-Odac-Connection-Signature` | `t=<unix seconds>,v1=<hex>`, see below |
| `X-Forwarded-For`, `X-Real-IP` | The visitor's IP address, the same value |
| `X-Forwarded-Proto` | `https` or `http` |
| `X-Forwarded-Host` | The `Host` the visitor sent |
| `X-Odac-Websocket` | `true` on WebSocket upgrade requests |
| `X-Request-ID` | A random ID, or the one the client sent |

Clients' copies of `X-Odac-Client-*`, `Client-IP`, `X-Client-IP`, `X-Remote-IP`, `X-Remote-Addr`, `X-Originating-IP` and `Proxy` are removed as well. Hosts with a client certificate mode get the [client certificate headers](../06-domain/04-client-certificates.md#verifying-in-your-app) on top.

### Trusting the Headers

The proxy is not the only thing that can reach your app. Over TCP, so can a published port and another container on the same network, and either of them can send these headers with made-up values. Decide per request:

- **Arrived on the socket from `ODAC_PROXY_SOCKET`:** it is the proxy. Trust the headers.
- **Arrived over TCP with a valid signature:** it is the proxy. Trust the headers.
- **Anything else:** treat it as a direct connection. Use the socket's own peer address, and treat the request as plain HTTP unless your own listener did TLS.

Read the client address and HTTPS state from `X-Odac-Connection-RemoteAddress` and `X-Odac-Connection-Ssl`, since those are what the signature covers. The `X-Forwarded-*` headers carry the same values but are not signed. If your framework has a "trust proxy" setting, turn it on only for requests that pass the rules above.

Without `ODAC_PROXY_KEY` (an ODAC server older than signing, or a container started before the update) there is no signature to check. Trust private network peers then, as earlier releases did, or nothing at all if a published port or a neighbour could reach the app.

### Verifying the Signature

`X-Odac-Connection-Signature` is `t=<unix seconds>,v1=<hex>`. To verify it:

1. Build this string from the request as your app received it, one field per line, joined by a single newline (`\n`) with no trailing newline. An absent header is an empty string.

   ```
   odac-connection-v1
   <t>
   <method>
   <Host header>
   <request target>
   <X-Odac-Connection-RemoteAddress>
   <X-Odac-Connection-Ssl>
   ```

   The request target is the path and query exactly as they appeared on the request line, for example `/search?q=a%20b`. Do not decode or normalize it.

2. Compute HMAC-SHA256 of that string with `ODAC_PROXY_KEY` as the key, and hex-encode it in lowercase.
3. Compare it with `v1` in constant time.
4. Reject a `t` more than 60 seconds away from your clock. The proxy and your app share the host's clock.

Test vector: with the key `k`, a `GET` to `example.com` for `/a?b=1` from `203.0.113.7` over HTTPS at `t=1700000000` is signed `t=1700000000,v1=d9a16b4e0be33e772aac465b062640fb9fa3163a9e7bf997c7b1323579b979c8`.

Node.js (`req` is an `http.IncomingMessage`):

```js
const crypto = require('crypto')

function fromOdacProxy(req) {
  const key = process.env.ODAC_PROXY_KEY
  const m = /^t=(\d+),v1=([0-9a-f]{64})$/.exec(req.headers['x-odac-connection-signature'] || '')
  if (!key || !m || Math.abs(Date.now() / 1000 - Number(m[1])) > 60) return false
  const signed = [
    'odac-connection-v1',
    m[1],
    req.method,
    req.headers.host || '',
    req.url,
    req.headers['x-odac-connection-remoteaddress'] || '',
    req.headers['x-odac-connection-ssl'] || ''
  ].join('\n')
  const mac = crypto.createHmac('sha256', key).update(signed).digest()
  return crypto.timingSafeEqual(mac, Buffer.from(m[2], 'hex'))
}
```

Go:

```go
func fromOdacProxy(r *http.Request) bool {
	key := os.Getenv("ODAC_PROXY_KEY")
	rest, ok := strings.CutPrefix(r.Header.Get("X-Odac-Connection-Signature"), "t=")
	ts, sig, found := strings.Cut(rest, ",v1=")
	t, err := strconv.ParseInt(ts, 10, 64)
	if key == "" || !ok || !found || err != nil || time.Since(time.Unix(t, 0)).Abs() > time.Minute {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(strings.Join([]string{
		"odac-connection-v1", ts, r.Method, r.Host, r.RequestURI,
		r.Header.Get("X-Odac-Connection-RemoteAddress"), r.Header.Get("X-Odac-Connection-Ssl"),
	}, "\n")))
	return hmac.Equal(mac.Sum(nil), got)
}
```

Python (`target` is the raw request target: gunicorn's `environ["RAW_URI"]`, or for ASGI `scope["raw_path"]` plus `?` and `scope["query_string"]` when there is one; `headers` has lowercase names):

```python
import hashlib, hmac, os, re, time

SIGNATURE = re.compile(r"t=(\d+),v1=([0-9a-f]{64})")

def from_odac_proxy(method, host, target, headers):
    key = os.environ.get("ODAC_PROXY_KEY", "")
    m = SIGNATURE.fullmatch(headers.get("x-odac-connection-signature", ""))
    if not key or not m or abs(time.time() - int(m.group(1))) > 60:
        return False
    signed = "\n".join([
        "odac-connection-v1", m.group(1), method, host, target,
        headers.get("x-odac-connection-remoteaddress", ""),
        headers.get("x-odac-connection-ssl", ""),
    ])
    mac = hmac.new(key.encode(), signed.encode(), hashlib.sha256).hexdigest()
    return hmac.compare_digest(mac, m.group(2))
```

PHP:

```php
function from_odac_proxy(): bool {
    $key = getenv('ODAC_PROXY_KEY') ?: '';
    $sig = $_SERVER['HTTP_X_ODAC_CONNECTION_SIGNATURE'] ?? '';
    if ($key === '' || !preg_match('/^t=(\d+),v1=([0-9a-f]{64})\z/', $sig, $m) || abs(time() - (int) $m[1]) > 60) {
        return false;
    }
    $signed = implode("\n", [
        'odac-connection-v1', $m[1], $_SERVER['REQUEST_METHOD'], $_SERVER['HTTP_HOST'] ?? '', $_SERVER['REQUEST_URI'],
        $_SERVER['HTTP_X_ODAC_CONNECTION_REMOTEADDRESS'] ?? '', $_SERVER['HTTP_X_ODAC_CONNECTION_SSL'] ?? '',
    ]);
    return hash_equals(hash_hmac('sha256', $signed, $key), $m[2]);
}
```

PHP-FPM clears the environment by default. Pass the key through with `env[ODAC_PROXY_KEY] = $ODAC_PROXY_KEY` in the pool config, or set `clear_env = no`.

### Why ODAC's Own Headers

The values follow the common `X-Forwarded-*` conventions, and ODAC sends those too. The signature does not use the standard `Signature` header of RFC 9421 (HTTP Message Signatures) on purpose: apps such as ActivityPub servers receive `Signature` headers from other servers and verify them end to end, and a proxy that replaced or extended that header would break them. Connection-level alternatives like the PROXY protocol describe one TCP connection, while the proxy reuses connections across visitors.

### For Operators

- **Turning the socket off.** Set `ODAC_BACKEND_SOCKETS=off` in the ODAC server's environment. Apps then get no `ODAC_PROXY_SOCKET` and the proxy uses TCP for everything. Signing is not affected.
- **Where it applies.** Sockets are used on Linux servers for apps on a bridge network. Apps in [host network mode](../03-app/06-network-mode.md) and ODAC tunnels always use TCP.
- **Tunnels are signed too.** ODAC Cloud reports the visitor's address and TLS state with each tunnel request. The ODAC server that receives the tunnel takes those two values from the Cloud connection it opened over verified TLS and signs them with the app's key, so an app handles tunnel requests with the same code as any other.
- **After updating ODAC,** restart or redeploy apps to give them `ODAC_PROXY_KEY` and the socket.
