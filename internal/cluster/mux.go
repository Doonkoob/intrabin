package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

const (
	alpnRaft = "intrabin-raft"
	alpnHTTP = "http/1.1"
)

// chanListener is a net.Listener fed from a channel.
type chanListener struct {
	addr   net.Addr
	ch     chan net.Conn
	once   sync.Once
	closed chan struct{}
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{addr: addr, ch: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

func (l *chanListener) deliver(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.closed:
		c.Close()
	}
}

// mux accepts TLS connections on the cluster port and routes them by ALPN: Raft RPC streams
// (mutually authenticated) or the cluster HTTP API (join, ping, result shipping, write forwarding).
type mux struct {
	inner net.Listener
	raft  *chanListener
	http  *chanListener
	log   *slog.Logger
}

func newMux(inner net.Listener, cfg *tls.Config, advertise net.Addr, log *slog.Logger) *mux {
	m := &mux{inner: tls.NewListener(inner, cfg), raft: newChanListener(advertise), http: newChanListener(inner.Addr()), log: log}
	go m.loop()
	return m
}

func (m *mux) loop() {
	for {
		c, err := m.inner.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				m.raft.Close()
				m.http.Close()
				return
			}
			continue
		}
		go m.route(c.(*tls.Conn))
	}
}

func (m *mux) route(c *tls.Conn) {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.HandshakeContext(ctx); err != nil {
		c.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	st := c.ConnectionState()
	switch st.NegotiatedProtocol {
	case alpnRaft:
		if len(st.VerifiedChains) == 0 {
			m.log.Warn("rejected raft connection without a valid cluster certificate", "remote", c.RemoteAddr().String())
			c.Close()
			return
		}
		m.raft.deliver(c)
	default:
		m.http.deliver(c)
	}
}

func (m *mux) Close() error { return m.inner.Close() }

// streamLayer implements raft.StreamLayer over mutually authenticated TLS.
type streamLayer struct {
	*chanListener
	client func(proto string) *tls.Config
}

func (s *streamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	return tls.DialWithDialer(d, "tcp", string(address), s.client(alpnRaft))
}
