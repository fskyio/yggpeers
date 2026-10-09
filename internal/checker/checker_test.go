package checker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/quic-go/quic-go"

	"yggpeers/internal/config"
	"yggpeers/internal/protocol"
	"yggpeers/internal/ygg"
)

var peerPrivate = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
var peerKey = hex.EncodeToString(peerPrivate.Public().(ed25519.PublicKey))

// yggPeer answers each link like Yggdrasil: it sends its handshake, then reads
// the dialer's. It reports how many bytes the dialer sent on the link.
type yggPeer struct {
	handshake []byte
	received  chan int64
}

func newYggPeer(t *testing.T, password string) *yggPeer {
	t.Helper()
	meta := ygg.Metadata{MinorVersion: 5, PublicKey: peerPrivate.Public().(ed25519.PublicKey)}
	handshake, err := meta.Encode(peerPrivate, []byte(password))
	if err != nil {
		t.Fatal(err)
	}
	return &yggPeer{handshake: handshake, received: make(chan int64, 1)}
}

func (p *yggPeer) serve(link io.ReadWriteCloser) {
	defer link.Close()
	if _, err := link.Write(p.handshake); err != nil {
		p.received <- -1
		return
	}
	n, _ := io.Copy(io.Discard, link)
	p.received <- n
}

func (p *yggPeer) serveWebSocket() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ygg", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"ygg-ws"}})
		if err != nil {
			return
		}
		if c.Subprotocol() != "ygg-ws" {
			c.Close(websocket.StatusPolicyViolation, "client must speak the ygg-ws subprotocol")
			return
		}
		p.serve(websocket.NetConn(context.Background(), c, websocket.MessageBinary))
	})
	return mux
}

func serveTCP(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()
	return ln.Addr().String()
}

func serveQUIC(t *testing.T, cfg *tls.Config, handle func(*quic.Stream)) string {
	t.Helper()
	ln, err := quic.ListenAddr("127.0.0.1:0", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				if stream, err := conn.AcceptStream(context.Background()); err == nil {
					handle(stream)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// serveSOCKS is a minimal no-authentication SOCKS5 proxy. It records each
// requested destination and hands the tunnel to handle as if it had arrived.
func serveSOCKS(t *testing.T, handle func(net.Conn)) (string, chan string) {
	t.Helper()
	requested := make(chan string, 1)
	addr := serveTCP(t, func(conn net.Conn) {
		destination, err := socksConnect(conn)
		if err != nil {
			conn.Close()
			return
		}
		requested <- destination
		handle(conn)
	})
	return addr, requested
}

func socksConnect(conn net.Conn) (string, error) {
	var err error
	read := func(n int) []byte {
		b := make([]byte, n)
		if err == nil {
			_, err = io.ReadFull(conn, b)
		}
		return b
	}
	read(int(read(2)[1])) // version and offered methods
	if _, werr := conn.Write([]byte{5, 0}); err == nil {
		err = werr
	}
	request := read(4) // version, command, reserved, address type
	if err == nil && request[3] != 3 {
		return "", fmt.Errorf("address type %d, want a domain name", request[3])
	}
	host := read(int(read(1)[0]))
	port := binary.BigEndian.Uint16(read(2))
	if err != nil {
		return "", err
	}
	_, err = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	return net.JoinHostPort(string(host), strconv.Itoa(int(port))), err
}

func serverTLS(t *testing.T, protos ...string) *tls.Config {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   protos,
	}
}

func check(t *testing.T, timeout time.Duration, uri string) protocol.Result {
	t.Helper()
	result, err := New(config.ProbeConfig{DialTimeout: timeout}).Check(context.Background(), protocol.Target{PeerID: 42, URI: uri})
	if err != nil || result.PeerID != 42 || !result.Valid() {
		t.Fatalf("check %s: result=%+v err=%v", uri, result, err)
	}
	return result
}

func TestHandshakeOverEveryTransport(t *testing.T) {
	cert := serverTLS(t)
	const destination = "peer.onion:1234"
	for _, tc := range []struct {
		name  string
		start func(p *yggPeer) (uri string, requested chan string)
	}{
		{"tcp", func(p *yggPeer) (string, chan string) {
			return "tcp://" + serveTCP(t, func(c net.Conn) { p.serve(c) }), nil
		}},
		{"tls", func(p *yggPeer) (string, chan string) {
			return "tls://" + serveTCP(t, func(c net.Conn) { p.serve(tls.Server(c, cert)) }), nil
		}},
		{"quic", func(p *yggPeer) (string, chan string) {
			return "quic://" + serveQUIC(t, cert, func(s *quic.Stream) { p.serve(s) }), nil
		}},
		{"ws", func(p *yggPeer) (string, chan string) {
			server := httptest.NewServer(p.serveWebSocket())
			t.Cleanup(server.Close)
			return "ws://" + server.Listener.Addr().String() + "/ygg", nil
		}},
		{"wss", func(p *yggPeer) (string, chan string) {
			server := httptest.NewTLSServer(p.serveWebSocket())
			t.Cleanup(server.Close)
			return "wss://" + server.Listener.Addr().String() + "/ygg", nil
		}},
		{"socks", func(p *yggPeer) (string, chan string) {
			proxy, requested := serveSOCKS(t, func(c net.Conn) { p.serve(c) })
			return "socks://" + proxy + "/" + destination, requested
		}},
		{"sockstls", func(p *yggPeer) (string, chan string) {
			proxy, requested := serveSOCKS(t, func(c net.Conn) { p.serve(tls.Server(c, cert)) })
			return "sockstls://" + proxy + "/" + destination, requested
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := newYggPeer(t, "")
			uri, requested := tc.start(peer)
			result := check(t, 5*time.Second, uri+"?key="+peerKey)
			if result.Status != protocol.Up || result.Key != peerKey || result.ProtocolVersion != "0.5" {
				t.Fatalf("result = %+v, want up from %s with protocol 0.5", result, peerKey)
			}
			select {
			case n := <-peer.received:
				if n != 0 {
					t.Fatalf("probe sent %d bytes on the link; it must only listen", n)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("link stayed open after the check")
			}
			if requested != nil {
				if got := <-requested; got != destination {
					t.Fatalf("proxy asked for %q, want %q", got, destination)
				}
			}
		})
	}
}

func TestNonYggdrasilServicesAreDown(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	web := httptest.NewServer(ok)
	defer web.Close()
	secureWeb := httptest.NewTLSServer(ok)
	defer secureWeb.Close()
	h3 := serveQUIC(t, serverTLS(t, "h3"), func(*quic.Stream) {})
	for _, uri := range []string{
		"tcp://" + web.Listener.Addr().String(),
		"tls://" + secureWeb.Listener.Addr().String(),
		"quic://" + h3,
		"ws://" + web.Listener.Addr().String() + "/",
		"wss://" + secureWeb.Listener.Addr().String() + "/",
	} {
		t.Run(uri[:strings.Index(uri, ":")], func(t *testing.T) {
			if status := check(t, 300*time.Millisecond, uri).Status; status != protocol.Down {
				t.Fatalf("status = %s, want down", status)
			}
		})
	}
}

func TestPinnedKeysAndPasswords(t *testing.T) {
	other, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherKey := hex.EncodeToString(other)
	for _, tc := range []struct {
		name, password, query string
		want                  protocol.Status
	}{
		{"pin in uppercase hex", "", "?key=" + strings.ToUpper(peerKey), protocol.Up},
		{"one of several pins", "", "?key=" + otherKey + "&key=" + peerKey, protocol.Up},
		{"different pinned key", "", "?key=" + otherKey, protocol.Down},
		{"matching password", "secret", "?password=secret", protocol.Up},
		{"missing password", "secret", "", protocol.Down},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := newYggPeer(t, tc.password)
			uri := "tcp://" + serveTCP(t, func(c net.Conn) { peer.serve(c) }) + tc.query
			if status := check(t, 5*time.Second, uri).Status; status != tc.want {
				t.Fatalf("status = %s, want %s", status, tc.want)
			}
		})
	}
}

func TestAssignmentCancellationIsNotDown(t *testing.T) {
	accepted := make(chan struct{}, 1)
	addr := serveTCP(t, func(conn net.Conn) {
		accepted <- struct{}{}
		io.Copy(io.Discard, conn)
	})
	c := New(config.ProbeConfig{DialTimeout: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result protocol.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := c.Check(ctx, protocol.Target{PeerID: 42, URI: "tcp://" + addr})
		done <- outcome{result, err}
	}()
	<-accepted
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.result.Status != "" {
			t.Fatalf("cancellation produced an observation: %+v, %v", got.result, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("check ignored cancellation while waiting for the handshake")
	}
}
