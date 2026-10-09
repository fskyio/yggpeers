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

	"yggpeers/internal/config"
	"yggpeers/internal/coordinator"
	"yggpeers/internal/fetcher"
	"yggpeers/internal/metadata"
	"yggpeers/internal/server"
	"yggpeers/internal/store"
	"yggpeers/internal/ygg"
)

func main() {
	cfg := config.New()
	if len(os.Args) > 1 {
		if err := manageProbes(cfg, os.Args[1:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	var admin *ygg.Admin
	if cfg.YggdrasilAdmin != "" {
		var err error
		if admin, err = ygg.NewAdmin(cfg.YggdrasilAdmin); err != nil {
			log.Fatal(err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	st, err := store.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	f := fetcher.New(st)
	enricher := metadata.New(st, admin)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Println("Fetching peer list...")
	if err := f.FetchAndParse(ctx); err != nil {
		log.Printf("fetch: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		coordinator.Run(ctx, cfg, st)
	}()

	go func() {
		defer wg.Done()
		runEvery(ctx, cfg.FetchInterval, func() {
			if err := f.FetchAndParse(ctx); err != nil {
				log.Printf("fetch: %v", err)
			}
		})
	}()

	go func() {
		defer wg.Done()
		refresh := func() {
			if err := enricher.Refresh(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("metadata: %v", err)
			}
		}
		refresh()
		// Metadata has its own cache freshness. Refresh due hosts/IPs even
		// when the upstream peer list is unchanged.
		runEvery(ctx, time.Minute, refresh)
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
