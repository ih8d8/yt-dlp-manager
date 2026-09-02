package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Client identification for login rate limiting.
//
// The socket peer is the only identity that cannot be chosen by the client, so
// it stays the default. Behind a reverse proxy, though, every request carries
// the PROXY's address: without the opt-in below, one shared bucket holds every
// client, and one person's failed guesses throttle everyone else. The
// documented remote deployment (README, SECURITY.md) is exactly that shape.
//
// This is an opt-in convenience, not a load-bearing control. loginMaxDelay is
// deliberately short, so leaving it unconfigured costs a shared minute at
// worst; configuring it is what makes the limiter accurate per client.
//
// Forwarded headers are trusted ONLY when the peer is a configured trusted
// proxy, and only as far as the chain is corroborated: the walk runs right to
// left and stops at the first address that is not itself a trusted proxy, so
// hops a client prepended itself are never reached. With no configuration this
// file changes nothing.
//
// Deliberately NOT extended to first-run setup authorization: peerIsLoopback in
// auth_handlers.go must keep reading the accepted socket. A proxy that appends
// to X-Forwarded-For rather than replacing it lets any client that can reach it
// put "127.0.0.1" in the chain, and treating that as locality would hand the
// administrator account to whoever asked first.

// maxForwardedHops bounds the chain this walks. A real deployment has a handful
// of hops; anything past this is a malformed or hostile header, and falling
// back to the peer is the safe direction (it shares a bucket, it never trusts).
const maxForwardedHops = 32

// peerAddr returns the IP of the accepted connection.
func peerAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return parseClientAddr(host)
}

// parseClientAddr normalizes one textual address. IPv4-mapped IPv6 is unmapped
// so a client reaching a dual-stack listener is the same identity either way,
// and any zone suffix is dropped because it names a local interface rather than
// the peer.
func parseClientAddr(host string) (netip.Addr, bool) {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// isTrustedProxy reports whether a is one of the configured proxies.
func isTrustedProxy(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientKey identifies the client for login rate limiting.
//
// It returns the socket peer unless that peer is a configured trusted proxy and
// the forwarded chain names a client behind it. Every failure mode — no
// configuration, an untrusted peer, a missing, malformed or over-long header, a
// chain consisting only of trusted proxies — falls back to the peer, which is
// the behavior this server had before trusted proxies existed.
func (s *Server) clientKey(r *http.Request) string {
	peer, ok := peerAddr(r)
	if !ok {
		// Unparseable RemoteAddr: use it verbatim rather than collapsing every
		// such request onto one key.
		return r.RemoteAddr
	}
	if len(s.deps.TrustedProxies) == 0 || !isTrustedProxy(peer, s.deps.TrustedProxies) {
		return peer.String()
	}
	if a, found := forwardedClient(r, s.deps.TrustedProxies); found {
		return a.String()
	}
	return peer.String()
}

// forwardedClient walks X-Forwarded-For from right to left and returns the
// first address that is not itself a trusted proxy — the closest hop the
// infrastructure actually vouches for.
//
// Left-to-right would read the FIRST entry, which is whatever the original
// client sent and therefore fully attacker-chosen.
//
// A malformed entry encountered DURING the walk — that is, inside the run of
// trusted proxies the chain is being unwound through — abandons it: a suffix
// this cannot account for is not one to draw an identity from. Garbage further
// left is simply never read, and that asymmetry is deliberate. Anything to the
// left of the winning hop was written by the client, so treating it as a reason
// to give up would hand an attacker a one-header way to collapse everyone back
// onto the shared peer bucket — reinstating the lockout this exists to prevent.
func forwardedClient(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return netip.Addr{}, false
	}
	chain := make([]string, 0, maxForwardedHops)
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if len(chain) >= maxForwardedHops {
				return netip.Addr{}, false
			}
			chain = append(chain, part)
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		a, ok := parseClientAddr(chain[i])
		if !ok {
			return netip.Addr{}, false
		}
		if isTrustedProxy(a, trusted) {
			continue
		}
		return a, true
	}
	return netip.Addr{}, false
}

// ParseTrustedProxies turns a comma-separated list of IP addresses and CIDR
// blocks into prefixes. A bare address becomes a single-host prefix. It is
// exported so the command layer can validate the operator's value at startup
// and refuse to start on a typo, rather than silently trusting nothing.
func ParseTrustedProxies(raw string) ([]netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if strings.Contains(p, "/") {
			pref, err := netip.ParsePrefix(p)
			if err != nil {
				return nil, err
			}
			// Masked() clears host bits so "10.0.0.7/8" behaves as written
			// rather than never matching.
			out = append(out, pref.Masked())
			continue
		}
		a, ok := parseClientAddr(p)
		if !ok {
			return nil, &net.AddrError{Err: "not an IP address or CIDR block", Addr: p}
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}
