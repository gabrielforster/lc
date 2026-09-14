package netutil

import (
	"io"
	"sync"
	"time"
)

// CloseWriter is implemented by connections that can signal "no more data from
// this side" without tearing the whole connection down. *net.TCPConn provides
// it directly; *yamux.Stream's Close sends a FIN and leaves reads working, so
// it is adapted to this interface by YamuxHalfCloser.
type CloseWriter interface {
	CloseWrite() error
}

// Join copies between a and b until both directions finish, propagating EOF as
// a half-close rather than a full close.
//
// Propagating the half-close matters: a plain io.Copy pair leaks connections,
// because a peer that has finished sending but is still waiting to receive
// never learns the other side is done. The symptom is gradual resource
// exhaustion rather than a visible failure, which is why it is handled here
// once instead of at each call site.
//
// Join returns the first error that was not a normal end-of-stream.
func Join(a, b io.ReadWriteCloser) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ferr error
	)

	cp := func(dst, src io.ReadWriteCloser) {
		defer wg.Done()
		_, err := io.Copy(dst, src)

		if err == nil {
			// A clean EOF means this direction is finished while the other may
			// still be in use, so only the write half is closed.
			if cw, ok := dst.(CloseWriter); ok {
				cw.CloseWrite()
			} else {
				dst.Close()
			}
			return
		}

		// An error means the connection is broken rather than finished. Half-
		// closing here would leave the opposite direction blocked forever on a
		// peer that is gone -- and that peer is precisely what an idle timeout
		// is trying to reclaim.
		//
		// Closing is not enough on its own: a yamux stream's Close is itself a
		// half-close, so a goroutine already blocked reading it would keep
		// waiting. Expiring the deadlines forces those reads to return, which
		// is what actually releases the sockets, the stream and the tunnel's
		// connection slot.
		expire(a)
		expire(b)
		a.Close()
		b.Close()

		if !isBenign(err) {
			mu.Lock()
			if ferr == nil {
				ferr = err
			}
			mu.Unlock()
		}
	}

	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()

	a.Close()
	b.Close()
	return ferr
}

// deadliner is implemented by anything that can have in-flight reads and
// writes interrupted -- net.Conn and yamux.Stream both do.
type deadliner interface {
	SetDeadline(time.Time) error
}

// expire forces any read or write already blocked on c to return.
func expire(c any) {
	if d, ok := c.(deadliner); ok {
		// Any time in the past works; reads waiting now return immediately.
		d.SetDeadline(time.Now().Add(-time.Second))
	}
}

// isBenign reports whether err is just a connection ending, which every proxied
// connection does eventually and which is not worth logging.
func isBenign(err error) bool {
	if err == nil || err == io.EOF || err == io.ErrClosedPipe {
		return true
	}
	s := err.Error()
	return contains(s, "use of closed network connection") ||
		contains(s, "connection reset by peer") ||
		contains(s, "broken pipe") ||
		contains(s, "stream closed") ||
		contains(s, "session shutdown")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
