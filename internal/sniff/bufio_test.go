package sniff

import (
	"bufio"
	"io"
)

// newBufio wraps a reader for tests that re-read a sniffed connection.
func newBufio(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }
