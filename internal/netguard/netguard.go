// Package netguard is the SSRF guard for outbound connections to
// tenant-chosen destinations (webhook URLs, SMTP hosts, vendor APIs):
// every connection is checked against the resolved IP address at connect
// time, so DNS rebinding can't reach private space (threat model B4).
package netguard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrBlocked means the destination resolved to an address that isn't
// public unicast.
var ErrBlocked = errors.New("netguard: destination address not allowed")

// blockedPrefixes are ranges beyond what netip's predicates cover.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can reach IPv4 private space
}

// PublicAddr reports whether ip is a public unicast address.
func PublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// Guard builds guarded dialers and HTTP clients.
type Guard struct {
	allow func(netip.Addr) bool
}

// New returns a guard that allows public unicast addresses only.
func New() *Guard { return &Guard{allow: PublicAddr} }

// NewAllowing returns a guard with a custom address rule (tests that serve
// from loopback). Never use it in production wiring.
func NewAllowing(allow func(netip.Addr) bool) *Guard { return &Guard{allow: allow} }

// control runs after DNS resolution, once per address tried.
func (g *Guard) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !g.allow(ap.Addr()) {
		return ErrBlocked
	}
	return nil
}

// Dialer returns a net.Dialer that refuses non-allowed addresses.
func (g *Guard) Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: g.control}
}

// DialContext dials through the guard.
func (g *Guard) DialContext(ctx context.Context, network, addr string, timeout time.Duration) (net.Conn, error) {
	c, err := g.Dialer(timeout).DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("netguard: dial: %w", err)
	}
	return c, nil
}

// HTTPClient returns a client that dials through the guard, ignores proxy
// environment variables, never follows redirects (a redirect is returned
// as the response) and requires TLS 1.2+. tlsConf may be nil.
func (g *Guard) HTTPClient(timeout time.Duration, tlsConf *tls.Config) *http.Client {
	if tlsConf == nil {
		tlsConf = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           g.Dialer(10 * time.Second).DialContext,
			TLSClientConfig:       tlsConf,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          200,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Drain reads and discards up to n bytes of r, so the connection can be
// reused without reading an unbounded body.
func Drain(r io.Reader, n int64) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, n))
}
