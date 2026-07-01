package logging

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
)

// Levels beyond the four slog predeclares, completing the ladder Spring
// accepts for its logging.level properties.
const (
	// LevelTrace enables the most verbose diagnostics. It sits one slog
	// spacing step below slog.LevelDebug, the position TRACE occupies
	// below DEBUG in Logback.
	LevelTrace slog.Level = slog.LevelDebug - 4

	// LevelOff silences a logger entirely: no record carries a level this
	// high. It mirrors Logback's OFF, which is Integer.MAX_VALUE.
	LevelOff slog.Level = math.MaxInt32
)

// ErrInvalidLevel reports a level token that names no known level. The
// offending token is attached to the returned error and the sentinel remains
// reachable through errors.Is.
var ErrInvalidLevel = errors.New("log level is not recognized")

// ParseLevel converts a Spring-style level token into a slog.Level. It
// accepts the Spring tokens trace, debug, info, warn, error, fatal, and off
// in any case, the extra convenience alias warning, plus the offset notation
// slog itself parses, such as "DEBUG-2". Two mappings mirror Spring Boot
// exactly: fatal parses as slog.LevelError, the way Logback maps FATAL to
// ERROR, and false parses as LevelOff, the alias Spring keeps because YAML
// reads a bare off as the boolean false.
func ParseLevel(token string) (slog.Level, error) {
	trimmed := strings.TrimSpace(token)
	switch strings.ToLower(trimmed) {
	case "trace":
		return LevelTrace, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "fatal":
		return slog.LevelError, nil
	case "off", "false":
		return LevelOff, nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(trimmed)); err != nil {
		return 0, fmt.Errorf("%w: %q: %w", ErrInvalidLevel, token, err)
	}
	return level, nil
}

// LevelName renders a level with the Spring-style tokens TRACE and OFF for
// the two levels this package adds, and with slog's own notation for every
// other value, so slog.LevelDebug+2 renders as "DEBUG+2".
func LevelName(level slog.Level) string {
	switch level {
	case LevelTrace:
		return "TRACE"
	case LevelOff:
		return "OFF"
	}
	return level.String()
}
