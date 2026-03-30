package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	redisAddr := flag.String("redis", "127.0.0.1:6379", "Redis address")
	listenAddr := flag.String("listen", ":8080", "HTTP listen address")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Println("=== Token API Server ===")

	store, err := NewRedisStore(*redisAddr)
	if err != nil {
		log.Fatalf("[FATAL] Redis connection failed: %v", err)
	}
	defer store.Close()
	log.Printf("[REDIS] Connected to %s", *redisAddr)

	handler := NewHandler(store)

	server := &http.Server{
		Addr:    *listenAddr,
		Handler: handler,
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("[SHUTDOWN] Shutting down...")
		server.Close()
	}()

	log.Printf("[HTTP] Listening on %s", *listenAddr)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("[FATAL] HTTP server error: %v", err)
	}
	log.Println("[SHUTDOWN] Server stopped")
}
