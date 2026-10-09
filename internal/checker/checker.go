// Package checker opens peer URIs the way a Yggdrasil node dials them and
// verifies the handshake the peer sends.
package checker

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/coder/websocket"
	"github.com/quic-go/quic-go"
	"golang.org/x/net/proxy"

	"yggpeers/internal/config"
	"yggpeers/internal/endpoint"
	"yggpeers/internal/protocol"
	"yggpeers/internal/ygg"
)

type Checker struct {
	cfg      config.ProbeConfig
	dialer   *net.Dialer
	wsClient *http.Client
}

func New(cfg config.ProbeConfig) *Checker {
	dialer := &net.Dialer{Timeout: cfg.DialTimeout}
	return &Checker{
		cfg:    cfg,
		dialer: dialer,
		wsClient: &http.Client{Transport: &http.Transport{
			DialContext:       dialer.DialContext,
			TLSClientConfig:   tlsConfig(""),
			DisableKeepAlives: true,
		}},
	}
}

// Check reports a peer up once its URI yields a validly signed Yggdrasil
// handshake whose key matches any ?key= pins. The probe only listens and never
// sends its own handshake, so the peer drops the link before routing starts.
// Any protocol version counts. A per-target timeout is down; cancellation of
// the assignment is no result.
func (c *Checker) Check(ctx context.Context, target protocol.Target) (protocol.Result, error) {
	if err := ctx.Err(); err != nil {
		return protocol.Result{}, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()
	meta, err := c.handshake(checkCtx, target.URI)
	if err := ctx.Err(); err != nil {
		return protocol.Result{}, err
	}
	if err != nil {
		if c.cfg.Debug {
			log.Printf("check %s: down: %v", target.URI, err)
		}
		return protocol.Result{PeerID: target.PeerID, Status: protocol.Down}, nil
	}
	result := protocol.Result{
		PeerID:          target.PeerID,
		Status:          protocol.Up,
		Key:             hex.EncodeToString(meta.PublicKey),
		ProtocolVersion: fmt.Sprintf("%d.%d", meta.MajorVersion, meta.MinorVersion),
	}
	if c.cfg.Debug {
		log.Printf("check %s: up: key %s, protocol %s", target.URI, result.Key, result.ProtocolVersion)
	}
	return result, nil
}

func (c *Checker) handshake(ctx context.Context, uri string) (ygg.Metadata, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return ygg.Metadata{}, err
	}
	dest, err := endpoint.Parse(uri)
	if err != nil {
		return ygg.Metadata{}, err
	}
	query := u.Query()
	link, err := c.open(ctx, u, dest, query)
	if err != nil {
		return ygg.Metadata{}, err
	}
	defer link.Close()
	// Closing unblocks the read on timeout or cancellation.
	stop := context.AfterFunc(ctx, func() { link.Close() })
	defer stop()
	meta, err := ygg.ReadMetadata(link, []byte(query.Get("password")))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Report the timeout, not the closed link it caused.
			return ygg.Metadata{}, fmt.Errorf("waiting for handshake: %w", ctxErr)
		}
		return ygg.Metadata{}, err
	}
	key := hex.EncodeToString(meta.PublicKey)
	if pins := query["key"]; len(pins) > 0 && !slices.ContainsFunc(pins, func(pin string) bool { return strings.EqualFold(pin, key) }) {
		return ygg.Metadata{}, fmt.Errorf("key %s does not match the pinned keys", key)
	}
	return meta, nil
}

// open connects the transport and returns the stream carrying the handshake.
func (c *Checker) open(ctx context.Context, u *url.URL, dest endpoint.Destination, query url.Values) (io.ReadCloser, error) {
	addr := net.JoinHostPort(dest.Host, dest.Port)
	serverName := query.Get("sni")
	if serverName == "" {
		serverName = dest.Host
	}
	switch u.Scheme {
	case "tcp":
		return c.dialer.DialContext(ctx, "tcp", addr)
	case "tls":
		d := tls.Dialer{NetDialer: c.dialer, Config: tlsConfig(serverName)}
		return d.DialContext(ctx, "tcp", addr)
	case "socks", "sockstls":
		conn, err := c.dialSOCKS(ctx, u, dest)
		if err != nil || u.Scheme == "socks" {
			return conn, err
		}
		tlsConn := tls.Client(conn, tlsConfig(serverName))
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		return tlsConn, nil
	case "quic":
		return dialQUIC(ctx, dest, serverName)
	case "ws", "wss":
		conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
			HTTPClient:   c.wsClient,
			Subprotocols: []string{"ygg-ws"},
		})
		if err != nil {
			return nil, err
		}
		return wsLink{Conn: websocket.NetConn(ctx, conn, websocket.MessageBinary), ws: conn}, nil
	default:
		return nil, fmt.Errorf("unsupported peer protocol %q", u.Scheme)
	}
}

// Yggdrasil authenticates the handshake key, not TLS certificates.
func tlsConfig(serverName string) *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, ServerName: serverName, MinVersion: tls.VersionTLS12}
}

// dialSOCKS reaches the destination through the proxy named in the URI.
func (c *Checker) dialSOCKS(ctx context.Context, u *url.URL, dest endpoint.Destination) (net.Conn, error) {
	var auth *proxy.Auth
	if u.User != nil && u.User.Username() != "" {
		password, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: password}
	}
	d, err := proxy.SOCKS5("tcp", net.JoinHostPort(dest.ProxyHost, dest.ProxyPort), auth, c.dialer)
	if err != nil {
		return nil, err
	}
	return d.(proxy.ContextDialer).DialContext(ctx, "tcp", net.JoinHostPort(dest.Host, dest.Port))
}

func dialQUIC(ctx context.Context, dest endpoint.Destination, serverName string) (io.ReadCloser, error) {
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", dest.Host)
	if err != nil {
		return nil, err
	}
	// QUIC needs TLS 1.3. Yggdrasil listeners advertise no ALPN, so offer none.
	cfg := tlsConfig(serverName)
	cfg.MinVersion = tls.VersionTLS13
	err = fmt.Errorf("no addresses for %s", dest.Host)
	for _, ip := range ips {
		var conn *quic.Conn
		conn, err = quic.DialAddr(ctx, net.JoinHostPort(ip.String(), dest.Port), cfg, nil)
		if err != nil {
			continue
		}
		stream, err := conn.OpenStreamSync(ctx)
		if err == nil {
			// The peer accepts a stream only once a frame arrives, and handles
			// connections one at a time until then. Finishing our empty send side
			// delivers it at once; the peer answers, then reads end-of-stream.
			err = stream.Close()
		}
		if err != nil {
			conn.CloseWithError(0, "")
			return nil, err
		}
		return quicLink{stream: stream, conn: conn}, nil
	}
	return nil, err
}

type quicLink struct {
	stream *quic.Stream
	conn   *quic.Conn
}

func (l quicLink) Read(p []byte) (int, error) { return l.stream.Read(p) }
func (l quicLink) Close() error               { return l.conn.CloseWithError(0, "") }

// wsLink closes without the close handshake, which could outlast the timeout.
type wsLink struct {
	net.Conn
	ws *websocket.Conn
}

func (l wsLink) Close() error { return l.ws.CloseNow() }
