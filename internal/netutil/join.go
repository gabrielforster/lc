package netutil

import (
	"io"
	"sync"
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
		// Signal completion to the peer, then make sure a stalled reader on the
		// other side cannot block forever if half-close is unavailable.
		if cw, ok := dst.(CloseWriter); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		if err != nil && !isBenign(err) {
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
