// ODAC DNS Server — High-performance authoritative DNS server.
//
// This binary is spawned by Node.js (server/src/DNS.js) as a child process,
// mirroring the architecture of the Go proxy (server/proxy/main.go).
// Communication with Node.js occurs via a Unix socket control API.
//
// Architecture:
//   Node.js (DNS.js) --[Unix Socket]--> Go DNS (this binary)
//     POST /config  → full zone configuration sync
//     GET  /health  → liveness check
//
// The DNS server listens on UDP and TCP port 53 (or fallback ports) and
// serves authoritative responses for configured zones.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"

	"odac-dns/api"
	"odac-dns/resolver"
)

// fallbackPorts are tried in order when port 53 is unavailable.
// These match the Node.js DNS.js fallback logic.
var fallbackPorts = []int{5353, 1053, 8053}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("[DNS] Starting ODAC DNS Server...")

	// Increase file descriptor limit for high query throughput
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err == nil {
		rLimit.Cur = rLimit.Max
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
			log.Printf("[DNS] Error setting rlimit: %v", err)
		} else {
			log.Printf("[DNS] File descriptor limit set to: %d", rLimit.Cur)
		}
	}

	// Initialize resolver and rate limiter
	res := resolver.NewResolver()
	rateLimiter := resolver.NewRateLimiter(res)

	// Determine the DNS listening port
	port := determineDNSPort()

	// Start UDP and TCP DNS servers
	udpServer, tcpServer := startDNSServers(rateLimiter, port)

	// Print the active port for Node.js to parse from stdout
	// CRITICAL: Node.js parses this line to know which port DNS is listening on
	fmt.Printf("ODAC_DNS_PORT=%d\n", port)

	// Start Control API (Unix Socket or TCP fallback)
	apiListener := startControlAPI(res)

	// Wait for termination signal
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("[DNS] Shutting down gracefully...")

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if err := udpServer.ShutdownContext(ctx); err != nil {
			log.Printf("[DNS] UDP server shutdown error: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := tcpServer.ShutdownContext(ctx); err != nil {
			log.Printf("[DNS] TCP server shutdown error: %v", err)
		}
	}()

	wg.Wait()
	rateLimiter.Stop()

	if apiListener != nil {
		apiListener.Close()
	}

	log.Println("[DNS] ODAC DNS Server stopped.")
}

// determineDNSPort finds an available port for the DNS server.
// Tries port 53 first, then falls back to 5353, 1053, 8053.
func determineDNSPort() int {
	// Allow override via environment variable
	if envPort := os.Getenv("ODAC_DNS_PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			return p
		}
	}

	// Try port 53 first
	if isPortAvailable(53) {
		return 53
	}

	log.Println("[DNS] Port 53 is unavailable, trying fallback ports...")

	// Handle systemd-resolved on Linux
	if runtime.GOOS == "linux" {
		if handleSystemdResolved() {
			if isPortAvailable(53) {
				return 53
			}
		}
	}

	// Try fallback ports
	for _, p := range fallbackPorts {
		if isPortAvailable(p) {
			log.Printf("[DNS] Using fallback port %d", p)
			return p
		}
	}

	// Last resort: use 5353
	log.Println("[DNS] All preferred ports unavailable, forcing port 5353")
	return 5353
}

// isPortAvailable checks if a UDP port is available for binding.
func isPortAvailable(port int) bool {
	addr := fmt.Sprintf(":%d", port)
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// handleSystemdResolved attempts to resolve port 53 conflicts with
// systemd-resolved on Linux. Returns true if the conflict was resolved.
func handleSystemdResolved() bool {
	// Check if systemd-resolved is active
	content, err := os.ReadFile("/run/systemd/resolve/resolv.conf")
	if err != nil {
		return false
	}

	if !strings.Contains(string(content), "nameserver") {
		return false
	}

	log.Println("[DNS] Detected systemd-resolved, attempting to disable DNS stub listener...")

	// Create config to disable stub listener
	confDir := "/etc/systemd/resolved.conf.d"
	if err := os.MkdirAll(confDir, 0755); err != nil {
		log.Printf("[DNS] Cannot create resolved.conf.d: %v", err)
		return false
	}

	confContent := "[Resolve]\nDNSStubListener=no\n"
	confFile := confDir + "/odac-dns.conf"
	if err := os.WriteFile(confFile, []byte(confContent), 0644); err != nil {
		log.Printf("[DNS] Cannot write resolved config: %v", err)
		return false
	}

	// Restart systemd-resolved
	// Note: This requires appropriate permissions (root or sudo)
	log.Println("[DNS] Wrote systemd-resolved config, restart required for effect")
	return false // Conservative: don't assume restart succeeded
}

// startDNSServers creates and starts UDP and TCP DNS servers on the given port.
func startDNSServers(handler dns.Handler, port int) (*dns.Server, *dns.Server) {
	addr := fmt.Sprintf(":%d", port)

	udpServer := &dns.Server{
		Addr:    addr,
		Handler: handler,
		Net:     "udp",
		UDPSize: 4096, // EDNS0 support for larger responses
		ReusePort: runtime.GOOS == "linux",
	}

	tcpServer := &dns.Server{
		Addr:      addr,
		Handler:   handler,
		Net:       "tcp",
		ReusePort: runtime.GOOS == "linux",
	}

	go func() {
		log.Printf("[DNS] Starting UDP server on %s", addr)
		if err := udpServer.ListenAndServe(); err != nil {
			log.Fatalf("[DNS] UDP server failed: %v", err)
		}
	}()

	go func() {
		log.Printf("[DNS] Starting TCP server on %s", addr)
		if err := tcpServer.ListenAndServe(); err != nil {
			log.Fatalf("[DNS] TCP server failed: %v", err)
		}
	}()

	// Small delay to ensure servers are bound before returning
	time.Sleep(100 * time.Millisecond)
	log.Printf("[DNS] DNS servers started on port %d (UDP+TCP)", port)

	return udpServer, tcpServer
}

// startControlAPI starts the HTTP control API on a Unix socket or TCP fallback.
// Mirrors the proxy's API listener setup in main.go.
func startControlAPI(res *resolver.Resolver) net.Listener {
	socketPath := os.Getenv("ODAC_DNS_SOCKET_PATH")
	apiServer := api.NewServer(res)

	var listener net.Listener
	var err error

	if socketPath != "" {
		// Unix Socket Mode (Linux/macOS)
		// Clean up old socket if exists
		if _, statErr := os.Stat(socketPath); statErr == nil {
			if removeErr := os.Remove(socketPath); removeErr != nil {
				log.Printf("[DNS] Warning: Failed to remove old socket: %v", removeErr)
			}
		}

		listener, err = net.Listen("unix", socketPath)
		if err != nil {
			log.Fatalf("[DNS] Failed to start API listener on %s: %v", socketPath, err)
		}

		if chmodErr := os.Chmod(socketPath, 0660); chmodErr != nil {
			log.Printf("[DNS] Warning: Failed to chmod socket: %v", chmodErr)
		}

		log.Printf("[DNS] Control API listening on unix:%s", socketPath)

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
			log.Fatalf("[DNS] Failed to start API listener: %v", err)
		}

		apiPort := listener.Addr().(*net.TCPAddr).Port
		// CRITICAL: Node.js parses this line for API port discovery
		fmt.Printf("ODAC_DNS_API_PORT=%d\n", apiPort)
		log.Printf("[DNS] Control API listening on 127.0.0.1:%d", apiPort)
	}

	go func() {
		if err := http.Serve(listener, apiServer); err != nil {
			log.Printf("[DNS] Control API error: %v", err)
		}
	}()

	return listener
}
