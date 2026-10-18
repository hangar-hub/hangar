package upstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
)

// fakePublicDNS makes the open pool connect to the local test server for
// "hub.public.test", as if huggingface.co pointed there, and resolves that
// name to a public address for the origin check.
func fakePublicDNS(t *testing.T, g *guardedTransport, server string) {
	t.Helper()
	open := g.open.(*http.Transport)
	d := guardedDialer(true)
	open.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "hub.public.test:") {
			addr = server
		}
		return d.DialContext(ctx, network, addr)
	}
	g.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "hub.public.test" {
			return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
		}
		return lookupHost(context.Background(), host)
	}
}

func testClient(g *guardedTransport) *HTTPClient {
	return NewHTTPClient(HTTPOptions{Version: "test", Transport: g, BaseBackoff: time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }})
}

// TestUpstreamRedirectsCannotReachTheLocalNetwork checks the network
// policy: a public Upstream's redirect to a private address is refused,
// without retries, while a LAN Upstream may redirect within the LAN.
// Before the policy, the client followed any redirect anywhere, including
// to the cloud metadata service.
func TestUpstreamRedirectsCannotReachTheLocalNetwork(t *testing.T) {
	t.Parallel()
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		internalHits.Add(1)
		_, _ = w.Write([]byte(`{"id":"a/b","sha":"` + strings.Repeat("a", 40) + `"}`))
	}))
	t.Cleanup(internal.Close)
	// The internal service, under a name other than the Upstream's host.
	internalURL := strings.Replace(internal.URL, "127.0.0.1", "localhost", 1)
	var hubHits atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubHits.Add(1)
		http.Redirect(w, r, internalURL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(hub.Close)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "a", Name: "b"}

	g := newGuardedTransport()
	fakePublicDNS(t, g, strings.TrimPrefix(hub.URL, "http://"))
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(hub.URL, "http://"))
	public := Upstream{Name: "hf", BaseURL: "http://hub.public.test:" + port}
	_, err := testClient(g).RepoInfo(context.Background(), public, Credentials{}, id, "")
	require.ErrorIs(t, err, ErrBlockedAddress)
	require.ErrorContains(t, err, "a public Upstream may not redirect to the local network")
	require.Equal(t, int32(1), hubHits.Load(), "a refused address is not retried")
	require.Zero(t, internalHits.Load())

	// A LAN Upstream (a loopback address here) may redirect within the LAN.
	lan := Upstream{Name: "lan", BaseURL: hub.URL}
	info, err := testClient(newGuardedTransport()).RepoInfo(context.Background(), lan, Credentials{}, id, "")
	require.NoError(t, err)
	require.Equal(t, model.CommitSHA(strings.Repeat("a", 40)), info.SHA)
	require.Equal(t, int32(1), internalHits.Load())
}

// TestUpstreamNeverReachesReservedAddresses checks that neither an
// Upstream's own URL nor a redirect from a LAN Upstream reaches a
// link-local, metadata or unspecified address. 0.0.0.0 reaches the local
// host on Linux and macOS.
func TestUpstreamNeverReachesReservedAddresses(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(local.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(local.URL, "http://"))
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "a", Name: "b"}
	c := testClient(newGuardedTransport())
	for _, base := range []string{"http://0.0.0.0:" + port, "http://169.254.169.254", "http://[fe80::1]:" + port, "http://100.100.100.200"} {
		_, err := c.RepoInfo(context.Background(), Upstream{Name: "x", BaseURL: base}, Credentials{}, id, "")
		require.ErrorIs(t, err, ErrBlockedAddress, base)
		require.ErrorContains(t, err, "reserved address", base)
	}
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/iam/security-credentials/", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	_, err := c.RepoInfo(context.Background(), Upstream{Name: "lan", BaseURL: redirector.URL}, Credentials{}, id, "")
	require.ErrorIs(t, err, ErrBlockedAddress)
	require.Zero(t, hits.Load())
}

func TestCheckAddr(t *testing.T) {
	t.Parallel()
	for _, a := range []string{"169.254.169.254", "::ffff:169.254.169.254", "fe80::1", "0.0.0.0", "0.1.2.3", "::", "224.0.0.1", "ff02::1",
		"255.255.255.255", "100.100.100.200", "fd00:ec2::254", "64:ff9b::a9fe:a9fe"} {
		var be *blockedAddressError
		require.True(t, errors.As(checkAddr(netip.MustParseAddr(a), true), &be), a)
	}
	for _, a := range []string{"127.0.0.1", "10.0.0.1", "192.168.50.10", "100.64.0.1", "fd12::1", "::1"} {
		require.NoError(t, checkAddr(netip.MustParseAddr(a), true), a)
		require.ErrorIs(t, checkAddr(netip.MustParseAddr(a), false), ErrBlockedAddress, a)
	}
	for _, a := range []string{"8.8.8.8", "2606:4700::1111", "52.84.1.1"} {
		require.NoError(t, checkAddr(netip.MustParseAddr(a), false), a)
	}
}
