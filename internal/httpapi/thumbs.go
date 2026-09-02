package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Thumbnail proxy: fetches an item's probed thumbnail exactly once, caches
// it under the state directory, and serves it to authenticated admins.
//
// Safety posture (v1):
//   - the URL is never client-supplied; it comes only from our own yt-dlp
//     probe output and stays attached to a live item id,
//   - https only, redirects validated with the same rules,
//   - dial control rejects any address that is not public unicast
//     (loopback/private/link-local/multicast blocked — basic SSRF guard),
//   - 6s timeout, 512 KiB cap, content sniffing restricted to jpeg/png/webp,
//   - served bytes are bounded by construction; no response headers from the
//     origin are forwarded.

const (
	maxThumbBytes     = 512 << 10
	thumbFetchTimeout = 6 * time.Second
)

// allowedImageTypes is a set, not a type-to-extension table: cache entries are
// always named "<id>.img" and the type is re-sniffed on the way out, so an
// extension per type would be dead data implying a naming scheme that does not
// exist. Membership is the whole contract — anything not listed is refused.
var allowedImageTypes = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
	"image/webp": {},
}

// downloadIDRe accepts both the original 8-character IDs already persisted by
// older releases and the 32-character IDs minted now, with headroom for a
// longer future format. It exists because item ids reach the filesystem:
// http.ServeMux matches on the ESCAPED path and unescapes only the captured
// segment, so "%2F" survives routing and becomes a real separator inside
// PathValue. Without this guard filepath.Join walks straight out of the state
// directory.
var downloadIDRe = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

func validDownloadID(id string) bool { return downloadIDRe.MatchString(id) }

// thumbCachePath resolves an item's cached thumbnail and verifies the result
// is still inside the thumbs directory. Both the id check and the containment
// check are kept: the first is the rule, the second is the backstop.
func (s *Server) thumbCachePath(id string) (dir, file string, ok bool) {
	if s.deps.StateDir == "" || !validDownloadID(id) {
		return "", "", false
	}
	dir = filepath.Join(s.deps.StateDir, "thumbs")
	file = filepath.Join(dir, id+".img")
	if filepath.Dir(file) != dir {
		return "", "", false
	}
	return dir, file, true
}

// EvictThumbnail removes an item's cached thumbnail. It is called from the
// removal path, where the id is already known-good, so the cache no longer
// depends on someone requesting a thumbnail for a dead item to be cleaned up.
func (s *Server) EvictThumbnail(id string) {
	if _, file, ok := s.thumbCachePath(id); ok {
		_ = os.Remove(file)
	}
}

// sweepThumbnails drops cached images with no surviving item. Eviction on
// removal only fires while something is watching the event stream, so without
// a startup sweep the cache would still grow across restarts. Errors are
// ignored throughout: a stale thumbnail is never worth failing a boot for.
func (s *Server) sweepThumbnails() {
	if s.deps.StateDir == "" || s.deps.Manager == nil {
		return
	}
	dir := filepath.Join(s.deps.StateDir, "thumbs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	live := make(map[string]struct{})
	for _, it := range s.deps.Manager.List() {
		live[it.ID] = struct{}{}
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		id, ok := strings.CutSuffix(name, ".img")
		if !ok || !validDownloadID(id) {
			// Not something this cache wrote; leave it alone rather than
			// deleting a file we cannot account for.
			continue
		}
		if _, alive := live[id]; !alive {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validDownloadID(id) {
		writeError(w, r, http.StatusNotFound, codeNotFound, "no such download")
		return
	}
	it, ok := s.deps.Manager.Get(id)
	if !ok {
		// Item gone: drop any cached artifact lazily.
		s.EvictThumbnail(id)
		writeError(w, r, http.StatusNotFound, codeNotFound, "no such download")
		return
	}
	dir, cached, pathOK := s.thumbCachePath(id)
	if it.ThumbURL == "" || !pathOK {
		writeError(w, r, http.StatusNotFound, codeNotFound, "no thumbnail available")
		return
	}

	// Bounded like the network fetch: a cache file is only ever written by
	// this process, but reading it unbounded would make any tampering with
	// the cache directory an unbounded allocation.
	if data, err := readCappedFile(cached, maxThumbBytes); err == nil {
		s.serveThumbBytes(w, r, data)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "thumbnail cache unreadable")
		return
	}

	data, ctype, err := s.fetchThumbnailOnce(r.Context(), id, it.ThumbURL)
	if err != nil {
		writeError(w, r, http.StatusBadGateway, codeNotFound, "thumbnail unavailable")
		return
	}
	if err := os.MkdirAll(dir, 0o700); err == nil {
		// A fixed "<id>.tmp" name races: two concurrent first-time requests
		// for the same download would write the same path and one rename
		// could publish a half-written file.
		tmp := ""
		if f, terr := os.CreateTemp(dir, ".thumb-*.tmp"); terr == nil {
			tmp = f.Name()
			_, werr := f.Write(data)
			cerr := f.Close()
			if werr != nil || cerr != nil || os.Chmod(tmp, 0o600) != nil {
				_ = os.Remove(tmp)
				tmp = ""
			}
		}
		if tmp != "" {
			_ = os.Rename(tmp, cached)
			_ = os.Chmod(cached, 0o600)
		}
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

func (s *Server) serveThumbBytes(w http.ResponseWriter, r *http.Request, data []byte) {
	ct := http.DetectContentType(data[:min(512, len(data))])
	if _, ok := allowedImageTypes[ct]; ok {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

// thumbClient is process-wide on purpose. A per-request http.Transport cannot
// reuse connections (a fresh TLS handshake per thumbnail) and, worse, the
// connection it parks in its idle pool outlives the transport that owns it —
// nothing closes it, because the transport itself is garbage with no
// IdleConnTimeout. One shared client with explicit bounds fixes both.
var thumbClient = &http.Client{
	Timeout: thumbFetchTimeout,
	Transport: &http.Transport{
		DialContext: safeDialContext,
		// Thumbnails for a library page nearly all come from one host, so a
		// per-host connection cap is what actually bounds the fan-out: a
		// cold cache would otherwise open one connection per visible row.
		MaxConnsPerHost:       8,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: thumbFetchTimeout,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 2 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return errors.New("redirect must stay https")
		}
		return nil
	},
}

func fetchThumbnail(parent context.Context, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(parent, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	if req.URL.Scheme != "https" {
		return nil, "", errors.New("thumbnail url must be https")
	}
	resp, err := thumbClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("thumbnail fetch status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxThumbBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxThumbBytes {
		return nil, "", errors.New("thumbnail exceeds size cap")
	}
	ctype := http.DetectContentType(data[:min(512, len(data))])
	if _, ok := allowedImageTypes[ctype]; !ok {
		return nil, "", errors.New("thumbnail is not a supported image")
	}
	return data, ctype, nil
}

// safeDialContext rejects non-public destinations before connecting. The
// Control hook receives the RESOLVED address, so DNS rebinding to internal
// space is caught at dial time, not just by hostname inspection.
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{
		Timeout: 4 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return errors.New("thumbnail host did not resolve to an IP")
			}
			if !isPublicIP(ip) {
				return errors.New("thumbnail host is not a public address")
			}
			return nil
		},
	}
	return d.DialContext(ctx, network, addr)
}

// specialUseNets are ranges that net.IP.IsPrivate does not cover but which are
// still not public routable space. IsPrivate only knows RFC1918 and fc00::/7,
// so on its own it would happily dial a Tailscale/CGNAT peer on 100.64.0.0/10
// or a reserved 240.0.0.0/4 address.
var specialUseNets = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // CGNAT — also where Tailscale lives
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"192.88.99.0/24",  // deprecated 6to4 relay anycast
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved, includes 255.255.255.255
		"64:ff9b::/96",    // NAT64
		"64:ff9b:1::/48",  // local-use NAT64
		"100::/64",        // discard-only
		"2001::/23",       // IETF protocol assignments
		"2001:db8::/32",   // documentation
		"3fff::/20",       // documentation
		"5f00::/16",       // segment routing
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// isPublicIP reports whether an address is safe to fetch a thumbnail from.
// The policy is a denylist of special-use space rather than IsPrivate alone,
// because IsPrivate answers "is this RFC1918", not "is this reachable
// internal infrastructure".
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// An IPv4-mapped IPv6 address must be judged as the IPv4 address it is.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, n := range specialUseNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// readCappedFile reads at most limit bytes and reports an error if the file is
// larger, so a corrupted or tampered cache entry cannot drive allocation.
func readCappedFile(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("cached thumbnail exceeds size cap")
	}
	return data, nil
}

// fetchThumb is the remote fetch, indirected so tests can exercise the
// coalescing logic without a real origin: the dialer correctly refuses
// loopback, which is where a test server necessarily lives.
var fetchThumb = fetchThumbnail

// thumbFetch is one in-flight remote thumbnail fetch that later callers for
// the same download wait on instead of starting their own.
type thumbFetch struct {
	done  chan struct{}
	data  []byte
	ctype string
	err   error
}

// fetchThumbnailOnce collapses concurrent cache misses for one download into a
// single remote request. The Library grid renders many rows at once, so on a
// cold cache every visible row would otherwise hit the origin separately.
//
// The shared fetch deliberately does not use the first caller's request
// context: if that client disconnects, everyone waiting behind it would fail
// too. It gets its own timeout instead.
func (s *Server) fetchThumbnailOnce(ctx context.Context, id, url string) ([]byte, string, error) {
	s.thumbFlightMu.Lock()
	if s.thumbFlight == nil {
		s.thumbFlight = map[string]*thumbFetch{}
	}
	if f, ok := s.thumbFlight[id]; ok {
		s.thumbFlightMu.Unlock()
		s.thumbFlightWaiters.Add(1)
		defer s.thumbFlightWaiters.Add(-1)
		select {
		case <-f.done:
			return f.data, f.ctype, f.err
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	f := &thumbFetch{done: make(chan struct{})}
	s.thumbFlight[id] = f
	s.thumbFlightMu.Unlock()

	go func() {
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), thumbFetchTimeout)
		defer cancel()
		f.data, f.ctype, f.err = fetchThumb(fetchCtx, url)
		close(f.done)
		s.thumbFlightMu.Lock()
		delete(s.thumbFlight, id)
		s.thumbFlightMu.Unlock()
	}()

	select {
	case <-f.done:
		return f.data, f.ctype, f.err
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
}
