package main

import (
	"testing"

	"github.com/rs/zerolog"
)

func TestNewLogger_DefaultsToInfo(t *testing.T) {
	l := newLogger("")
	if l.GetLevel() != zerolog.InfoLevel {
		t.Fatalf("level = %v, want info", l.GetLevel())
	}
}

func TestNewLogger_DefaultsBogusToInfo(t *testing.T) {
	l := newLogger("loud")
	if l.GetLevel() != zerolog.InfoLevel {
		t.Fatalf("level = %v, want info", l.GetLevel())
	}
}

func TestNewLogger_AcceptsAllFour(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error"} {
		t.Run(lvl, func(t *testing.T) {
			l := newLogger(lvl)
			if l.GetLevel().String() != lvl {
				t.Fatalf("level = %v, want %s", l.GetLevel(), lvl)
			}
		})
	}
}
