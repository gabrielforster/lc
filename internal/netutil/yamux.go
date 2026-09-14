package netutil

import "github.com/hashicorp/yamux"

// YamuxHalfCloser adapts a yamux stream to CloseWriter.
//
// yamux.Stream.Close is already a half-close — on an established stream it sends
// a FIN, moves to streamLocalClose and leaves reads working — but it is spelled
// Close, so Join would otherwise treat it as a full teardown and cut off data
// still in flight from the other direction.
type YamuxHalfCloser struct {
	*yamux.Stream
}

func (s YamuxHalfCloser) CloseWrite() error { return s.Stream.Close() }
