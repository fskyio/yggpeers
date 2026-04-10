package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	ListenAddr          string
	RepoDir             string
	DBPath              string
	FetchInterval       time.Duration
	CheckInterval       time.Duration
	DialTimeout         time.Duration
	MaxConcurrentChecks int
	BatchDelay          time.Duration
	CheckDarknet        bool
	Debug               bool
}

func New() Config {
	return Config{
		ListenAddr:          getenv("LISTEN_ADDR", ":8080"),
		RepoDir:             getenv("REPO_DIR", "./data/public-peers"),
		DBPath:              getenv("DB_PATH", "./data/yggpeers.db"),
		FetchInterval:       getDuration("FETCH_INTERVAL", 15*time.Minute),
		CheckInterval:       getDuration("CHECK_INTERVAL", 5*time.Minute),
		DialTimeout:         getDuration("DIAL_TIMEOUT", 5*time.Second),
		MaxConcurrentChecks: getInt("MAX_CONCURRENT_CHECKS", 50),
		BatchDelay:          getDuration("BATCH_DELAY", 0),
		CheckDarknet:        os.Getenv("CHECK_DARKNET") != "",
		Debug:               os.Getenv("DEBUG") != "",
	}
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
