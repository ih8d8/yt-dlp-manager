package ipc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const SocketName = "yt-dlp-manager.sock"

const legacySocketName = "yt-dlp-tui.sock"

const maxLine = 1 << 20

var errTooLarge = errors.New("message too large")

// DefaultSocketPath returns the IPC socket location. $XDG_RUNTIME_DIR is a
// user-owned 0700 directory on every compliant desktop, which is what makes
// the 0600 socket same-user-only; the fallback lives under the user's cache
// dir for the same reason (never in world-writable /tmp, where another local
// user could squat the path and impersonate the daemon).
func DefaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, SocketName)
	}
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), fmt.Sprintf("yt-dlp-manager-%d.sock", os.Getuid()))
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "yt-dlp-manager", SocketName)
}

// LegacySocketPath returns the pre-rename socket location. It exists only so
// explicit TUI/CLI clients can fall back to a still-running pre-rename
// daemon; server ownership never creates or removes this path.
func LegacySocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, legacySocketName)
	}
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), fmt.Sprintf("yt-dlp-tui-%d.sock", os.Getuid()))
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "yt-dlp-tui", legacySocketName)
}

// callTimeout bounds how long a Call waits for its response.
var callTimeout = 15 * time.Second

type respOrErr struct {
	line []byte
	err  error
}

// Conn multiplexes a single unix-socket connection carrying both RPC
// responses and pushed events. One reader goroutine owns the wire and routes
// lines by shape: anything with an "event" field is an Event, everything else
// is a Response. This keeps Call and NextEvent race-free without changing the
// wire format.
type Conn struct {
	c   net.Conn
	br  *bufio.Reader
	enc *Encoder

	events chan Event
	resps  chan respOrErr
	closed chan struct{}
	once   sync.Once

	callMu sync.Mutex // one in-flight Call at a time

	errMu sync.Mutex
	rerr  error // terminal read error once the pump has exited
}

func Dial(path string) (*Conn, error) {
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, err
	}
	return wrap(c), nil
}

func wrap(c net.Conn) *Conn {
	conn := &Conn{
		c:      c,
		br:     bufio.NewReaderSize(c, 64<<10),
		enc:    NewEncoder(c),
		events: make(chan Event, 256),
		resps:  make(chan respOrErr, 1),
		closed: make(chan struct{}),
	}
	go conn.pump()
	return conn
}

func (c *Conn) setErr(err error) {
	c.errMu.Lock()
	if c.rerr == nil {
		c.rerr = err
	}
	c.errMu.Unlock()
}

func (c *Conn) readErr() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.rerr
}

func (c *Conn) pump() {
	fatal := func(err error) {
		c.setErr(err)
		select {
		case c.resps <- respOrErr{err: err}:
		default:
		}
		close(c.events)
	}
	for {
		line, err := readBoundedLine(c.br, maxLine)
		if err != nil {
			if errors.Is(err, errTooLarge) {
				continue // skip the poisoned line, keep the framing intact
			}
			fatal(err)
			return
		}
		if isEvent(line) {
			var ev Event
			if derr := decodeStrict(line, &ev); derr != nil {
				fatal(fmt.Errorf("malformed event from manager: %w", derr))
				return
			}
			// Ordered delivery: consumers are expected to drain. But a blocked
			// send must still be interruptible, or a consumer that stops
			// reading wedges the pump forever — taking RPC responses with it,
			// and preventing the fatal path from ever closing the channel.
			select {
			case c.events <- ev:
			case <-c.closed:
				return
			}
			continue
		}
		deliver := func(m respOrErr) bool {
			select {
			case c.resps <- m:
				return true
			default:
				return false
			}
		}
		if deliver(respOrErr{line: line}) {
			continue
		}
		// A timed-out Call left no waiter; evict the stale response so the
		// fresh one (responses are strictly ordered per connection) lands.
		select {
		case <-c.resps:
		default:
		}
		deliver(respOrErr{line: line})
	}
}

func (c *Conn) Call(req Request) (Response, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	if err := c.readErr(); err != nil {
		return Response{}, err
	}
	// Discard a response orphaned by a previous timeout.
	select {
	case <-c.resps:
	default:
	}
	if err := c.enc.Encode(req); err != nil {
		return Response{}, err
	}
	timer := time.NewTimer(callTimeout)
	defer timer.Stop()
	select {
	case m := <-c.resps:
		if m.err != nil {
			return Response{}, m.err
		}
		var resp Response
		if err := decodeStrict(m.line, &resp); err != nil {
			return Response{}, err
		}
		return resp, nil
	case <-timer.C:
		err := errors.New("timed out waiting for manager response")
		c.setErr(err)
		_ = c.c.Close()
		return Response{}, err
	}
}

// Subscribe requests the event stream. It must be called before any other
// consumer reads events; the first event on a fresh connection is the
// snapshot. The returned snapshot is decoded with the same strict schema as
// every other message.
func (c *Conn) Subscribe() (Event, error) {
	if err := c.readErr(); err != nil {
		return Event{}, err
	}
	if err := c.enc.Encode(Request{Cmd: "subscribe"}); err != nil {
		return Event{}, err
	}
	timer := time.NewTimer(callTimeout)
	defer timer.Stop()
	select {
	case ev, ok := <-c.events:
		if !ok {
			return Event{}, c.readErr()
		}
		return ev, nil
	case <-timer.C:
		err := errors.New("timed out waiting for snapshot")
		c.setErr(err)
		_ = c.c.Close()
		return Event{}, err
	}
}

// NextEvent blocks until the next pushed event or the connection dies.
func (c *Conn) NextEvent() (Event, error) {
	ev, ok := <-c.events
	if !ok {
		return Event{}, c.readErr()
	}
	return ev, nil
}

func (c *Conn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.c.Close()
}

// isEvent classifies a raw line. Events always carry the "event" field (no
// omitempty on the wire); responses never do.
func isEvent(line []byte) bool {
	var probe struct {
		Event *string `json:"event"`
	}
	return json.Unmarshal(line, &probe) == nil && probe.Event != nil
}

func decodeStrict(line []byte, v any) error {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("JSON message must be an object")
	}
	d := json.NewDecoder(bytes.NewReader(trimmed))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values in one message")
		}
		return fmt.Errorf("trailing data after JSON message: %w", err)
	}
	return nil
}

// ReadLineJSON reads one newline-delimited message with a hard size limit
// applied during accumulation (not after), then strictly decodes it.
func ReadLineJSON(br *bufio.Reader, v any) error {
	line, err := readBoundedLine(br, maxLine)
	if err != nil {
		return err
	}
	return decodeStrict(line, v)
}

// readBoundedLine accumulates at most limit bytes before the newline. An
// oversized line returns errTooLarge after draining the remainder, so the
// stream stays usable for subsequent messages.
func readBoundedLine(br *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case err == bufio.ErrBufferFull:
			buf = append(buf, chunk...)
			if len(buf) > limit {
				drainLineRemainder(br)
				return nil, errTooLarge
			}
		case err == io.EOF:
			buf = append(buf, chunk...)
			if len(buf) == 0 {
				return nil, io.EOF
			}
			return buf, nil
		case err != nil:
			return nil, err
		default:
			buf = append(buf, chunk...)
			if len(buf) > limit {
				return nil, errTooLarge
			}
			return buf, nil
		}
	}
}

func drainLineRemainder(br *bufio.Reader) {
	for {
		_, err := br.ReadSlice('\n')
		if err != bufio.ErrBufferFull {
			return
		}
	}
}

// Encoder writes newline-delimited JSON messages to a connection.
//
// Encode is safe for concurrent use, which the IPC server needs: its request
// loop writes responses while a subscription goroutine writes events over the
// same connection. Locking only the underlying writer is not enough —
// json.Encoder is not documented as concurrency-safe and keeps a sticky error
// field it both reads and writes — so the whole Encode is serialized.
type Encoder struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{enc: json.NewEncoder(w)}
}

func (e *Encoder) Encode(v any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.enc.Encode(v)
}
