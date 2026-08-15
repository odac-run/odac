## 🔒 Network Isolation

Some apps have no business reaching the internet — untrusted or third-party code you do not want phoning home, a data-processing worker that only consumes what you hand it, or anything under a compliance rule that forbids egress. `odac app isolate` cuts an app off from every network outside this host.

This is **not** a network mode. An isolated app is still an ordinary bridge app — same container IP, same port namespace, same proxy routing — it simply joins a bridge with no route off the machine. Isolation and [Network Mode](06-network-mode.md) are independent settings; the one exception is host mode, which cannot be isolated at all (see below).

### Usage

```bash
# Cut off all outbound network access
odac app isolate my-app

# Give it back
odac app isolate my-app --off
```

### Available Prefixes
- `-i`, `--id`: The App ID or Name
- `--off`: Restore outbound network access

> ⚠️ **Important:** A container's network is fixed when it is created, so the change takes effect on the next start. **Restart** the application afterwards:
> ```bash
> odac app restart my-app
> ```

### How It Is Enforced

Isolated apps join **`odac-network-isolated`**, a bridge created with Docker's **`internal`** flag. That flag installs firewall rules dropping every packet routed between the bridge's subnet and any other interface.

It is a real boundary, not a routing trick — and because the network itself is the boundary, it applies from the container's very first packet. There is no window during startup in which traffic could still escape, and no firewall state for ODAC to keep in sync as containers come and go.

> 📝 **Note:** ODAC creates `odac-network-isolated` on first use. If a network of that name already exists **without** the internal flag, ODAC reuses it but logs an error — Docker cannot retrofit the flag onto an existing network. Delete it and let ODAC recreate it, or the isolation is not real.

### What Still Works

The app keeps an ordinary container IP. ODAC runs in the host network namespace, and host-originated traffic is not *forwarded*, so it never meets those drop rules. That means:

- **Domains and SSL work normally.** The proxy reaches the app exactly as before.
- **Zero-downtime Blue-Green deploys keep working.** Unlike host mode, isolation costs you nothing at deploy time.
- **Image builds and pulls are unaffected.** Both run outside the app's container.

### What Stops Working

- **All outbound access.** No package installs at runtime, no outbound API calls, no license or update checks, no SMTP. If the app needs any of these to function, it cannot be isolated.
- **Published ports become host-local.** Inbound traffic from another machine is forwarded, so it dies on the same rule. `curl` from this host still works; a client elsewhere on the network cannot reach a published port. Use a domain instead — proxy routing is unaffected.
- **Apps on the shared bridge.** An isolated app sits on a different network, so it cannot reach apps on `odac-network`.

### Letting Another App Reach an Isolated One

Do not attach the isolated app to a second network — that would hand it a route to the internet again and quietly void the guarantee, so ODAC refuses it. Attach the **other** app instead; a container may sit on several networks at once:

```bash
# The database keeps its internet access on the shared bridge and also joins
# the isolated network, where the isolated app can reach it.
odac app network set my-db odac-network odac-network-isolated
```

### Isolation and Host Mode

Host networking shares the host's entire network stack, so there is no bridge of its own to cut off — the two settings are incompatible. ODAC refuses the combination from both directions: `app isolate` is rejected for a host-networked app, and `app network --host` is rejected for an isolated one. Turn the other setting off first.

### Examples

**Untrusted third-party app, still served on a domain:**
```bash
odac app isolate my-app
odac app restart my-app
odac domain add -d app.example.com -a my-app   # works: the proxy still reaches it
```

**A worker that must reach the database but not the internet:**
```bash
odac app isolate my-worker
odac app network set my-db odac-network odac-network-isolated
odac app restart my-worker
```

> 📝 **Note:** Check first that the app does not need outbound access to start — many images fetch something on first boot. If it hangs or crashes after isolation, that is usually why.
