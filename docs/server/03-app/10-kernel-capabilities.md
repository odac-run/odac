## 🔐 Capabilities, Sysctls and UDP Ports

Some apps need more of the kernel than a plain container gets. A VPN is the clearest case: WireGuard has to create a network interface and program routes inside its own namespace, turn on IP forwarding, and answer on a **UDP** port. None of that is reachable with the container defaults, and none of it needs the blunt instrument of [Privileged Access](05-privileged-access.md).

Three fields cover it, all of them part of the app record and all of them applied when the container is created.

### Capabilities (`caps`)

A capability is one slice of root's power. `NET_ADMIN` lets an app configure interfaces and routing **inside its own network namespace**, which is what a tunnel needs and nothing more.

```json
{
  "type": "app",
  "app": "wireguard",
  "caps": ["NET_ADMIN"]
}
```

ODAC grants only these:

| Capability | What it is for |
| --- | --- |
| `NET_ADMIN` | Interfaces, routing and netfilter in the app's own namespace: WireGuard, OpenVPN, Tailscale |
| `NET_RAW` | Raw and packet sockets: `ping`, `traceroute` |
| `NET_BIND_SERVICE` | Bind a port below 1024 without running as root |
| `IPC_LOCK` | `mlock` memory so secrets never reach swap: Vault, Redis |
| `SYS_NICE` | Change the scheduling priority of the app's own threads |

Anything else is refused when the app is created. `SYS_ADMIN`, `SYS_MODULE`, `SYS_RAWIO`, `SYS_PTRACE` and their neighbours cross the container boundary: loading a kernel module or reading arbitrary host memory is root on the host, whatever the container believes it is. An app that genuinely needs that is asking for [Privileged Access](05-privileged-access.md), which is a deliberate, separate decision.

Names may be written with or without the `CAP_` prefix and in any case: `NET_ADMIN`, `net_admin` and `CAP_NET_ADMIN` are the same request.

### Sysctls (`sysctls`)

Kernel parameters, set per container:

```json
{
  "type": "app",
  "app": "wireguard",
  "caps": ["NET_ADMIN"],
  "sysctls": {
    "net.ipv4.ip_forward": "1",
    "net.ipv4.conf.all.src_valid_mark": "1"
  }
}
```

Only **namespaced** parameters are accepted, the ones the container engine can set for one container without touching the host: everything under `net.*`, plus `kernel.msg*`, `kernel.sem`, `kernel.shm*` and `fs.mqueue.*`. A host-wide knob like `vm.max_map_count` is refused — the engine would reject it too, and a rejected container is one ODAC keeps recreating rather than a visible error.

Values may be sent as strings, numbers or booleans; `1`, `"1"` and `true` all reach the kernel as `1`.

> ⚠️ **Host networking drops `net.*` sysctls.** A host-networked app shares the host's network namespace, so those settings would retune the host itself. Switching an app to host mode removes them and tells you which ones it removed; set them on the host instead. Capabilities and the non-network sysctls are unaffected. See [Network Mode](06-network-mode.md).

### UDP Ports (`proto`)

A port entry names its transport:

```json
{
  "ports": [
    { "container": 51820, "host": 51820, "proto": "udp", "public": true }
  ]
}
```

- `proto` is `"tcp"` or `"udp"`. Absent means `tcp`, which is what every port was before this field existed.
- The same number may be published on both transports: `51820/tcp` and `51820/udp` are two different bindings.
- A `proxy`-routed entry is always TCP. ODAC's reverse proxy speaks HTTP, so it has nothing to do with a datagram port and the combination is refused.
- `public: true` binds every interface, exactly as it does for TCP. **A published port bypasses the host firewall's INPUT chain** (the engine's DNAT rewrites the destination before it gets there), so a public UDP port is reachable from the internet whether or not your firewall has a matching UDP rule. If you keep firewall rules for published ports, they must name `udp` — a TCP rule does nothing for a tunnel.

The `app.port.set` API the dashboard's port editor uses carries `proto` through untouched, so a user's first config edit never silently downgrades a tunnel to TCP. A protocol it cannot read is refused rather than defaulted, for the same reason.

### Template Apps

In a multi-app template each field belongs to the container that needs it, under `apps.<key>`:

```json
{
  "type": "template",
  "name": "vpn-stack",
  "apps": {
    "tunnel": {
      "image": "linuxserver/wireguard",
      "caps": ["NET_ADMIN"],
      "sysctls": { "net.ipv4.ip_forward": "1" },
      "ports": [{ "container": 51820, "host": 51820, "proto": "udp", "public": true }]
    },
    "web": { "image": "nginx" }
  }
}
```

Only `tunnel` gets the capability. A request ODAC will not grant fails the whole stack before a single container is created, so a template never half-deploys on this.

### Notes

- All three fields are fixed when the container is created. **Restart** the app to apply a change: `odac app restart my-app`.
- An app that asks for none of them is created exactly as it always was, with no extra fields on its record.
- Every grant is written to the server log with the app's name, so there is a record of which containers hold extra kernel privilege.
