## 🎛️ GPU Reservation

AI workloads need the host's accelerator, not just its CPU. Ready-made AI apps installed from the Cloud arrive with their GPU reservation already attached, and `odac app gpu` is the same field for everything else: an app you created yourself, one that started on the CPU and outgrew it, or one whose reservation you want to resize or give back.

### Usage

```bash
# Reserve the GPU this host has (vendor auto-detected)
odac app gpu my-app

# Name the runtime explicitly
odac app gpu my-app --nvidia
odac app gpu my-app --amd
odac app gpu my-app --intel

# Reserve part of a multi-GPU host
odac app gpu my-app --nvidia --count 2

# Accelerate when the host can, run on the CPU when it cannot
odac app gpu my-app --optional
odac app gpu my-app --intel --optional

# Release the reservation, back to CPU
odac app gpu my-app --off
```

### Available Prefixes
- `-i`, `--id`: The App ID or Name
- `--nvidia`, `--amd`, `--intel`: The runtime to reserve. Omit them and ODAC uses the card it detected on this host
- `--count`: How many devices to reserve (default: all of them)
- `--optional`: Treat the reservation as a preference. The app installs and starts on a host with no GPU, on the CPU
- `--off`: Release the reservation

> ⚠️ **Important:** A container's device requests are fixed when it is created, so the change takes effect on the next start. **Restart** the application afterwards:
> ```bash
> odac app restart my-app
> ```

### Required or Optional

A reservation answers two questions, and they are separate on purpose:

| | Which accelerator | And if the host has none |
|---|---|---|
| flag | `--nvidia` / `--amd` / `--intel`, or nothing for auto | plain (required) or `--optional` |

The default is **required**: the app needs the card, so a host that cannot provide one is refused up front rather than crash-looping a CUDA image on the CPU.

`--optional` is for the much larger group of apps that use a GPU when there is one and work without it: video recorders and transcoders, thumbnail and media pipelines, anything with a CPU code path of its own. Such an app installs everywhere, and ODAC decides at every start:

- Host has a card ODAC can pass through → the container gets it
- Host has none, or the engine refuses at create time → the container starts on the CPU, with one line in the app log saying why

Combined with an omitted vendor flag, `--optional` also stays **unresolved** in the config. It is not a snapshot of today's hardware: put a card in the machine and `odac app restart my-app` picks it up, take it out and the app keeps running. A required reservation pins its runtime instead, so `odac app list` shows exactly what the app holds.

> ⚠️ **ODAC passes the device through, it does not configure the app.** An optional reservation puts `/dev/dri` (or the NVIDIA devices) inside the container; whether the app uses them is still up to the app's own settings, for example Frigate's `hwaccel_args` and detector configuration.

### The Host Pre-Flight

The reservation is checked against this host before it is stored, the same check `odac app create` runs. A host with an NVIDIA driver but no `nvidia-container-toolkit` has real hardware that no container can be handed, so the command refuses with the missing piece named instead of storing a reservation that would fail every start from then on:

```
This host cannot pass an NVIDIA GPU to a container: the NVIDIA container
runtime is not registered with Docker. Install nvidia-container-toolkit and
run `nvidia-ctk runtime configure --runtime=docker`, then restart Docker.
```

ROCm and Intel need no engine support, only the host's driver nodes (`/dev/kfd`, `/dev/dri`), and the refusal names those instead.

`--off` skips the pre-flight entirely. An app that was created with a reservation must always be able to go back to the CPU, including on a host that has since lost its card.

`--optional` skips it too, for the opposite reason: falling back to the CPU is the answer it already asked for, so there is nothing to refuse.

### What the Host Reports

`odac info` (system.info) carries the host's own answer:

- **runtime**: the kind of working card present, `null` when there is none
- **schedulable**: whether the container engine can actually hand it to a container
- **reason**: which piece is missing, `null` when schedulable

A bare `odac app gpu my-app` reserves whatever **runtime** reports. If ODAC cannot see the card from inside its own container the command says so, and naming the vendor explicitly (or setting `ODAC_GPU_RUNTIME` to `nvidia`, `rocm`, `intel` or `none`) is the way through.

Detection prefers the most capable card: NVIDIA, then AMD/ROCm, then an Intel GPU with a DRM render node. An iGPU is a modest compute device but a real media engine, so it is reported last rather than not at all. Apps that genuinely need CUDA pin `nvidia` in their recipe and never reach that fallback.

### What the App Reports

Once a reservation is optional, the config alone no longer answers "is this app using a GPU right now": the fallback can happen at ODAC's pre-flight or at the engine's refusal. So `odac app list` (app.list) reports both halves under the app's `gpu` member.

```json
"gpu": {
  "count": "all",
  "optional": true,
  "attached": { "vendor": "intel", "nodes": ["/dev/dri"] }
}
```

- The request members (`runtime`, `vendor`, `count`, `optional`) are exactly what was persisted, unchanged
- `attached` is present **only while a container is actually holding an accelerator**, so a plain truthiness check answers the question. It is absent for a stopped app, for one that fell back to the CPU, and for every app that never asked for a GPU

The attachment is read back from the container itself, not from the request, and comes from the inspect `odac app list` already performs per app, so reporting it costs no extra calls to the engine.

`nodes` are the device paths as passed. Intel and ROCm receive the `/dev/dri` directory, which the daemon expands to every `card*` and `renderD*` node on the host, so there is no single render node to name here; the host's own card inventory (vendor, model, VRAM) is in `odac info` instead. NVIDIA reports no nodes at all, because its container runtime injects the devices itself, and carries `count` instead.

> **`attached` is nested on purpose.** The `gpu` object is the shape the Cloud sent at create time and may be handed straight back to `app.gpu`. A resolved `vendor` written beside the request's own members would turn an auto reservation into a pinned one on that round-trip, freezing one day's hardware into the config.

### Notes

- Reserving a GPU is orthogonal to [Network Mode](06-network-mode.md) and [Network Isolation](07-network-isolation.md). An isolated app can still hold a GPU.
- The reservation is stored in the app's config and echoed back in `odac app list`, so the Cloud sees the same shape it sends at create time.
- ROCm and Intel containers also receive the host's render group ids, so an image that does not run as root can still open the device nodes.
