package cluster

// Cluster traffic can reach a node in two ways, chosen by the form of its cluster address:
//
//   - host:port      — a direct TLS connection to the cluster port (INTRABIN_CLUSTER_LISTEN, 7443).
//   - http(s)://URL  — a WebSocket tunnel through the node's public web address (TunnelPath), so a
//     node behind a reverse proxy or CDN on 443/80/8080 needs no extra open port.
//
// The tunnel only carries bytes. The same mutually authenticated TLS 1.3 session as on the cluster
// port runs inside it, end to end between the two nodes, so a proxy or CDN that terminates the
// outer HTTPS connection can neither read nor alter cluster traffic.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- required by the WebSocket handshake (RFC 6455), not used for security
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TunnelPath is the path, below a node's public URL, that accepts tunnelled cluster connections.
const TunnelPath = "/api/v1/cluster/tunnel"

const (
	wsGUID      = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsProtocol  = "intrabin-cluster"
	wsMaxFrame  = 1 << 24
	wsChunk     = 64 << 10
	wsPingEvery = 25 * time.Second // keeps idle tunnels open through proxies and CDNs
	maxTunnels  = 256
)

const (
	opContinuation = 0x0
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// isTunnelAddr reports whether a cluster address is a URL (reached through the WebSocket tunnel).
func isTunnelAddr(a string) bool {
	return strings.HasPrefix(a, "https://") || strings.HasPrefix(a, "http://")
}

// wsConn is a net.Conn over a WebSocket connection carrying binary frames.
type wsConn struct {
	net.Conn
	br      *bufio.Reader
	client  bool // frames sent by a client are masked
	remote  net.Addr
	onClose func()

	wmu sync.Mutex

	remaining int64
	masked    bool
	mask      [4]byte
	maskPos   int

	closeOnce sync.Once
	done      chan struct{}
}

func newWSConn(c net.Conn, br *bufio.Reader, client bool) *wsConn {
	w := &wsConn{Conn: c, br: br, client: client, done: make(chan struct{})}
	go w.keepalive()
	return w
}

func (c *wsConn) RemoteAddr() net.Addr {
	if c.remote != nil {
		return c.remote
	}
	return c.Conn.RemoteAddr()
}

func (c *wsConn) keepalive() {
	t := time.NewTicker(wsPingEvery)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			if err := c.writeFrame(opPing, nil); err != nil {
				return
			}
		}
	}
}

func (c *wsConn) Read(p []byte) (int, error) {
	for c.remaining == 0 {
		if err := c.nextFrame(); err != nil {
			return 0, err
		}
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.br.Read(p)
	if c.masked {
		for i := 0; i < n; i++ {
			p[i] ^= c.mask[c.maskPos&3]
			c.maskPos++
		}
	}
	c.remaining -= int64(n)
	return n, err
}

// nextFrame reads frame headers until a data frame with payload starts, answering control frames.
func (c *wsConn) nextFrame() error {
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return err
	}
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := int64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return err
		}
		n = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return err
		}
		n = int64(binary.BigEndian.Uint64(b[:]))
	}
	if n < 0 || n > wsMaxFrame {
		return errors.New("tunnel: frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return err
		}
	}
	switch op {
	case opBinary, opContinuation:
		c.remaining, c.masked, c.mask, c.maskPos = n, masked, mask, 0
		return nil
	case opPing, opPong, opClose:
		if n > 125 {
			return errors.New("tunnel: control frame too large")
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i&3]
			}
		}
		switch op {
		case opPing:
			return c.writeFrame(opPong, payload)
		case opClose:
			_ = c.writeFrame(opClose, nil)
			return io.EOF
		}
		return nil
	default:
		return fmt.Errorf("tunnel: unexpected frame type %d", op)
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		k := min(len(p), wsChunk)
		if err := c.writeFrame(opBinary, p[:k]); err != nil {
			return total, err
		}
		total += k
		p = p[k:]
	}
	return total, nil
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	hdr := make([]byte, 2, 14+len(payload))
	hdr[0] = 0x80 | op
	switch n := len(payload); {
	case n <= 125:
		hdr[1] = byte(n)
	case n <= 0xffff:
		hdr[1] = 126
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr[1] = 127
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	buf := hdr
	if c.client {
		buf[1] |= 0x80
		var mask [4]byte
		if _, err := rand.Read(mask[:]); err != nil {
			return err
		}
		buf = append(buf, mask[:]...)
		start := len(buf)
		buf = append(buf, payload...)
		for i := range payload {
			buf[start+i] ^= mask[i&3]
		}
	} else {
		buf = append(buf, payload...)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.Conn.Write(buf)
	return err
}

func (c *wsConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.Conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = c.writeFrame(opClose, nil)
		err = c.Conn.Close()
		if c.onClose != nil {
			c.onClose()
		}
	})
	return err
}

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + wsGUID)) // #nosec G401 -- RFC 6455 handshake
	return base64.StdEncoding.EncodeToString(h[:])
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// acceptWebSocket completes the server side of the WebSocket handshake and takes over the connection.
func acceptWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if k, err := base64.StdEncoding.DecodeString(key); r.Method != http.MethodGet || err != nil || len(k) != 16 ||
		!headerHasToken(r.Header, "Connection", "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "a WebSocket upgrade is required", http.StatusBadRequest)
		return nil, errors.New("not a websocket upgrade")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection cannot be upgraded", http.StatusInternalServerError)
		return nil, errors.New("response writer cannot be hijacked")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	// The HTTP server's read/write timeouts stay on a hijacked connection; the tunnel is long-lived.
	_ = conn.SetDeadline(time.Time{})
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + wsAccept(key) + "\r\n"
	if headerHasToken(r.Header, "Sec-WebSocket-Protocol", wsProtocol) {
		resp += "Sec-WebSocket-Protocol: " + wsProtocol + "\r\n"
	}
	if _, err := brw.WriteString(resp + "\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return newWSConn(conn, brw.Reader, false), nil
}

// tunnelURL returns the tunnel endpoint for a node's public URL.
func tunnelURL(addr string) (*url.URL, error) {
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid cluster URL %q", addr)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + TunnelPath
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u, nil
}

// dialWebSocket opens a WebSocket tunnel to a node's public URL.
func dialWebSocket(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	u, err := tunnelURL(addr)
	if err != nil {
		return nil, err
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, err
	}
	if u.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("tunnel TLS to %s: %w", u.Host, err)
		}
		conn = tc
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	var k [16]byte
	if _, err := rand.Read(k[:]); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(k[:])
	req := "GET " + u.RequestURI() + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: " + wsProtocol + "\r\n" +
		"User-Agent: intrabin/" + versionString() + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("tunnel to %s: %w", u.Host, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("tunnel to %s: HTTP %d (does the URL reach Intrabin, and does the proxy allow WebSocket?)", u.Host, resp.StatusCode)
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		conn.Close()
		return nil, fmt.Errorf("tunnel to %s: invalid WebSocket handshake", u.Host)
	}
	_ = conn.SetDeadline(time.Time{})
	return newWSConn(conn, br, true), nil
}

// dialRaw opens a byte stream to a peer's cluster address (before the cluster TLS handshake).
func dialRaw(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	if isTunnelAddr(addr) {
		return dialWebSocket(ctx, addr, timeout)
	}
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

// dialPeer opens a mutually authenticated cluster TLS connection to a peer.
func (m *Manager) dialPeer(ctx context.Context, addr, proto string, timeout time.Duration) (*tls.Conn, error) {
	raw, err := dialRaw(ctx, addr, timeout)
	if err != nil {
		return nil, err
	}
	return tlsHandshake(ctx, raw, m.clientTLS(proto), timeout)
}

func tlsHandshake(ctx context.Context, raw net.Conn, cfg *tls.Config, timeout time.Duration) (*tls.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := tls.Client(raw, cfg)
	if err := c.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return c, nil
}

// peerClient returns an HTTP client for the peer API of the node at addr.
func (m *Manager) peerClient(addr string) *http.Client {
	m.peerMu.Lock()
	defer m.peerMu.Unlock()
	if c, ok := m.peers[addr]; ok {
		return c
	}
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return m.dialPeer(ctx, addr, alpnHTTP, 5*time.Second)
		},
		MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second,
	}}
	m.peers[addr] = c
	return c
}

func (m *Manager) closePeerClients() {
	m.peerMu.Lock()
	defer m.peerMu.Unlock()
	for _, c := range m.peers {
		c.CloseIdleConnections()
	}
	m.peers = map[string]*http.Client{}
}

type tunnelAddr string

func (a tunnelAddr) Network() string { return "websocket" }
func (a tunnelAddr) String() string  { return string(a) }

// ServeTunnel accepts a cluster connection tunnelled through the public web address. clientIP is the
// caller's address as determined by the API server (trusted proxies honoured); it is used for
// logging and join rate limiting. Authentication happens inside the tunnel (cluster mTLS or a join
// token), exactly as on the cluster port.
func (m *Manager) ServeTunnel(w http.ResponseWriter, r *http.Request, clientIP string) {
	m.mu.Lock()
	mx := m.mux
	m.mu.Unlock()
	if mx == nil {
		http.Error(w, "this node is not part of a cluster", http.StatusNotFound)
		return
	}
	select {
	case m.tunnelSlots <- struct{}{}:
	default:
		http.Error(w, "too many cluster tunnels", http.StatusServiceUnavailable)
		return
	}
	ws, err := acceptWebSocket(w, r)
	if err != nil {
		<-m.tunnelSlots
		return
	}
	ws.remote = tunnelAddr(net.JoinHostPort(clientIP, "0"))
	ws.onClose = func() { <-m.tunnelSlots }
	if !mx.serve(ws) {
		ws.Close()
	}
}

// probeAddress proves that addr reaches node id: a cluster TLS handshake succeeds and the server
// presents that node's certificate.
func (m *Manager) probeAddress(ctx context.Context, id, addr string) error {
	c, err := m.dialPeer(ctx, addr, alpnHTTP, 10*time.Second)
	if err != nil {
		return fmt.Errorf("%w: %s is not reachable from this node: %v", ErrBadRequest, addr, err)
	}
	defer c.Close()
	certs := c.ConnectionState().PeerCertificates
	if len(certs) == 0 || certs[0].Subject.CommonName != id {
		return fmt.Errorf("%w: %s reaches a different node", ErrBadRequest, addr)
	}
	return nil
}

// normalizeAdvertise trims a cluster address and canonicalises URLs.
func normalizeAdvertise(a string) string {
	a = strings.TrimSpace(a)
	if !isTunnelAddr(strings.ToLower(a)) {
		return a
	}
	u, err := url.Parse(a)
	if err != nil {
		return a
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String()
}

// validAdvertise checks a cluster address: host:port of the cluster port, or the node's public
// http(s) URL.
func validAdvertise(a string) error {
	if len(a) > 300 {
		return fmt.Errorf("%w: cluster address is too long", ErrBadRequest)
	}
	var host, port string
	if isTunnelAddr(a) {
		u, err := url.Parse(a)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("%w: a URL cluster address must look like https://intrabin.example.com", ErrBadRequest)
		}
		host, port = u.Hostname(), u.Port()
	} else {
		h, p, err := net.SplitHostPort(a)
		if err != nil || h == "" {
			return fmt.Errorf("%w: cluster address must be host:port or the node's https:// URL", ErrBadRequest)
		}
		if p == "" {
			return fmt.Errorf("%w: invalid port", ErrBadRequest)
		}
		host, port = h, p
	}
	if port != "" {
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("%w: invalid port", ErrBadRequest)
		}
	}
	if host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("%w: cluster address must be reachable by peers, not a wildcard", ErrBadRequest)
	}
	return nil
}

// advertiseHost returns the host part of a cluster address (for the node certificate).
func advertiseHost(a string) string {
	if isTunnelAddr(a) {
		if u, err := url.Parse(a); err == nil {
			return u.Hostname()
		}
		return ""
	}
	h, _, _ := net.SplitHostPort(a)
	return h
}
