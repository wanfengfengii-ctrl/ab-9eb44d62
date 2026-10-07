package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"schema-registry/internal/httpserver"
	"schema-registry/internal/registry"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	port := envOr("PORT", "8080")
	dataFile := envOr("DATA_FILE", filepath.Join("data", "registry.json"))

	if dir := filepath.Dir(dataFile); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("create data directory: %v", err)
		}
	}

	store, err := registry.OpenStore(dataFile)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	svc := registry.NewService(store)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           httpserver.New(svc),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("schema registry listening on :%s (data file: %s)", port, dataFile)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}
