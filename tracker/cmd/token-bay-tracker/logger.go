package main

import (
	"os"

	"github.com/rs/zerolog"
)

// newLogger builds a zerolog logger at the requested level. Empty or
// unparseable level → info. Output goes to stderr; tests capture via
// zerolog.New on a *bytes.Buffer.
func newLogger(level string) zerolog.Logger {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	lvl, err := zerolog.ParseLevel(level)
	if err != nil || lvl == zerolog.NoLevel {
		lvl = zerolog.InfoLevel
	}
	return zerolog.New(os.Stderr).Level(lvl).With().Timestamp().Logger()
}
