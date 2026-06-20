package logger

import (
	"os"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

// New configures the global zerolog logger and returns it.
// All packages using zerolog/log inherit the same format and level.
// trace is the more verbose of the two and implies debug.
func New(debug, trace bool) zerolog.Logger {
	level := zerolog.InfoLevel
	if debug {
		level = zerolog.DebugLevel
	}
	if trace {
		level = zerolog.TraceLevel
	}
	zerolog.SetGlobalLevel(level)
	l := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
		Level(level).
		With().Timestamp().Logger()
	zlog.Logger = l
	return l
}
