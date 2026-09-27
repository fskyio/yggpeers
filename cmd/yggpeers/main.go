package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"yggpeers/internal/checker"
	"yggpeers/internal/config"
	"yggpeers/internal/fetcher"
	"yggpeers/internal/server"
	"yggpeers/internal/store"
)

func main() {
	cfg := config.New()

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	st, err := store.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	f := fetcher.New(st)
	c := checker.New(cfg, st)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Println("Fetching peer list...")
	if err := f.FetchAndParse(ctx); err != nil {
		log.Printf("fetch: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		log.Println("Running uptime checks...")
		if err := c.CheckAll(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("check: %v", err)
		}
		runEvery(ctx, cfg.CheckInterval, func() {
			if err := c.CheckAll(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("check: %v", err)
			}
		})
	}()

	go func() {
		defer wg.Done()
		runEvery(ctx, cfg.FetchInterval, func() {
			if err := f.FetchAndParse(ctx); err != nil {
				log.Printf("fetch: %v", err)
			}
		})
	}()

	srv, err := server.New(cfg, st)
	if err != nil {
		log.Fatalf("create server: %v", err)
	}

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("Listening on %s", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case <-ctx.Done():
		log.Println("Shutting down...")
	case err := <-serverErr:
		if err != nil {
			log.Printf("http server: %v", err)
		}
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}

	wg.Wait()
}

// runEvery invokes fn on the given interval until ctx is cancelled.
func runEvery(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}
