<p align="center">
  <img src="https://odac.run/assets/img/github/odac/header.png?v=1" alt="ODAC Header">
</p>

# ⚡ ODAC



**ODAC** is a high-performance, autonomous server deployment system designed to simplify DevOps. It provides a robust, self-managing infrastructure for hosting and managing modern web applications with enterprise-grade stability.

## ✨ Key Features

*   🛠️ **Zero-Bloat Architecture:** Engineered for maximum efficiency, leaving almost all system resources for your applications. No external dependencies like Redis, Postgres, Nginx, or Traefik required. Just download and run.
*   ⚡ **Next-Gen Performance:** Built-in Go Proxy automatically upgrades legacy apps (Node.js, PHP, Python) to HTTP/3 (QUIC) and 0-RTT. Get instant page loads without changing a single line of code.
*   🚀 **Zero-Config Deployment:** Push your code, and ODAC handles the build, ports, and reverse proxying automatically.
*   🔄 **Zero-Downtime Infrastructure:** ODAC updates itself with zero downtime and automatic rollbacks. For your apps, it uses true Blue-Green deployments with TCP Readiness Probes—traffic atomically switches only when your new container is fully booted and listening, ensuring 100% uptime.
*   🐳 **Secure Isolation:** Applications run in isolated lightweight containers, preventing "noisy neighbor" issues.
*   🔒 **Autopilot Security:** Zero-touch SSL generation, auto-renewal, and strict traffic analysis (Replay Attack protection).
*   📬 **Built-in Mail Server:** A production-ready SMTP/IMAP server included. No need for external email services.


## 🚀 Quick Start

> 🔥 **Install with a single command. Works on Linux, macOS, and Windows.**

#### Linux & macOS

```bash
curl -sL https://get.odac.run | sudo bash
```

#### Windows (PowerShell)

```powershell
irm https://get.odac.run | iex
```

This command:

- 🐳 **Installs Docker** automatically if it's missing from your system.
- 📦 **Deploys ODAC** inside a secure, production-ready container.
- 🚀 **Initializes the System** and prepares it for immediate use.


## 💻 CLI & Usage

After installation, simply run `odac` to view the **System Dashboard**, status, and available commands:

```bash
odac
```

To deploy a new application from a repository or template:

```bash
odac app create
```

## ☁️ ODAC Cloud (Beta)

Connect your servers to **ODAC Cloud** for a unified dashboard experience. Manage multiple servers, view aggregated metrics, and deploy apps from a single interface.

> 🚧 **Closed Beta:** ODAC Cloud is currently in closed beta. [Join the waitlist](https://odac.run) to get early access.

*   **Remote Management:** Control your servers from anywhere.
*   **Real-Time Metrics:** Visualize detailed performance data.
*   **Multi-Server Aggregation:** Manage your entire fleet in one place.
## 📚 Documentation

For more detailed information and API reference, please check out our [official documentation website](https://docs.odac.run).

## 📄 License

This project is licensed under the AGPL-3.0 License. See the [LICENSE](LICENSE) file for details.
