package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	logger := log.New(os.Stdout, "schema-registry ", log.LstdFlags|log.LUTC)

	port := getenv("PORT", "8080")
	dataFile := getenv("DATA_FILE", "")
	if dataFile == "" {
		dataFile = filepath.Join(getenv("DATA_DIR", "data"), "schemas.json")
	}
	if err := os.MkdirAll(filepath.Dir(dataFile), 0o755); err != nil {
		logger.Fatalf("cannot create data directory: %v", err)
	}

	store, err := OpenStore(dataFile)
	if err != nil {
		logger.Fatalf("cannot open store: %v", err)
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           newMux(&api{store: store}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	logger.Printf("listening on :%s, state file %s", port, dataFile)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("server failed: %v", err)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
