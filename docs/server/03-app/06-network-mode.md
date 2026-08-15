## 🌐 Network Mode

By default every ODAC application container joins the shared **`odac-network`** bridge. It gets its own IP, its own port namespace, and the ODAC proxy routes traffic to it by container IP. This is the right default for almost every app.

Some workloads need the **host** network namespace instead — service discovery protocols that broadcast on the LAN, VPN or tunnel clients, apps that open large dynamic port ranges, or anything that has to see the host's real interfaces. `odac app network` switches an app between the two.

> Looking to *deny* an app network access rather than change its topology? That is a separate setting — see [Network Isolation](07-network-isolation.md).

### The Two Modes

| Mode | Flag | What it does | When to use |
|------|------|--------------|-------------|
| **Bridge** | `--bridge` (default) | Joins `odac-network` with its own IP and port namespace. Published ports apply, the proxy routes by container IP, and the app can reach the internet and other apps. | Everything ordinary. This is the default. |
| **Host** | `--host` | Shares the host's network namespace. No IP of its own, no port isolation — it binds host ports directly. | mDNS/SSDP/DLNA discovery, VPN and tunnel clients, WebRTC or SIP with wide dynamic port ranges, apps that must observe the host's real interfaces. |

### Usage

```bash
# Share the host network namespace
odac app network my-app --host

# Back to the shared bridge network
odac app network my-app --bridge
odac app network my-app            # same thing — bridge is the default
```

You will be asked to confirm with `yes` before host mode is applied.

### Available Prefixes
- `-i`, `--id`: The App ID or Name
- `--host`: Share the host's network namespace
- `--bridge`: ODAC's shared bridge network (default)

> ⚠️ **Important:** A container's network mode is fixed when it is created, so the change takes effect on the next start. **Restart** the application afterwards:
> ```bash
> odac app restart my-app
> ```

---

## Host Mode

> ⚠️ **Host mode removes network isolation.** The container shares the host's network stack: it can reach every service listening on the host's loopback — including ODAC's own API on `127.0.0.1:1453` — and any port it binds is bound host-wide. Grant it only to apps you trust.

> 🚫 **Host networking and routed domains are mutually exclusive.** A domain means the app is serving live traffic, and host networking rules out zero-downtime deploys (see below). Rather than silently downgrading a live site to restart-with-downtime, ODAC refuses the combination **from both directions**: `app network --host` is rejected for an app that already has domains, and `domain add` is rejected for an app that is already host-networked. Pick one — remove the domains, or stay on the bridge. This applies to every interface; the CLI and the dashboard get the same answer.

**Published ports stop applying.** A host-mode app binds the host port itself, so ODAC drops any published mapping rather than sending it to Docker (which would discard it anyway). Configure the app to listen on the port you want directly.

**Zero-downtime deploys are not possible.** ODAC normally deploys with a Blue-Green switch: the new container starts and passes readiness checks before the old one is retired. In host mode the app's port is a host-wide singleton, so a second container cannot bind it — the green container would die immediately. This is the reason routed apps are refused host mode up front.

Both entry points into that state are guarded, so it is unreachable through normal use. Should an app still end up host-networked *and* routed — a hand-edited config, say — ODAC does not attempt a doomed Blue-Green deploy. It falls back to a stop-then-start recreate, accepting a **brief downtime window**, and logs the reason:

```
[App]  ZDD skipped for my-app: host networking makes the app's port a host-wide
       singleton, so a green container cannot bind it. Falling back to a
       recreate (brief downtime).
```

**The refusal is a deploy-safety policy, not a routing limitation.** ODAC itself runs in the host network namespace, so the proxy can reach a host-mode app on `127.0.0.1:<port>` perfectly well — routing is not the problem. Domains are refused because the app would lose zero-downtime deploys, not because traffic could not reach it.

---

### Examples

**Media server needing DLNA/mDNS discovery (no domain routed to it):**
```bash
odac app network my-app --host
odac app restart my-app
```

**Back to the shared network:**
```bash
odac app network my-app --bridge
odac app restart my-app
```

> 📝 **Note:** Prefer bridge mode whenever it works. Reach for host mode only when the app genuinely needs the host's network namespace — you give up port isolation, network isolation, and zero-downtime deploys to get it.
