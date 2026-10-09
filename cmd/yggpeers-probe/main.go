package main

import (
	"context"
	"errors"
	"log"
	"os/signal"
	"syscall"

	"yggpeers/internal/config"
	"yggpeers/internal/probe"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.NewProbe()
	if err != nil {
		return err
	}
	worker, err := probe.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("Starting outbound probe for %s", cfg.ServerURL)
	if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
