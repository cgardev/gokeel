package logging

import (
	"errors"
	"log/slog"
	"testing"
)

func TestParseLevelAcceptsEverySpringToken(t *testing.T) {
	tokens := []struct {
		token string
		level slog.Level
	}{
		{"trace", LevelTrace},
		{"TRACE", LevelTrace},
		{"debug", slog.LevelDebug},
		{"Info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		// Logback maps FATAL to ERROR; the token parses for Spring parity.
		{"fatal", slog.LevelError},
		{"off", LevelOff},
		{"OFF", LevelOff},
		// Spring aliases false to OFF because YAML reads a bare off as false.
		{"false", LevelOff},
		{" info ", slog.LevelInfo},
		{"DEBUG-2", slog.LevelDebug - 2},
		{"info+2", slog.LevelInfo + 2},
	}
	for _, entry := range tokens {
		level, err := ParseLevel(entry.token)
		if err != nil {
			t.Fatalf("token %q: %v", entry.token, err)
		}
		if level != entry.level {
			t.Errorf("token %q parsed as %v, want %v", entry.token, level, entry.level)
		}
	}
}

func TestParseLevelRejectsAnUnknownToken(t *testing.T) {
	if _, err := ParseLevel("loud"); !errors.Is(err, ErrInvalidLevel) {
		t.Fatalf("parse error = %v, want ErrInvalidLevel", err)
	}
}

func TestLevelNameRendersTheSpringTokens(t *testing.T) {
	names := []struct {
		level slog.Level
		name  string
	}{
		{LevelTrace, "TRACE"},
		{LevelOff, "OFF"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelDebug + 2, "DEBUG+2"},
	}
	for _, entry := range names {
		if got := LevelName(entry.level); got != entry.name {
			t.Errorf("level %d rendered as %q, want %q", entry.level, got, entry.name)
		}
	}
}
