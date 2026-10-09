package config

import (
	"testing"
	"time"
)

func TestProbeRejectsUnsafeConfiguration(t *testing.T) {
	base := ProbeConfig{ServerURL: "https://example.com", Token: "opaque-token", DialTimeout: time.Second, MaxConcurrentChecks: 2}
	cases := []struct {
		name   string
		change func(*ProbeConfig)
	}{
		{"missing URL", func(c *ProbeConfig) { c.ServerURL = "" }},
		{"relative URL", func(c *ProbeConfig) { c.ServerURL = "example.com" }},
		{"nonloopback HTTP", func(c *ProbeConfig) { c.ServerURL = "http://example.com" }},
		{"URL credentials", func(c *ProbeConfig) { c.ServerURL = "https://user:password@example.com" }},
		{"URL query", func(c *ProbeConfig) { c.ServerURL = "https://example.com?token=secret" }},
		{"invalid port", func(c *ProbeConfig) { c.ServerURL = "https://example.com:70000" }},
		{"empty port", func(c *ProbeConfig) { c.ServerURL = "https://example.com:" }},
		{"missing token", func(c *ProbeConfig) { c.Token = "" }},
		{"invalid token", func(c *ProbeConfig) { c.Token = "token\r\nOther: header" }},
		{"zero timeout", func(c *ProbeConfig) { c.DialTimeout = 0 }},
		{"zero concurrency", func(c *ProbeConfig) { c.MaxConcurrentChecks = 0 }},
		{"negative delay", func(c *ProbeConfig) { c.BatchDelay = -time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.change(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("unsafe configuration accepted: %+v", cfg)
			}
		})
	}
	for _, serverURL := range []string{"https://example.com", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		cfg := base
		cfg.ServerURL = serverURL
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid %s: %v", serverURL, err)
		}
	}
}

func TestNewProbeRejectsMalformedLimits(t *testing.T) {
	t.Setenv("SERVER_URL", "http://127.0.0.1:8080")
	t.Setenv("PROBE_TOKEN", "opaque-token")
	t.Setenv("PROBE_TOKEN_FILE", "")
	t.Setenv("DIAL_TIMEOUT", "1s")
	t.Setenv("MAX_CONCURRENT_CHECKS", "2")
	t.Setenv("BATCH_DELAY", "0s")
	for _, key := range []string{"DIAL_TIMEOUT", "MAX_CONCURRENT_CHECKS", "BATCH_DELAY"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "invalid")
			if _, err := NewProbe(); err == nil {
				t.Fatalf("malformed %s was silently defaulted", key)
			}
		})
	}
	t.Setenv("PROBE_TOKEN_FILE", "secret-file")
	if _, err := NewProbe(); err == nil {
		t.Fatal("both token sources accepted")
	}
}
