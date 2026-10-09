package checker

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/quic-go/quic-go"

	"yggpeers/internal/config"
	"yggpeers/internal/protocol"
)

type Checker struct {
	cfg        config.ProbeConfig
	dialer     *net.Dialer
	httpClient *http.Client
}

func New(cfg config.ProbeConfig) *Checker {
	dialer := &net.Dialer{Timeout: cfg.DialTimeout}
	return &Checker{
		cfg:    cfg,
		dialer: dialer,
		httpClient: &http.Client{
			Timeout: cfg.DialTimeout,
			Transport: &http.Transport{
				DialContext: dialer.DialContext,
				// These are peer reachability checks, not authenticated API traffic.
				TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
				TLSHandshakeTimeout:   cfg.DialTimeout,
				ResponseHeaderTimeout: cfg.DialTimeout,
			},
		},
	}
}

// Check reports transport reachability, not protocol-level peer validity.
// A per-target timeout is down; cancellation of the assignment is no result.
func (c *Checker) Check(ctx context.Context, target protocol.Target) (protocol.Result, error) {
	if err := ctx.Err(); err != nil {
		return protocol.Result{}, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()
	addr := net.JoinHostPort(target.Host, target.Port)
	var up bool
	switch target.Protocol {
	case "quic":
		up = c.dialQUIC(checkCtx, addr)
	case "ws":
		up = c.probeHTTP(checkCtx, "http", target.Host, target.Port)
	case "wss":
		up = c.probeHTTP(checkCtx, "https", target.Host, target.Port)
	default:
		// TCP and TLS intentionally both use TCP connection reachability.
		up = c.dialTCP(checkCtx, addr)
	}
	if err := ctx.Err(); err != nil {
		return protocol.Result{}, err
	}
	status := protocol.Down
	if up {
		status = protocol.Up
	}
	return protocol.Result{PeerID: target.PeerID, Status: status}, nil
}

func (c *Checker) dialTCP(ctx context.Context, addr string) bool {
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
		// An ALPN error means QUIC/TLS negotiation completed: peer is up.
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

func (c *Checker) probeHTTP(ctx context.Context, scheme, host, port string) bool {
	url := fmt.Sprintf("%s://%s/", scheme, net.JoinHostPort(host, port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if c.cfg.Debug {
			log.Printf("http check %s: %v", url, err)
		}
		return false
	}
	resp.Body.Close()
	return true
}
