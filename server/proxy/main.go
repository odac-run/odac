package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"odac-proxy/api"
	"odac-proxy/config"
	"odac-proxy/proxy"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// listen creates a net.Listener with SO_REUSEPORT support on Linux
func listen(network, address string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: setSocketOptions,
	}
	return lc.Listen(context.Background(), network, address)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("Starting ODAC Proxy...")

	// Increase file descriptor limit
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err == nil {
		rLimit.Cur = rLimit.Max
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
			log.Printf("Error setting rlimit: %v", err)
		} else {
			log.Printf("File descriptor limit set to: %d", rLimit.Cur)
		}
	}
	// Optimize kernel parameters for high-performance networking
	// These functions contain internal checks to only run on Linux with appropriate permissions
	optimizeUDPBuffers()   // Larger UDP buffers for HTTP/3 (QUIC)
	optimizeTCPCongestion() // BBR congestion control + FQ qdisc

	// Initialize components
	cfg := config.Firewall{Enabled: true} // Default
	fw := proxy.NewFirewall(cfg)
	prx := proxy.NewProxy()

	// Stack middleware: Firewall -> Proxy
	// We removed timeoutMiddleware because robust timeout handling is now done
	// via http.Transport (ResponseHeaderTimeout) and http.Server (IdleTimeout, ReadHeaderTimeout).
	handler := fw.Check(prx)

	// Check for Socket Environment Variable
	socketPath := os.Getenv("ODAC_SOCKET_PATH")

	var apiListener net.Listener
	var err error

	if socketPath != "" {
		// UNIX SOCKET MODE
		// Clean up old socket if exists
		if _, err := os.Stat(socketPath); err == nil {
			if err := os.Remove(socketPath); err != nil {
				log.Printf("Warning: Failed to remove old socket: %v", err)
			}
		}

		apiListener, err = net.Listen("unix", socketPath)
		if err != nil {
			log.Fatalf("Failed to start API listener on %s: %v", socketPath, err)
		}

		if err := os.Chmod(socketPath, 0660); err != nil {
			log.Printf("Warning: Failed to chmod socket: %v", err)
		}
		log.Printf("Control API listening on unix:%s", socketPath)

	} else {
		// TCP FALLBACK MODE (Windows or manual)
		apiListener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatalf("Failed to start API listener: %v", err)
		}

		apiPort := apiListener.Addr().(*net.TCPAddr).Port
		// CRITICAL: Print this specific line for Node.js to parse (Legacy/Windows mode)
		fmt.Printf("ODAC_PROXY_PORT=%d\n", apiPort)
		log.Printf("Control API listening on 127.0.0.1:%d", apiPort)
	}

	apiServer := api.NewServer(prx, fw)

	go func() {
		if err := http.Serve(apiListener, apiServer); err != nil {
			log.Fatalf("Control API failed: %v", err)
		}
	}()

	// Cleanup socket on exit if using socket
	if socketPath != "" {
		defer os.Remove(socketPath)
	}

	// Start HTTP Server (Port 80)
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10, // 32 KB limit for headers to prevent DoS
	}

	go func() {
		log.Println("Starting HTTP server on :80")
		listener, err := listen("tcp", ":80")
		if err != nil {
			log.Fatalf("HTTP listener failed: %v", err)
		}
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// Start HTTPS Server (Port 443)
	// ============================================================================
	// TLS PERFORMANCE & SECURITY OPTIMIZATION (Enterprise-Grade + Post-Quantum)
	// ============================================================================
	// 1. Post-Quantum: X25519MLKEM768 hybrid (ML-KEM-768 + X25519, NIST FIPS 203)
	//    - Protects against "Harvest Now, Decrypt Later" quantum attacks
	//    - Automatic fallback to X25519 for older clients
	// 2. Session Tickets: Stateless session resumption (1-RTT, ~50% latency ↓)
	// 3. Cipher Priority: AES-GCM for AES-NI CPUs, ChaCha20 fallback for ARM
	// 4. Renegotiation: Disabled to prevent CVE-2009-3555 class attacks
	// ============================================================================
	tlsConfig := &tls.Config{
		GetCertificate: prx.GetCertificate,
		NextProtos:     []string{"h3", "h2", "http/1.1"},
		MinVersion:     tls.VersionTLS12,

		// Session Resumption: Critical for reducing handshake latency
		// Go's TLS server automatically handles session tickets when SessionTicketsDisabled=false
		// This enables returning clients to resume sessions with 1-RTT instead of full handshake
		SessionTicketsDisabled: false,

		// Security: Disable TLS renegotiation to prevent CVE-2009-3555 class attacks
		// Client-initiated renegotiation can be used for DoS and MITM attacks
		// For a reverse proxy, there's no legitimate use case for renegotiation
		Renegotiation: tls.RenegotiateNever,

		// Curve Preferences: Post-quantum hybrid first, then classical curves
		// Order matters: most preferred first, client will use first mutually supported
		CurvePreferences: []tls.CurveID{
			tls.X25519MLKEM768, // Post-quantum hybrid: ML-KEM-768 + X25519 (NIST FIPS 203)
			tls.X25519,         // Classical fallback, ~40% faster than P-256
			tls.CurveP256,      // Widely compatible fallback
			tls.CurveP384,      // High-security fallback (slower)
		},

		// Cipher Suite Priority (TLS 1.2 only - TLS 1.3 uses its own fixed suites)
		// Order: ECDSA preferred (faster verification), RSA fallback
		// AES-GCM first for Intel/AMD with AES-NI, ChaCha20 for ARM/mobile
		PreferServerCipherSuites: true,
		CipherSuites: []uint16{
			// ECDSA suites (faster, requires ECDSA cert)
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, // Fastest with AES-NI
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305, // ARM/mobile friendly

			// RSA suites (broader compatibility)
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		},
	}

	httpsServer := &http.Server{
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10, // 32 KB limit for headers to prevent DoS
	}

	// Start HTTP/3 Server (UDP :443)
	h3Server := &http3.Server{
		Addr:      ":443",
		Handler:   handler,
		TLSConfig: tlsConfig,
		QUICConfig: &quic.Config{
			Allow0RTT: true,
		},
	}

	go func() {
		log.Println("Starting HTTP/3 server on :443 (UDP)")
		if err := h3Server.ListenAndServe(); err != nil {
			// Don't fatal on HTTP/3 failure, it might be a permission/network issue, just log it
			log.Printf("HTTP/3 server failed: %v", err)
			log.Println("Hint: If you see 'message too long' or performance issues, run: sysctl -w net.core.rmem_max=2500000")
		}
	}()

	go func() {
		log.Println("Starting HTTPS server on :443")
		listener, err := listen("tcp", ":443")
		if err != nil {
			log.Fatalf("HTTPS listener failed: %v", err)
		}
		if err := httpsServer.Serve(tls.NewListener(listener, tlsConfig)); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTPS server failed: %v", err)
		}
	}()

	// Wait for termination signal
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("ODAC Proxy shutting down gracefully...")

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Shutdown all servers
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := h3Server.Shutdown(ctx); err != nil {
			log.Printf("HTTP/3 server shutdown error: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := httpsServer.Shutdown(ctx); err != nil {
			log.Printf("HTTPS server shutdown error: %v", err)
		}
	}()

	// Wait for both shutdowns to complete
	wg.Wait()

	log.Println("ODAC Proxy stopped.")
}

// optimizeUDPBuffers attempts to increase UDP buffer sizes for better QUIC performance.
// Standard Linux default is usually too low (~212KB), causing packet drops at high speeds.
// We target 2.5MB which is recommended for high-performance QUIC servers.
func optimizeUDPBuffers() {
	if runtime.GOOS != "linux" {
		return
	}

	const targetSize = 2500000 // ~2.5 MB

	params := []string{
		"/proc/sys/net/core/rmem_max",
		"/proc/sys/net/core/wmem_max",
	}

	for _, path := range params {
		// Read current value
		content, err := os.ReadFile(path)
		if err != nil {
			// Fail silently/warn only, as we might not have permissions (e.g. non-root)
			log.Printf("[WARN] Could not read kernel param %s: %v", path, err)
			continue
		}

		valStr := strings.TrimSpace(string(content))
		currentVal, err := strconv.Atoi(valStr)
		if err != nil {
			log.Printf("[WARN] Could not parse kernel param %s value '%s': %v", path, valStr, err)
			continue
		}

		if currentVal < targetSize {
			// Attempt to update
			err := os.WriteFile(path, []byte(strconv.Itoa(targetSize)), 0644)
			if err != nil {
				log.Printf("[WARN] Failed to auto-tune %s: %v. HTTP/3 performance might be limited.", path, err)
			} else {
				log.Printf("[INFO] Optimized Kernel Buffer: %s (%d -> %d) for HTTP/3", path, currentVal, targetSize)
			}
		} else {
			// Already optimized
			// log.Printf("[DEBUG] Kernel param %s is already sufficient (%d)", path, currentVal)
		}
	}
}

// optimizeTCPCongestion enables BBR congestion control algorithm for better throughput.
// BBR (Bottleneck Bandwidth and RTT) is Google's model-based congestion control that:
// - Measures actual bandwidth and RTT instead of relying on packet loss signals
// - Achieves 20-50% better throughput on high-latency links (intercontinental)
// - Handles packet loss gracefully (up to ~20% loss with minimal degradation)
// - Reduces bufferbloat by not filling intermediate buffers
// FQ (Fair Queue) qdisc is required for BBR to function optimally.
//
// Note: This is a best-effort optimization. All errors are logged as warnings
// and the proxy continues to function normally. Safe for shared hosting.
func optimizeTCPCongestion() {
	if runtime.GOOS != "linux" {
		return
	}

	const congestionPath = "/proc/sys/net/ipv4/tcp_congestion_control"
	const qdiscPath = "/proc/sys/net/core/default_qdisc"
	const availablePath = "/proc/sys/net/ipv4/tcp_available_congestion_control"

	// Quick check: If BBR is already active, nothing to do
	currentCC, err := os.ReadFile(congestionPath)
	if err != nil {
		// Can't read - probably containerized without /proc access
		return
	}
	if strings.TrimSpace(string(currentCC)) == "bbr" {
		// BBR already active, nothing to do
		return
	}

	// Step 1: Check if BBR module is available
	available, err := os.ReadFile(availablePath)
	if err != nil {
		return
	}

	// Step 2: If BBR not in kernel, try to load the module (fails on OpenVZ/shared hosting)
	if !strings.Contains(string(available), "bbr") {
		if err := loadKernelModule("tcp_bbr"); err != nil {
			// Log specific error for debugging (is it permission? is it missing file?)
			log.Printf("[INFO] BBR unavailable. Kernel module load failed: %v", err)
			log.Printf("[INFO] Using standard TCP congestion control")
			return
		}
		// Re-check after loading module
		available, _ = os.ReadFile(availablePath)
		if !strings.Contains(string(available), "bbr") {
			log.Printf("[INFO] BBR module loaded but not accepted by kernel, using standard TCP")
			return
		}
		log.Printf("[INFO] Loaded tcp_bbr kernel module")
	}

	// Step 3: Set FQ qdisc (required for BBR to work optimally)
	currentQdisc, _ := os.ReadFile(qdiscPath)
	if strings.TrimSpace(string(currentQdisc)) != "fq" {
		if err := os.WriteFile(qdiscPath, []byte("fq"), 0644); err == nil {
			log.Printf("[INFO] Set default_qdisc: %s -> fq", strings.TrimSpace(string(currentQdisc)))
		} else {
			// Can't set qdisc but BBR might still work with default qdisc
			log.Printf("[INFO] Could not set FQ qdisc, BBR may have reduced effectiveness")
		}
	}

	// Step 4: Enable BBR congestion control
	if err := os.WriteFile(congestionPath, []byte("bbr"), 0644); err == nil {
		log.Printf("[INFO] Enabled TCP BBR: %s -> bbr", strings.TrimSpace(string(currentCC)))
	} else {
		log.Printf("[INFO] Could not enable BBR, continuing with %s", strings.TrimSpace(string(currentCC)))
	}
}

// loadKernelModule attempts to load a kernel module using modprobe.
// This requires root/privileged access and /lib/modules mounted from host.
func loadKernelModule(moduleName string) error {
	cmd := exec.Command("modprobe", moduleName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, string(output))
	}
	return nil
}
