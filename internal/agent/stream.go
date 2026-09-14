package agent

import (
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/gabrielforster/lc/internal/muxproto"
	"github.com/hashicorp/yamux"
)

// streamConn presents a yamux stream as a net.Conn whose reads continue from
// wherever the control-frame reader stopped.
//
// Reading the StreamInit header through a buffered reader can pull in payload
// bytes behind it. Handing the raw stream to the transform would silently drop
// those, so reads are routed back through the same buffer.
type streamConn struct {
	*yamux.Stream
	r *muxproto.Reader
}

func (c *streamConn) Read(p []byte) (int, error) { return c.r.Buffered().Read(p) }

// CloseWrite exposes the stream's half-close, which yamux spells Close.
func (c *streamConn) CloseWrite() error { return c.Stream.Close() }

func (c *streamConn) LocalAddr() net.Addr  { return c.Stream.LocalAddr() }
func (c *streamConn) RemoteAddr() net.Addr { return c.Stream.RemoteAddr() }

func (c *streamConn) SetDeadline(t time.Time) error      { return c.Stream.SetDeadline(t) }
func (c *streamConn) SetReadDeadline(t time.Time) error  { return c.Stream.SetReadDeadline(t) }
func (c *streamConn) SetWriteDeadline(t time.Time) error { return c.Stream.SetWriteDeadline(t) }

// yamuxLogger adapts slog to yamux's Logger interface.
type yamuxLogger struct{ log *slog.Logger }

func slogAdapter(log *slog.Logger) yamuxLogger { return yamuxLogger{log: log} }

func (l yamuxLogger) Print(v ...any) { l.log.Debug("yamux: " + fmt.Sprint(v...)) }
func (l yamuxLogger) Printf(format string, v ...any) {
	l.log.Debug("yamux: " + fmt.Sprintf(format, v...))
}
func (l yamuxLogger) Println(v ...any) { l.log.Debug("yamux: " + fmt.Sprintln(v...)) }
