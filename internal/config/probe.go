package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ProbeConfig configures an outbound-only probe, independently of the server.
type ProbeConfig struct {
	ServerURL           string
	Token               string
	DialTimeout         time.Duration
	MaxConcurrentChecks int
	BatchDelay          time.Duration
	CheckDarknet        bool
	Debug               bool
}

func NewProbe() (ProbeConfig, error) {
	cfg := ProbeConfig{
		ServerURL:           os.Getenv("SERVER_URL"),
		Token:               os.Getenv("PROBE_TOKEN"),
		DialTimeout:         5 * time.Second,
		MaxConcurrentChecks: 50,
		CheckDarknet:        os.Getenv("CHECK_DARKNET") != "",
		Debug:               os.Getenv("DEBUG") != "",
	}
	if file := os.Getenv("PROBE_TOKEN_FILE"); file != "" {
		if cfg.Token != "" {
			return ProbeConfig{}, fmt.Errorf("set exactly one of PROBE_TOKEN and PROBE_TOKEN_FILE")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return ProbeConfig{}, fmt.Errorf("read PROBE_TOKEN_FILE: %w", err)
		}
		cfg.Token = strings.TrimSpace(string(data))
	}
	for key, dest := range map[string]*time.Duration{"DIAL_TIMEOUT": &cfg.DialTimeout, "BATCH_DELAY": &cfg.BatchDelay} {
		if value := os.Getenv(key); value != "" {
			d, err := time.ParseDuration(value)
			if err != nil {
				return ProbeConfig{}, fmt.Errorf("%s: %w", key, err)
			}
			*dest = d
		}
	}
	if value := os.Getenv("MAX_CONCURRENT_CHECKS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			return ProbeConfig{}, fmt.Errorf("MAX_CONCURRENT_CHECKS: %w", err)
		}
		cfg.MaxConcurrentChecks = n
	}
	if err := cfg.Validate(); err != nil {
		return ProbeConfig{}, err
	}
	cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")
	return cfg, nil
}

func (cfg ProbeConfig) Validate() error {
	u, err := url.Parse(cfg.ServerURL)
	if err != nil || u == nil || u.Opaque != "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("SERVER_URL must be an absolute server origin without credentials, path, query or fragment")
	}
	if strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("SERVER_URL has an empty port")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("SERVER_URL has an invalid port")
		}
	}
	switch u.Scheme {
	case "https":
	case "http":
		ip := net.ParseIP(u.Hostname())
		if !strings.EqualFold(u.Hostname(), "localhost") && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("SERVER_URL allows HTTP only for loopback hosts; use HTTPS")
		}
	default:
		return fmt.Errorf("SERVER_URL must use HTTPS (or loopback HTTP)")
	}
	if cfg.Token == "" {
		return fmt.Errorf("PROBE_TOKEN or PROBE_TOKEN_FILE is required")
	}
	for _, ch := range cfg.Token {
		if ch <= ' ' || ch > '~' {
			return fmt.Errorf("probe token must contain only non-whitespace printable ASCII")
		}
	}
	if cfg.DialTimeout <= 0 || cfg.MaxConcurrentChecks <= 0 || cfg.BatchDelay < 0 {
		return fmt.Errorf("DIAL_TIMEOUT and MAX_CONCURRENT_CHECKS must be positive; BATCH_DELAY must be nonnegative")
	}
	return nil
}
