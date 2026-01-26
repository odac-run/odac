# ⚡ ODAC



**ODAC** is a high-performance, autonomous server deployment system designed to simplify DevOps. It provides a robust, self-managing infrastructure for hosting and managing modern web applications with enterprise-grade stability.

## ✨ Key Features

*   ⚡ **High-Performance Architecture:** Features a hyper-optimized **Go proxy** for the data plane to handle massive concurrency with sub-millisecond latency, significantly outperforming traditional Node.js-only solutions.
*   🚀 **Zero-Config Deployment:** Deploy applications instantly without complex configuration files. Focus on your code while ODAC handles the infrastructure.
*   🐳 **Containerized Isolation:** Applications are automatically deployed in secure, lightweight containers. This provides robust resource isolation, preventing "noisy neighbor" issues and enhancing security.
*   🔒 **Automated Security:** Zero-touch SSL certificate generation and auto-renewal for all your domains.
*   📬 **Integrated Mail Server:** A complete, production-ready IMAP/SMTP solution for managing domain-specific email accounts without external dependencies.
*   ⚙️ **Advanced Monitoring:** Real-time process management, auto-recovery, and comprehensive CLI-based observability tools.
*   🔄 **Always-On & Self-Updating:** The system keeps itself secure and up-to-date with zero-downtime over-the-air updates, ensuring your infrastructure never sleeps or rots.



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
