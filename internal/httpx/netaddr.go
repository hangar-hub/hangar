package httpx

import "net/netip"

// Address classes for Hangar's outbound connections (Upstreams and
// webhooks). Both decide on the address a connection is actually made to,
// after DNS resolution, so a name that resolves differently between a
// check and the connection (DNS rebinding) cannot get past them.

var (
	privatePrefixes = []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT (RFC 6598), also Tailscale
		netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments (RFC 6890)
		netip.MustParsePrefix("198.18.0.0/15"), // benchmarking (RFC 2544)
		netip.MustParsePrefix("0.0.0.0/8"),     // "this network" (RFC 1122)
		netip.MustParsePrefix("64:ff9b::/96"),  // NAT64 (RFC 6052), which can reach any IPv4 address
	}
	// metadataAddrs are cloud instance metadata services that are not
	// link-local, so IsReservedAddr refuses them by name.
	metadataAddrs = []netip.Addr{
		netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud
		netip.MustParseAddr("fd00:ec2::254"),   // AWS, over IPv6
	}
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
)

// IsPrivateAddr reports whether a is an address a multi-tenant Instance
// must not let its users reach: loopback, private, link-local (including
// cloud metadata), carrier-grade NAT, unique local, unspecified and
// multicast.
func IsPrivateAddr(a netip.Addr) bool {
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsUnspecified() ||
		a.IsMulticast() || a.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range privatePrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// IsReservedAddr reports whether a is an address Hangar never connects to
// for an Upstream or a webhook, whatever the Instance settings: link-local
// addresses (which include the cloud metadata service at 169.254.169.254),
// other known metadata services, unspecified addresses (0.0.0.0 reaches the
// local host on Linux), "this network", broadcast and multicast. No Hub,
// mirror or webhook receiver is ever served from these, and a metadata
// service hands out the host's cloud credentials.
func IsReservedAddr(a netip.Addr) bool {
	a = a.Unmap()
	if a.Is6() && nat64.Contains(a) {
		b := a.As16()
		if IsReservedAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})) {
			return true
		}
	}
	if a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() {
		return true
	}
	if a.Is4() && (a.As4()[0] == 0 || a == netip.AddrFrom4([4]byte{255, 255, 255, 255})) {
		return true
	}
	for _, m := range metadataAddrs {
		if a == m {
			return true
		}
	}
	return false
}
