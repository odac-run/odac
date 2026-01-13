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
	"strings"
	"syscall"
	"time"

	"odac-proxy/api"
	"odac-proxy/config"
	"odac-proxy/proxy"
)

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

	// Monitor parent process via Stdin
	go func() {
		buf := make([]byte, 1)
		_, err := os.Stdin.Read(buf)
		if err != nil {
			// Pipe closed or error = parent died
			log.Println("Parent process disconnected (stdin closed), shutting down...")
			os.Exit(0)
		}
	}()

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
	go func() {
		log.Println("Starting HTTP server on :80")
		server := &http.Server{
			Addr:              ":80",
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,  // Security: Mitigate Slowloris
			ReadTimeout:       0,                // Unlimited for large uploads (rely on TCP keepalive)
			WriteTimeout:      0,                // Unlimited for large downloads (streaming)
			IdleTimeout:       120 * time.Second,
		}
		if err := server.ListenAndServe(); err != nil {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// Start HTTPS Server (Port 443)
	go func() {
		log.Println("Starting HTTPS server on :443")
		
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

		server := &http.Server{
			Addr:              ":443",
			Handler:           handler,
			TLSConfig:         tlsConfig,
			ReadHeaderTimeout: 5 * time.Second,  // Security: Mitigate Slowloris
			ReadTimeout:       0,                // Unlimited for large uploads
			WriteTimeout:      0,                // Unlimited for large downloads
			IdleTimeout:       120 * time.Second,
		}

		if err := server.ListenAndServeTLS("", ""); err != nil {
			log.Fatalf("HTTPS server failed: %v", err)
		}
	}()

	// Wait for termination signal
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("ODAC Proxy shutting down...")
}


