package upstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hangar-hub/hangar/internal/httpx"
)

// ErrBlockedAddress means the Upstream client refused to connect to an
// address its network policy does not allow (06-upstreams-and-pulls.md
// "Network policy").
var ErrBlockedAddress = errors.New("the Instance's network policy refused the request to the Upstream")

// blockedAddressError names the refused address. It never names the URL,
// which can be a presigned redirect target.
type blockedAddressError struct {
	addr   netip.Addr
	reason string
}

func (e *blockedAddressError) Error() string {
	return ErrBlockedAddress.Error() + ": " + e.addr.String() + " is " + e.reason
}

func (e *blockedAddressError) Is(target error) bool { return target == ErrBlockedAddress }

// The network policy for Upstreams. An admin chooses each Upstream's URL
// and may point it at the LAN (another Hangar, a corporate mirror), so:
//
//   - Hangar connects to an Upstream's own host wherever it resolves,
//     except reserved addresses (link-local, which includes the cloud
//     metadata service, other metadata services, unspecified and multicast;
//     httpx.IsReservedAddr), which it never connects to.
//   - A redirect to another host may reach a private address only when the
//     Upstream's own host is private itself (resolves to a loopback,
//     private, carrier-grade NAT or unique local address), as a LAN Hangar
//     redirecting to its LAN object store does. A public Upstream, such as
//     huggingface.co, cannot redirect Hangar into the local network.
//
// Each check is made on the address the connection is made to, after DNS
// resolution, so DNS rebinding between a check and the connection gets
// nowhere. Requests to the Upstream's host and redirects that may reach
// private addresses use one connection pool, redirects that may not use
// another, so a pooled connection to a private address is never reused for
// a request that may not reach it. A proxy from HTTP_PROXY or HTTPS_PROXY is
// trusted: Hangar checks the proxy's address, and the proxy decides the
// rest.

type originKey struct{}

// origin is the Upstream host a request chain started at. http.Client
// gives every redirect the context of the first request.
type origin struct {
	host string
	once sync.Once
	priv bool
	err  error
}

func withOrigin(ctx context.Context, rawURL string) context.Context {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, originKey{}, &origin{host: strings.ToLower(u.Hostname())})
}

// guardedTransport sends each request through the pool its policy allows.
type guardedTransport struct {
	// open may reach private addresses; public may not. Neither reaches a
	// reserved address.
	open, public http.RoundTripper
	lookup       func(ctx context.Context, host string) ([]netip.Addr, error)
}

// newGuardedTransport returns the default Upstream transport, which waits
// at most headerTimeout for response headers.
func newGuardedTransport(headerTimeout time.Duration) *guardedTransport {
	mk := func(allowPrivate bool) *http.Transport {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DialContext = guardedDialer(allowPrivate).DialContext
		t.ResponseHeaderTimeout = headerTimeout
		return t
	}
	return &guardedTransport{open: mk(true), public: mk(false), lookup: lookupHost}
}

// guardedDialer returns a dialer that refuses reserved addresses and, unless
// allowPrivate, private ones.
func guardedDialer(allowPrivate bool) *net.Dialer {
	return &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		return checkAddr(ap.Addr(), allowPrivate)
	}}
}

func checkAddr(a netip.Addr, allowPrivate bool) error {
	switch {
	case httpx.IsReservedAddr(a):
		return &blockedAddressError{addr: a, reason: "a link-local, cloud metadata or other reserved address"}
	case !allowPrivate && httpx.IsPrivateAddr(a):
		return &blockedAddressError{addr: a, reason: "a private address, and a public Upstream may not redirect to the local network"}
	}
	return nil
}

func (g *guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	o, _ := req.Context().Value(originKey{}).(*origin)
	if o == nil || strings.EqualFold(req.URL.Hostname(), o.host) {
		return g.open.RoundTrip(req)
	}
	o.once.Do(func() { o.priv, o.err = g.hostIsPrivate(req.Context(), o.host) })
	if o.err != nil {
		return nil, o.err
	}
	if o.priv {
		return g.open.RoundTrip(req)
	}
	return g.public.RoundTrip(req)
}

// hostIsPrivate reports whether an Upstream host is on a private network:
// an IP literal or a name with at least one private address.
func (g *guardedTransport) hostIsPrivate(ctx context.Context, host string) (bool, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return httpx.IsPrivateAddr(a), nil
	}
	addrs, err := g.lookup(ctx, host)
	if err != nil {
		return false, err
	}
	for _, a := range addrs {
		if httpx.IsPrivateAddr(a) {
			return true, nil
		}
	}
	return false, nil
}

// lookupHost resolves a host name to its IPv4 and IPv6 addresses.
func lookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}
