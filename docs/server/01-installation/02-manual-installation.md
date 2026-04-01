## 🛠️ Manual Installation

If you prefer to understand the underlying steps or need to customize your setup, here is how the ODAC installation script works. The process varies between environments (Linux servers vs. desktop development) to ensure stability, performance, and proper access to system resources.

### 1. Dependency Management

The installer first verifies that **Docker** is available on the system.

- **Linux:** If Docker is missing, the script installs it. On **Rocky Linux** or **AlmaLinux**, it uses `dnf`. On other distributions, it uses the official Docker installation script.
- **macOS:** If missing, it installs **Docker Desktop** via **Homebrew** (`brew install --cask docker`) and initializes the daemon.
- **Windows:** The script checks for the `docker` command. If not found, a manual installation of **Docker Desktop** is required from the [official website](https://docs.docker.com/desktop/install/windows-install/).

### 2. Image Acquisition

Across all platforms, the latest ODAC image is pulled from the **GitHub Container Registry (GHCR)** and tagged for consistency:

```bash
docker pull ghcr.io/odac-run/odac:latest
docker tag ghcr.io/odac-run/odac:latest odacrun/odac:latest
```

### 3. Container Configuration

Configuration varies by platform to account for file system structures and networking limitations in Docker Desktop:

#### Linux (Server) Setup
- **Storage:** Host root is set to `/var/odac`.
- **Network:** Uses `--network host` for maximum performance and direct access to system ports.
- **Privileges:** Includes a read-only mount of `/lib/modules` for kernel interaction.

#### macOS (Local) Setup
- **Storage:** Host root is set to `$HOME/Odac`.
- **Network:** Maps ports **80**, **443**, and **53** to localhost, as host networking is not natively supported.

#### Windows (Local) Setup
- **Storage:** Host root is set to `%USERPROFILE%/Odac`. The installer automatically creates necessary subdirectories: `data/apps`, `data/storage`, and `data/sites`.
- **Network:** Like macOS, it maps ports **80**, **443**, and **53** to localhost.
- **Connectivity:** Mounts the standard Windows pipe `//./pipe/docker_engine` (or `/var/run/docker.sock` depending on WSL2 settings) to manage local containers.

### 4. CLI Wrapper Setup

To provide a seamless experience, the installer creates a native CLI wrapper so `odac` can be called directly from your terminal:

- **Linux & macOS:** A bash script is created at `/usr/local/bin/odac`.
- **Windows:** A batch file `odac.bat` is created at `C:\Windows\System32\odac.bat`. This requires **Administrator privileges** to write successfully.

---

> [!NOTE]
> The installation script also reports progress to `hub.odac.run` using the `ODAC_CODE` environment variable if present. This allows for real-time installation tracking in the ODAC Cloud.
