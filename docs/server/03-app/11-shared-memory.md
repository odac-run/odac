## 🧠 Shared Memory (`shmSize`)

A container gets a **64 MiB** `/dev/shm` by default. That is plenty for a web app and nowhere near enough for anything that hands raw buffers between its own processes through shared memory.

Frigate is the clearest case: its decoders write camera frames into `/dev/shm` and its detector reads them back out, so a handful of streams fills the default and the app dies mid-stream with an error that names no cause. Video pipelines, OpenCV workers, PostgreSQL with a large `shared_buffers` and Chrome-based renderers all land in the same place.

Size it on the create payload:

```json
{
  "type": "app",
  "app": "frigate",
  "shmSize": "512m"
}
```

### The Value

- Accepted as a **size string** (`"512m"`, `"1g"`, `"1.5GiB"`) or a **byte count** (`536870912`). Both spellings of a unit mean powers of 1024, the way container tooling has always read them: `1g` is 1 GiB.
- Stored on the app record as a canonical byte count, so the dashboard always reads the same shape back.
- **Minimum 1 MiB, maximum 64 GiB.** A bare `512` is 512 bytes and is refused rather than silently accepted, because it is almost always a missing unit.
- Omit it and nothing changes: the app record grows no field and the container is created exactly as it was before this existed.

Frigate's own rule of thumb is roughly `10 MiB` per camera on top of a `128 MiB` base, so `512m` covers a typical eight-camera install with room to spare.

### What It Costs

`/dev/shm` is a **tmpfs the host's RAM backs**. The size is a ceiling, not a reservation: nothing is allocated until the app writes, but everything it writes there is host memory. Size it for the workload rather than rounding up, and remember that an app which fills an oversized `/dev/shm` takes that memory from the host.

Every app given a bigger one is written to the server log with its name and size, so a host under memory pressure has a record of where the pages went.

### Template Apps

In a multi-app template the size belongs to the container that needs it, under `apps.<key>`:

```json
{
  "type": "template",
  "name": "nvr",
  "apps": {
    "detector": {
      "image": "blakeblackshear/frigate",
      "shmSize": "512m"
    },
    "ui": { "image": "nginx" }
  }
}
```

Only `detector` gets it. A size ODAC cannot read fails the whole stack before a single container is created, so a template never half-deploys on this.

### Notes

- The size is fixed when the container is created. **Restart** the app to apply a change: `odac app restart my-app`.
- A recipe may declare a size its image needs; an explicit request on the create payload wins, since only the operator knows how large the workload is.
- Unrelated to the `kernel.shm*` sysctls in [Capabilities & Sysctls](10-kernel-capabilities.md): those tune the System V shared memory limits, this sizes the POSIX `/dev/shm` mount.
