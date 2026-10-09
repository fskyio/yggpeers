package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	ListenAddr    string
	DBPath        string
	FetchInterval time.Duration
	CheckInterval time.Duration
	RoundTimeout  time.Duration
}

func New() Config {
	interval := getDuration("CHECK_INTERVAL", 5*time.Minute)
	return Config{
		ListenAddr:    getenv("LISTEN_ADDR", ":8080"),
		DBPath:        getenv("DB_PATH", "./data/yggpeers.db"),
		FetchInterval: getDuration("FETCH_INTERVAL", 15*time.Minute),
		CheckInterval: interval,
		RoundTimeout:  getDuration("ROUND_TIMEOUT", interval*4/5),
	}
}

func (c Config) Validate() error {
	if c.FetchInterval <= 0 || c.CheckInterval <= 0 {
		return fmt.Errorf("FETCH_INTERVAL and CHECK_INTERVAL must be positive")
	}
	if c.RoundTimeout <= 0 || c.RoundTimeout >= c.CheckInterval {
		return fmt.Errorf("ROUND_TIMEOUT must be positive and less than CHECK_INTERVAL")
	}
	return nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 1 {
		return def
	}
	return n
}

func getDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
