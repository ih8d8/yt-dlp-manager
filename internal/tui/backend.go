package tui

import (
	"errors"
	"fmt"
	"sync"

	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
)

type backend interface {
	Events() <-chan ipc.Event
	Do(cmd, id, url string) error
}

type localBackend struct {
	mgr    *manager.Manager
	ch     chan ipc.Event
	done   chan struct{}
	cancel func()
	once   sync.Once
}

func newLocalBackend(mgr *manager.Manager) *localBackend {
	src, cancel := mgr.Subscribe()
	out := make(chan ipc.Event, 512)
	b := &localBackend{mgr: mgr, ch: out, done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(out)
		for {
			var ev ipc.Event
			select {
			case next, ok := <-src:
				if !ok {
					return
				}
				ev = next
			case <-b.done:
				return
			}
			if ev.IsProgress() {
				// Progress is self-superseding; drop rather than stall.
				select {
				case out <- ev:
				case <-b.done:
					return
				default:
				}
				continue
			}
			// Structural events (removed/snapshot/state changes) must be
			// delivered or the UI diverges from the manager.
			select {
			case out <- ev:
			case <-b.done:
				return
			}
		}
	}()
	return b
}

func (l *localBackend) Events() <-chan ipc.Event { return l.ch }

func (l *localBackend) Do(cmd, id, url string) error {
	switch cmd {
	case "add":
		_, err := l.mgr.Add(url)
		return err
	case "pause":
		return l.mgr.Pause(id)
	case "resume":
		return l.mgr.Resume(id)
	case "remove", "cancel":
		return l.mgr.Cancel(id)
	case "start_now":
		return l.mgr.StartNow(id)
	case "clear_finished":
		_, err := l.mgr.ClearFinished()
		return err
	case "clear_all":
		_, err := l.mgr.ClearAll()
		return err
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func (l *localBackend) Close() {
	l.once.Do(func() {
		close(l.done)
		l.cancel()
	})
}

type socketBackend struct {
	conn *ipc.Conn
	ch   chan ipc.Event
	done chan struct{}
	once sync.Once
}

func newSocketBackend(c *ipc.Conn, snap ipc.Event) *socketBackend {
	b := &socketBackend{conn: c, ch: make(chan ipc.Event, 256), done: make(chan struct{})}
	b.ch <- snap
	go func() {
		defer close(b.ch)
		for {
			ev, err := c.NextEvent()
			if err != nil {
				select {
				case b.ch <- ipc.Event{Event: "__closed"}:
				default:
				}
				return
			}
			// Forwarding blocks rather than drops: the UI always drains, and
			// backpressure terminates at manager.publish, which never blocks.
			// Losing structural events here would leave ghost rows.
			select {
			case b.ch <- ev:
			case <-b.done:
				return
			}
		}
	}()
	return b
}

func (s *socketBackend) Events() <-chan ipc.Event { return s.ch }

func (s *socketBackend) Do(cmd, id, url string) error {
	resp, err := s.conn.Call(ipc.Request{Cmd: cmd, ID: id, URL: url})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

func (s *socketBackend) Close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.conn.Close()
	})
}
