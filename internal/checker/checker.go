package checker

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"yggpeers/internal/config"
	"yggpeers/internal/store"
)

type Checker struct {
	cfg        config.Config
	store      *store.Store
	dialer     *net.Dialer
	httpClient *http.Client
}

func New(cfg config.Config, s *store.Store) *Checker {
	dialer := &net.Dialer{Timeout: cfg.DialTimeout}
	return &Checker{
		cfg:    cfg,
		store:  s,
		dialer: dialer,
		httpClient: &http.Client{
			Timeout: cfg.DialTimeout,
			Transport: &http.Transport{
				DialContext:           dialer.DialContext,
				TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
				TLSHandshakeTimeout:   cfg.DialTimeout,
				ResponseHeaderTimeout: cfg.DialTimeout,
			},
		},
	}
}

func (c *Checker) CheckAll(ctx context.Context) error {
	allPeers, err := c.store.GetAllPeers()
	if err != nil {
		return err
	}

	// Filter out overlay-network peers when CHECK_DARKNET is disabled.
	// Recording them as "down" would corrupt their uptime numbers.
	peers := allPeers
	if !c.cfg.CheckDarknet {
		peers = peers[:0]
		for _, p := range allPeers {
			if !p.IsNetwork {
				peers = append(peers, p)
			}
		}
	}

	log.Printf("Checking %d peers...", len(peers))

	results := make([]store.CheckResult, len(peers))
	batchSize := c.cfg.MaxConcurrentChecks
	if batchSize < 1 {
		batchSize = 1
	}

	for batchStart := 0; batchStart < len(peers); batchStart += batchSize {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if batchStart > 0 && c.cfg.BatchDelay > 0 {
			select {
			case <-time.After(c.cfg.BatchDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		end := batchStart + batchSize
		if end > len(peers) {
			end = len(peers)
		}
		batch := peers[batchStart:end]

		var wg sync.WaitGroup
		for i, p := range batch {
			wg.Add(1)
			go func(i int, p store.Peer) {
				defer wg.Done()
				results[batchStart+i] = store.CheckResult{PeerID: p.ID, IsUp: c.check(ctx, p)}
			}(i, p)
		}
		wg.Wait()
	}

	if err := c.store.RecordChecks(results); err != nil {
		log.Printf("warn: record checks: %v", err)
	}

	if err := c.store.PruneOldChecks(store.StatsWindowDays); err != nil {
		log.Printf("warn: prune old checks: %v", err)
	}
	log.Println("Check complete.")
	return nil
}

func (c *Checker) check(ctx context.Context, p store.Peer) bool {
	addr := net.JoinHostPort(p.Host, p.Port)
	switch p.Protocol {
	case "tcp", "tls":
		return c.dialTCP(ctx, addr)
	case "quic":
		return c.dialQUIC(ctx, addr)
	case "ws":
		return c.probeHTTP("http", p.Host, p.Port)
	case "wss":
		return c.probeHTTP("https", p.Host, p.Port)
	default:
		return c.dialTCP(ctx, addr)
	}
}

func (c *Checker) dialTCP(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()
	conn, err := c.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		if c.cfg.Debug {
			log.Printf("tcp check %s: %v", addr, err)
		}
		return false
	}
	conn.Close()
	return true
}

func (c *Checker) dialQUIC(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		if c.cfg.Debug {
			log.Printf("quic check %s: bad addr: %v", addr, err)
		}
		return false
	}
	dialAddr := addr
	serverName := host
	if net.ParseIP(host) == nil {
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil {
			if c.cfg.Debug {
				log.Printf("quic check %s: dns: %v", addr, err)
			}
			return false
		}
		if len(addrs) == 0 {
			if c.cfg.Debug {
				log.Printf("quic check %s: dns: no addresses", addr)
			}
			return false
		}
		dialAddr = net.JoinHostPort(addrs[0], port)
	}

	conn, err := quic.DialAddr(ctx, dialAddr, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         serverName,
		NextProtos:         []string{"h3"},
	}, nil)
	if err != nil {
		// ALPN error means QUIC/TLS negotiation completed — peer is up.
		if strings.Contains(err.Error(), "ALPN") {
			return true
		}
		if c.cfg.Debug {
			log.Printf("quic check %s: down (%v)", addr, err)
		}
		return false
	}
	conn.CloseWithError(0, "")
	return true
}

func (c *Checker) probeHTTP(scheme, host, port string) bool {
	url := fmt.Sprintf("%s://%s/", scheme, net.JoinHostPort(host, port))
	resp, err := c.httpClient.Get(url)
	if err != nil {
		if c.cfg.Debug {
			log.Printf("http check %s: %v", url, err)
		}
		return false
	}
	resp.Body.Close()
	return true
}
