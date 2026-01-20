package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"odac-proxy/api"
	"odac-proxy/config"
	"odac-proxy/proxy"
)

// listen creates a net.Listener with SO_REUSEPORT support on Linux
func listen(network, address string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var opErr error
			if runtime.GOOS == "linux" {
				if err := c.Control(func(fd uintptr) {
					// SO_REUSEPORT is typically 15 on Linux/amd64
					// using syscall.SO_REUSEPORT is safer if available, but let's try standard syscall first
					// If syscall.SO_REUSEPORT is not defined on non-linux at compile time, this block inside runtime.GOOS check 
					// might still cause compilation error if we are not careful.
					// However, since we are editing a file that compiled before, syscall.SO_REUSEPORT might be available 
					// in the environment we are building (Docker Linux).
					// For safety against local Mac linting, we can use the constant value 0x0F (15) for Linux.
					
					opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, 0x0F, 1)
				}); err != nil {
					return err
				}
			}
			return opErr
		},
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
	tlsConfig := &tls.Config{
		GetCertificate: prx.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
		MinVersion:     tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		},
	}

	httpsServer := &http.Server{
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

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

	// Shutdown both servers
	go func() {
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}
	}()

	if err := httpsServer.Shutdown(ctx); err != nil {
		log.Printf("HTTPS server shutdown error: %v", err)
	}

	log.Println("ODAC Proxy stopped.")
}


