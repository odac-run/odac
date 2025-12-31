package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"odac-proxy/api"
	"odac-proxy/config"
	"odac-proxy/proxy"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("Starting ODAC Proxy...")

	// Initialize components
	cfg := config.Firewall{Enabled: true} // Default
	fw := proxy.NewFirewall(cfg)
	prx := proxy.NewProxy()

	// Stack middleware: Firewall -> Proxy
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

		if err := os.Chmod(socketPath, 0777); err != nil {
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
			Addr:         ":80",
			Handler:      handler,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  120 * time.Second,
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
			Addr:         ":443",
			Handler:      handler,
			TLSConfig:    tlsConfig,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  120 * time.Second,
		}

		if err := server.ListenAndServeTLS("", ""); err != nil {
			log.Printf("HTTPS server failed: %v", err)
		}
	}()

	// Wait for termination signal
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("ODAC Proxy shutting down...")
}

// Make sure Server implements ServeHTTP by adding Method in Api.go or here if used as Handler directly
// api.Server implementation in previous step has HandleConfig but needs to implement http.Handler if we used it as such.
// In api.go I used NewServeMux inside ListenAndServe.
// I should adjust api.go to expose the handler or use it properly in Start.
// Since I did custom net.Listen here, I need to pass the mux to http.Serve.
// Let's refactor api.go slightly in next tool call or fix it right here by assuming api.Server has a Handler method?
// Actually in api.go I wrote `ListenAndServe` which does `http.ListenAndServe(addr, mux)`.
// Here I want `http.Serve(listener, handler)`.
// So I should let `api.Server` create the handler.
