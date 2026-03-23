package util

import (
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// SetupLogging configures zerolog with the given level and format.
func SetupLogging(level string, jsonFormat bool) {
	var w io.Writer
	if jsonFormat {
		w = os.Stderr
	} else {
		w = zerolog.ConsoleWriter{
			Out:        os.Stderr,
			TimeFormat: time.RFC3339,
		}
	}

	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}

	log.Logger = zerolog.New(w).With().Timestamp().Logger().Level(lvl)
	zerolog.DefaultContextLogger = &log.Logger
}
