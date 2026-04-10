// ODAC Mail Server — High-performance SMTP and IMAP server.
//
// This binary is spawned by Node.js (server/src/Mail.js) as a child process,
// mirroring the architecture of the Go proxy (server/proxy/main.go) and
// Go DNS (server/dns/main.go).
//
// Communication with Node.js occurs via a Unix socket control API:
//
//	POST /config           → full configuration sync (domains, SSL, accounts)
//	POST /account          → create mail account
//	DELETE /account         → delete mail account
//	PUT /account/password  → update account password
//	GET /accounts          → list accounts for domain
//	POST /ssl/clear        → clear TLS context cache
//	GET /health            → liveness check
//
// The mail server listens on:
//
//	Port 25   — SMTP (plaintext with STARTTLS)
//	Port 143  — IMAP (plaintext with STARTTLS)
//	Port 465  — SMTP (implicit TLS)
//	Port 993  — IMAP (implicit TLS)
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"odac-mail/api"
	"odac-mail/auth"
	"odac-mail/config"
	smtpserver "odac-mail/smtp"
	"odac-mail/storage"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("[Mail] Starting ODAC Mail Server...")

	// Increase file descriptor limit for high connection throughput
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err == nil {
		rLimit.Cur = rLimit.Max
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
			log.Printf("[Mail] Error setting rlimit: %v", err)
		} else {
			log.Printf("[Mail] File descriptor limit set to: %d", rLimit.Cur)
		}
	}

	// Initialize storage
	store, err := storage.NewStore("")
	if err != nil {
		log.Fatalf("[Mail] Failed to initialize database: %v", err)
	}
	defer store.Close()

	// Initialize firewall
	fw := auth.NewFirewall()

	// Configuration state — updated atomically via control API
	var currentConfig config.Config
	var configMu sync.RWMutex

	onConfig := func(cfg config.Config) {
		configMu.Lock()
		currentConfig = cfg
		configMu.Unlock()
		log.Printf("[Mail] Configuration updated: %d domains", len(cfg.Domains))
	}

	// Expose config getter for SMTP/IMAP servers
	getConfig := func() config.Config {
		configMu.RLock()
		defer configMu.RUnlock()
		return currentConfig
	}

	// Start Control API
	apiListener := startControlAPI(store, fw, onConfig)
	defer apiListener.Close()

	// Start SMTP Server (ports 25 and 465)
	smtpSrv := smtpserver.NewServer(store, fw, getConfig)
	smtpSrv.Start()
	defer smtpSrv.Stop()

	// IMAP server will be started here in Phase 3.
	// Port reservations:
	//   143 — IMAP plaintext (STARTTLS)
	//   993 — IMAP implicit TLS
	log.Println("[Mail] SMTP servers started. IMAP server pending Phase 3 implementation.")

	// Wait for termination signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("[Mail] Shutting down gracefully...")

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	// Shutdown SMTP servers
	wg.Add(1)
	go func() {
		defer wg.Done()
		smtpSrv.Stop()
	}()

	// Future: Add IMAP server shutdown to WaitGroup here
	_ = ctx

	wg.Wait()
	log.Println("[Mail] ODAC Mail Server stopped.")
}

// startControlAPI starts the HTTP control API on a Unix socket or TCP fallback.
// Mirrors the proxy and DNS API listener setup for architectural consistency.
func startControlAPI(store *storage.Store, fw *auth.Firewall, onConfig func(config.Config)) net.Listener {
	socketPath := os.Getenv("ODAC_MAIL_SOCKET_PATH")
	apiServer := api.NewServer(store, fw, onConfig)

	var listener net.Listener
	var err error

	if socketPath != "" {
		// Unix Socket Mode (Linux/macOS)
		if _, statErr := os.Stat(socketPath); statErr == nil {
			if removeErr := os.Remove(socketPath); removeErr != nil {
				log.Printf("[Mail] Warning: Failed to remove old socket: %v", removeErr)
			}
		}

		listener, err = net.Listen("unix", socketPath)
		if err != nil {
			log.Fatalf("[Mail] Failed to start API listener on %s: %v", socketPath, err)
		}

		if chmodErr := os.Chmod(socketPath, 0660); chmodErr != nil {
			log.Printf("[Mail] Warning: Failed to chmod socket: %v", chmodErr)
		}

		log.Printf("[Mail] Control API listening on unix:%s", socketPath)

		// Cleanup socket on exit
		go func() {
			c := make(chan os.Signal, 1)
			signal.Notify(c, os.Interrupt, syscall.SIGTERM)
			<-c
			os.Remove(socketPath)
		}()
	} else {
		// TCP Fallback Mode (Windows or manual override)
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatalf("[Mail] Failed to start API listener: %v", err)
		}

		apiPort := listener.Addr().(*net.TCPAddr).Port
		// CRITICAL: Node.js parses this line for API port discovery
		fmt.Printf("ODAC_MAIL_PORT=%d\n", apiPort)
		log.Printf("[Mail] Control API listening on 127.0.0.1:%d", apiPort)
	}

	go func() {
		if err := http.Serve(listener, apiServer); err != nil {
			log.Printf("[Mail] Control API error: %v", err)
		}
	}()

	return listener
}
