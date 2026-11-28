package netguard_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/netguard"
)

func TestPublicAddr(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"8.8.8.8", "203.0.113.9", "2606:4700::1111"} {
		assert.True(t, netguard.PublicAddr(netip.MustParseAddr(s)), s)
	}
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
		"::1", "fd00::1", "fe80::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::a9fe:a9fe", "224.0.0.1", "240.0.0.1"} {
		assert.False(t, netguard.PublicAddr(netip.MustParseAddr(s)), s)
	}
}

// TestBlocksResolvedLoopback: "localhost" resolves to loopback; the check
// runs on the resolved address, so names can't smuggle private targets
// (DNS rebinding works the same way).
func TestBlocksResolvedLoopback(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	c := netguard.New().HTTPClient(5*time.Second, nil)
	for _, u := range []string{srv.URL, "http://localhost:" + port, "http://[::ffff:127.0.0.1]:" + port} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
		resp, err := c.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.ErrorIs(t, err, netguard.ErrBlocked, u)
	}
	_, err := netguard.New().DialContext(context.Background(), "tcp", srv.Listener.Addr().String(), time.Second)
	require.ErrorIs(t, err, netguard.ErrBlocked)
	assert.Equal(t, 0, hits)
}

// TestNoRedirectsNoProxy: a redirect is returned, never followed (it could
// point anywhere), and proxy environment variables are ignored.
func TestNoRedirectsNoProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	followed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			followed = true
			return
		}
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()
	c := netguard.NewAllowing(func(netip.Addr) bool { return true }).HTTPClient(5*time.Second, nil)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	require.NoError(t, err, "proxy variables ignored")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.False(t, followed)
}
