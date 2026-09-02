package httpapi

import (
	"net/http/httptest"
	"testing"
)

func TestParseTrustedProxies(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"127.0.0.1", 1, false},
		{"10.0.0.0/8", 1, false},
		{" 127.0.0.1 , ::1 ,10.0.0.0/8 ", 3, false},
		{"127.0.0.1,,::1", 2, false},
		{"not-an-ip", 0, true},
		{"10.0.0.0/99", 0, true},
		{"example.com", 0, true},
	} {
		got, err := ParseTrustedProxies(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseTrustedProxies(%q) = %v, want error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTrustedProxies(%q): %v", tc.raw, err)
			continue
		}
		if len(got) != tc.want {
			t.Errorf("ParseTrustedProxies(%q) = %d prefixes, want %d", tc.raw, len(got), tc.want)
		}
	}
}

// A CIDR written with host bits set must still match its block; "10.0.0.7/8"
// left unmasked would never contain anything.
func TestParseTrustedProxiesMasksHostBits(t *testing.T) {
	prefixes, err := ParseTrustedProxies("10.0.0.7/8")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := parseClientAddr("10.9.9.9")
	if !ok {
		t.Fatal("parseClientAddr failed")
	}
	if !isTrustedProxy(addr, prefixes) {
		t.Error("10.9.9.9 should be inside 10.0.0.7/8 once masked")
	}
}

// clientKeyFor builds a request with the given peer and X-Forwarded-For values
// and reports the rate-limiting identity the server derives from it.
func clientKeyFor(t *testing.T, trusted string, peer string, xff ...string) string {
	t.Helper()
	prefixes, err := ParseTrustedProxies(trusted)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{deps: Deps{TrustedProxies: prefixes}}
	req := httptest.NewRequest("POST", "/api/v1/session", nil)
	req.RemoteAddr = peer
	for _, v := range xff {
		req.Header.Add("X-Forwarded-For", v)
	}
	return s.clientKey(req)
}

// With no configuration the socket peer is the only identity, exactly as
// before trusted proxies existed — a forwarded header must change nothing.
func TestClientKeyIgnoresForwardedWithoutConfiguration(t *testing.T) {
	got := clientKeyFor(t, "", "203.0.113.7:5000", "198.51.100.9")
	if got != "203.0.113.7" {
		t.Errorf("clientKey = %q, want the socket peer 203.0.113.7", got)
	}
}

// The header is honored only when the peer is actually a configured proxy.
// This is the spoofing case: a direct client sending X-Forwarded-For must not
// be able to choose its own rate-limit bucket.
func TestClientKeyIgnoresForwardedFromUntrustedPeer(t *testing.T) {
	got := clientKeyFor(t, "127.0.0.1", "203.0.113.7:5000", "198.51.100.9")
	if got != "203.0.113.7" {
		t.Errorf("clientKey = %q, want the socket peer 203.0.113.7", got)
	}
}

func TestClientKeyUsesForwardedFromTrustedProxy(t *testing.T) {
	got := clientKeyFor(t, "127.0.0.1", "127.0.0.1:5000", "198.51.100.9")
	if got != "198.51.100.9" {
		t.Errorf("clientKey = %q, want the forwarded client 198.51.100.9", got)
	}
}

// A client that prepends its own hops must not be able to reach past the
// proxy: the walk is right to left and stops at the first untrusted address.
func TestClientKeyIgnoresClientPrependedHops(t *testing.T) {
	got := clientKeyFor(t, "127.0.0.1", "127.0.0.1:5000", "1.1.1.1, 2.2.2.2, 198.51.100.9")
	if got != "198.51.100.9" {
		t.Errorf("clientKey = %q, want the rightmost untrusted hop 198.51.100.9", got)
	}
}

// Two clients behind one proxy must land in different buckets — the whole
// point of the setting.
func TestClientKeySeparatesClientsBehindOneProxy(t *testing.T) {
	a := clientKeyFor(t, "127.0.0.1", "127.0.0.1:5000", "198.51.100.9")
	b := clientKeyFor(t, "127.0.0.1", "127.0.0.1:5000", "198.51.100.10")
	if a == b {
		t.Errorf("two clients behind one proxy shared a bucket: %q", a)
	}
}

// A chain of nothing but trusted proxies vouches for no client, so the peer
// stays the identity rather than a proxy's own address being used as one.
func TestClientKeyFallsBackWhenChainIsAllProxies(t *testing.T) {
	got := clientKeyFor(t, "127.0.0.1,10.0.0.0/8", "127.0.0.1:5000", "10.1.2.3, 10.4.5.6")
	if got != "127.0.0.1" {
		t.Errorf("clientKey = %q, want the peer 127.0.0.1", got)
	}
}

// A malformed entry inside the segment being unwound means the chain cannot be
// accounted for; falling back to the peer is the safe direction (a shared
// bucket, never a client-chosen one).
func TestClientKeyFallsBackOnMalformedChain(t *testing.T) {
	got := clientKeyFor(t, "127.0.0.1", "127.0.0.1:5000", "198.51.100.9, not-an-ip")
	if got != "127.0.0.1" {
		t.Errorf("clientKey = %q, want the peer 127.0.0.1", got)
	}
}

// Garbage to the LEFT of the winning hop is client-written and never read.
// Honoring it would let one header collapse every client back onto the shared
// peer bucket, which is the lockout this whole mechanism exists to prevent.
func TestClientKeyIgnoresGarbageLeftOfTheWinningHop(t *testing.T) {
	got := clientKeyFor(t, "127.0.0.1", "127.0.0.1:5000", "not-an-ip, 198.51.100.9")
	if got != "198.51.100.9" {
		t.Errorf("clientKey = %q, want 198.51.100.9: client-written junk must not "+
			"force a fallback to the shared bucket", got)
	}
}

// An absurdly long chain is a hostile header, not a deployment.
func TestClientKeyFallsBackOnOverlongChain(t *testing.T) {
	long := ""
	for i := 0; i <= maxForwardedHops; i++ {
		if i > 0 {
			long += ", "
		}
		long += "198.51.100.9"
	}
	got := clientKeyFor(t, "127.0.0.1", "127.0.0.1:5000", long)
	if got != "127.0.0.1" {
		t.Errorf("clientKey = %q, want the peer 127.0.0.1", got)
	}
}

// Repeated headers are one chain; a client must not escape the walk by
// splitting its hops across several X-Forwarded-For lines.
func TestClientKeyJoinsRepeatedHeaders(t *testing.T) {
	got := clientKeyFor(t, "127.0.0.1", "127.0.0.1:5000", "1.1.1.1", "198.51.100.9")
	if got != "198.51.100.9" {
		t.Errorf("clientKey = %q, want 198.51.100.9", got)
	}
}

// A dual-stack listener reports an IPv4 client as ::ffff:a.b.c.d; that must be
// the same identity as the plain IPv4 form, or the backoff is halved.
func TestClientKeyUnmapsIPv4MappedAddresses(t *testing.T) {
	mapped := clientKeyFor(t, "", "[::ffff:203.0.113.7]:5000")
	plain := clientKeyFor(t, "", "203.0.113.7:5000")
	if mapped != plain {
		t.Errorf("mapped %q != plain %q", mapped, plain)
	}
}

func TestClientKeyHandlesIPv6Peer(t *testing.T) {
	got := clientKeyFor(t, "", "[2001:db8::1]:5000")
	if got != "2001:db8::1" {
		t.Errorf("clientKey = %q, want 2001:db8::1", got)
	}
}

// Trusting a proxy must not turn a forwarded address into proof of locality:
// first-run setup authorization reads the accepted socket, never the header.
func TestForwardedLoopbackDoesNotAuthorizeSetup(t *testing.T) {
	req := httptest.NewRequest("PUT", "/api/v1/session/password", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	if !peerIsLoopback(req) {
		t.Error("peerIsLoopback must read the socket, not the forwarded chain")
	}

	remote := httptest.NewRequest("PUT", "/api/v1/session/password", nil)
	remote.RemoteAddr = "203.0.113.7:5000"
	remote.Header.Set("X-Forwarded-For", "127.0.0.1")
	if peerIsLoopback(remote) {
		t.Error("a forwarded 127.0.0.1 must never make a remote peer look local")
	}
}
