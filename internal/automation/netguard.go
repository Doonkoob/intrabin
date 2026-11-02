package automation

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned when an HTTP step targets an address it is not allowed to reach.
var ErrBlockedAddress = errors.New("destination address is not allowed")

var (
	// Never reachable, even with allow_private_network: cloud instance metadata endpoints.
	metadataAddrs = []netip.Addr{netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("fd00:ec2::254"), netip.MustParseAddr("100.100.100.200")}
	cgnat         = netip.MustParsePrefix("100.64.0.0/10")
	thisNet       = netip.MustParsePrefix("0.0.0.0/8")
)

// AddressAllowed reports whether an HTTP step may connect to ip. Private, loopback, link-local
// and carrier-grade NAT ranges require allowPrivate; metadata, multicast and unspecified
// addresses are always refused.
func AddressAllowed(ip netip.Addr, allowPrivate bool) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() || thisNet.Contains(ip) {
		return false
	}
	for _, m := range metadataAddrs {
		if ip == m {
			return false
		}
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip) {
		return allowPrivate
	}
	return true
}

// guardedClient returns an HTTP client that checks every resolved address at connect time (so
// DNS rebinding cannot bypass the check), ignores proxy settings, and does not follow redirects.
func guardedClient(allowPrivate bool, timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return ErrBlockedAddress
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !AddressAllowed(ip, allowPrivate) {
			return ErrBlockedAddress
		}
		return nil
	}}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           func(ctx context.Context, n, a string) (net.Conn, error) { return d.DialContext(ctx, n, a) },
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
