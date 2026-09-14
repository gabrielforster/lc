package server

import (
	"fmt"
	"log/slog"
)

// yamuxLogger adapts slog to yamux's Logger interface. yamux is chatty about
// routine session teardown, so its output lands at debug level.
type yamuxLogger struct{ log *slog.Logger }

func slogAdapter(log *slog.Logger) yamuxLogger { return yamuxLogger{log: log} }

func (l yamuxLogger) Print(v ...any) { l.log.Debug("yamux: " + fmt.Sprint(v...)) }

func (l yamuxLogger) Printf(format string, v ...any) {
	l.log.Debug("yamux: " + fmt.Sprintf(format, v...))
}

func (l yamuxLogger) Println(v ...any) { l.log.Debug("yamux: " + fmt.Sprintln(v...)) }
