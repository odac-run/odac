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

### Network Sysctls Are Dropped

An app carrying `net.*` [sysctls](10-kernel-capabilities.md) loses them when it moves to host mode. In the host namespace those settings would retune the host's own network stack, so the container engine refuses to start a container that carries them, and a container the engine refuses is one ODAC keeps recreating rather than a visible failure. The switch drops them and names what it dropped:

```
my-app now uses HOST networking (no network isolation from the host). Dropped
the network sysctls net.ipv4.ip_forward: they configure the host's own
namespace, so set them on the host instead. Restart required to apply.
```

Capabilities (`caps`) and the non-network sysctls are unaffected: they have nothing to do with which network namespace the container joins.

---

### Bridge Networks

A bridge app may sit on several Docker networks at once. Every new app starts on `odac-network` alone; the app's **Networks** list in the Hub adds or removes others, `odac-network` included.

- **The list is saved on the app.** It survives restarts and zero-downtime deploys: each new container is created on exactly these networks before it starts.
- **A running app changes immediately**, without a restart, and the proxy picks up its new address at once. A stopped app gets the list on its next start.
- **Removing `odac-network` is allowed.** ODAC runs in the host network namespace, so the proxy reaches an app on any bridge network, not only `odac-network`. Two routed apps that share no network cannot reach each other.
- **Removing every network shares none.** The app keeps its domains and its internet access, but no other app can reach it. See [Apps With No Shared Network](#apps-with-no-shared-network) below.
- `host`, `none` and `odac-network-private` cannot be added to a list. Use `odac app network --host` for the host namespace.
- Networks that do not exist yet are created as ordinary bridges.

Host-networked and isolated apps keep a fixed network and refuse a list. See [Network Isolation](07-network-isolation.md) for how to let another app reach an isolated one.

### Apps With No Shared Network

A container needs at least one network to have an address, so an app whose list is empty is placed on **`odac-network-private`**. ODAC creates this bridge with inter-container traffic switched off (`com.docker.network.bridge.enable_icc=false`):

| | `odac-network` | `odac-network-isolated` | `odac-network-private` |
|---|---|---|---|
| Reachable by the proxy | Yes | Yes | Yes |
| Internet access | Yes | No | Yes |
| Reachable by other apps on it | Yes | Yes | No |

Every app on `odac-network-private` is alone there: Docker drops traffic between any two containers on it, so putting several apps on it does not connect them. The proxy is unaffected because ODAC runs in the host network namespace, and host traffic is not forwarded between containers.

The placement is ODAC's, not part of the app's list. The Hub and `app list` show the list as empty, and the network cannot be named in a list. It does show up in `docker network ls` and `docker inspect`.

To let two such apps talk, add a network they share to both lists (for example `my-app-db`). They can reach each other on that network only, and stay unreachable to everything else.

> 📝 **Note:** ODAC creates `odac-network-private` on first use. If a network of that name already exists **with** inter-container traffic allowed, ODAC reuses it but logs an error, because Docker cannot change that option on an existing network. Delete it and let ODAC recreate it, or apps on it can reach each other.

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
