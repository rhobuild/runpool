package app

import (
	"log/slog"
	"testing"

	"github.com/rhobuild/runpool/internal/config"
)

// TestEveryConfiguredLevelReachesTheHandler: the level vocabulary's one
// behavioural consumer is this switch, and it read bare literals -- so
// `case "wran"` compiled and logged at info, silently, which is the
// failure the named type exists to prevent and could not while the
// switch spelled its own words.
//
// The default is deliberate and stays: a level the validator refuses
// never reaches here, so the fall-through is for the zero value before
// defaults are applied, not for an unknown word.
func TestEveryConfiguredLevelReachesTheHandler(t *testing.T) {
	for level, want := range map[config.LogLevel]slog.Level{
		config.LogLevelDebug: slog.LevelDebug,
		config.LogLevelInfo:  slog.LevelInfo,
		config.LogLevelWarn:  slog.LevelWarn,
		config.LogLevelError: slog.LevelError,
	} {
		log := newLogger(config.LogConfig{Level: level, Format: config.LogFormatJSON})
		if got := log.Handler(); !got.Enabled(t.Context(), want) {
			t.Errorf("level %q does not enable %v", level, want)
		}
		if want > slog.LevelDebug && log.Handler().Enabled(t.Context(), want-1) {
			t.Errorf("level %q enables everything below %v; the configured floor is not in force", level, want)
		}
	}
}
