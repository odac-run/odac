## 🔑 API Access

An application can drive ODAC itself (list apps, send mail, add a domain) by calling the same API the CLI uses. `odac app api` decides whether an app may do that, and which actions it may call. Nothing is granted by default: an app with no grant has neither a key nor a socket to reach.

### Scope: This Server Only

> [!IMPORTANT]
> This API is **local**. It talks to the ODAC server running on this machine and nothing else. It never connects to ODAC Cloud, and it carries no notion of your other servers. An app you grant access to can manage *this* server's apps, domains and mailboxes; it **cannot** reach or manage any other server registered in ODAC Cloud.

The key follows from that: `ODAC_API_KEY` is minted by your local ODAC server, signed with a secret it generated for itself on first start, and injected into the container automatically. There is **no ODAC Cloud account, login, or connection involved**. This works exactly the same on a machine that has never talked to the Cloud, and on one that is fully offline.

### How an App Reaches the API

When an app has a grant, ODAC injects two things at container start:

| Variable | Value | What it is |
|----------|-------|------------|
| `ODAC_API_SOCKET` | `/odac/api.sock` | The API's unix socket. ODAC's run directory is bind-mounted **read-only** at `/odac`. |
| `ODAC_API_KEY` | signed token | Identifies the calling app on every request. |

The socket is a local file, not a network service, so this works for apps that are [isolated](07-network-isolation.md) with no outbound access at all, and for apps on the shared bridge or in [host network mode](06-network-mode.md) alike.

A request is one JSON object written to the socket:

```json
{ "auth": "<ODAC_API_KEY>", "action": "app.list", "data": [] }
```

Action names are the ones in the [CLI Reference](../02-get-started/03-cli-reference.md) with the space replaced by a dot: `odac app list` is `app.list`, `odac mail send` is `mail.send`.

### Available Actions

These are the actions available to applications. An app may call the ones you granted it, and `--all` covers the whole table.

| Action | `data` | What it does |
|--------|--------|--------------|
| `app.list` | `[]` or `[true]` for detail | List apps |
| `app.create` | `[config]` | Create an app (same object the CLI builds) |
| `app.delete` | `[app]`, or `[app, {"purge": false}]` to keep its data | Delete an app |
| `app.start` | `[app]` | Start a stopped app |
| `app.stop` | `[app]` | Stop a running app |
| `app.restart` | `[app]` | Restart an app |
| `app.network` | `[app, "bridge"\|"host"]` | Set the network mode |
| `app.isolate` | `[app, true\|false]` | Cut off or restore outbound access |
| `app.device.add` | `[app, hostPath, containerPath]` | Connect a host device |
| `app.device.delete` | `[app, hostPath]` | Disconnect a host device |
| `domain.list` | `[]`, or `[app]` to filter | List domains |
| `domain.add` | `[domain, app]` | Route a domain to an app |
| `domain.delete` | `[domain]` | Remove a domain |
| `dns.list` | `[domain]` | List a domain's DNS records |
| `ssl.renew` | `[domain]` | Force an SSL certificate renewal |
| `mail.send` | `[message]` | Send mail from one of your domains |
| `mail.list` | `[domain]` | List mailboxes |
| `mail.create` | `[email, password, passwordAgain]` | Create a mailbox |
| `mail.password` | `[email, password, passwordAgain]` | Change a mailbox password |
| `mail.delete` | `[email]` | Delete a mailbox |

`app` is an App ID or name, exactly like the CLI's `-i` argument.

`mail.send` is the one action whose argument is a message object rather than plain strings:

```json
{
  "auth": "<ODAC_API_KEY>",
  "action": "mail.send",
  "data": [{
    "from": "no-reply@example.com",
    "to": "someone@example.com",
    "subject": "Welcome",
    "header": { "Content-Type": "text/html; charset=utf-8" },
    "html": "<p>Hello</p>",
    "text": "Hello"
  }]
}
```

`from` must belong to a domain configured on this server. `header` is required; supply at least a `Content-Type`, and provide `html`, `text`, or both.

### Usage

```bash
# Grant exactly what the app needs
odac app api my-app --allow app.list,mail.send

# Grant every action, present and future (asks you to confirm)
odac app api my-app --all

# Revoke
odac app api my-app --off

# Interactive: prompts for the action list
odac app api my-app
```

### Available Prefixes
- `-i`, `--id`: The App ID or Name
- `--allow`: Comma-separated action names to permit (`*` means all)
- `--all`: Permit every action, same as `--allow "*"`
- `--off`: Revoke API access

Action names are validated against the server's registered actions when you grant them, so a typo is rejected at the prompt instead of turning into a `permission_denied` on every call the app makes.

### Grant and Revoke Are Not Symmetric

> ⚠️ **Granting takes a restart.** The key and the socket mount are handed to the container when it starts, so a new grant reaches the app on its next start:
> ```bash
> odac app restart my-app
> ```

**Revoking does not.** Permissions are read from the configuration on every request, so `--off` (or a narrowed `--allow`) refuses the app's very next call. The running container keeps its now-worthless key until you restart it. If you are revoking because an app misbehaved, the access is gone the moment the command returns.

### Choosing Permissions

Grant the narrowest list that works. `--all` still means an app can create and delete other apps, and add or remove domains and mailboxes, from inside a container with no further check. An app that needs to send mail wants `--allow mail.send`. Treat a full grant the way you would treat [privileged access](05-privileged-access.md).
