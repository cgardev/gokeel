package logging

import (
	"errors"
	"log/slog"
	"testing"
)

func TestApplyOverridesTheLevelsCompiledIn(t *testing.T) {
	manager := NewManager(WithLevel("github.com/acme/application", slog.LevelWarn))

	configuration, err := ParseConfiguration([]byte(`{
		"levels": {
			"github.com/acme/application": "debug",
			"root": "error"
		}
	}`))
	if err != nil {
		t.Fatalf("parse configuration: %v", err)
	}
	if err := manager.Apply(configuration); err != nil {
		t.Fatalf("apply configuration: %v", err)
	}

	if got := manager.EffectiveLevel("github.com/acme/application"); got != slog.LevelDebug {
		t.Errorf("externally overridden name resolved %v, want %v", got, slog.LevelDebug)
	}
	if got := manager.EffectiveLevel("github.com/other"); got != slog.LevelError {
		t.Errorf("root fallback resolved %v, want the externally assigned %v", got, slog.LevelError)
	}
}

func TestApplyLeavesUnmentionedAssignmentsUntouched(t *testing.T) {
	manager := NewManager(WithLevel("github.com/acme/storage", slog.LevelDebug))

	if err := manager.Apply(Configuration{Levels: map[string]string{"github.com/acme/web": "warn"}}); err != nil {
		t.Fatalf("apply configuration: %v", err)
	}

	if got := manager.EffectiveLevel("github.com/acme/storage"); got != slog.LevelDebug {
		t.Errorf("unmentioned name resolved %v, want its compiled-in %v", got, slog.LevelDebug)
	}
}

func TestApplyRejectsAnInvalidLevelTokenAndChangesNothing(t *testing.T) {
	manager := NewManager(WithLevel("github.com/acme/application", slog.LevelWarn))

	err := manager.Apply(Configuration{Levels: map[string]string{
		"github.com/acme/application": "loud",
	}})

	if !errors.Is(err, ErrInvalidLevel) {
		t.Fatalf("apply error = %v, want ErrInvalidLevel", err)
	}
	if got := manager.EffectiveLevel("github.com/acme/application"); got != slog.LevelWarn {
		t.Errorf("effective level after the failed apply = %v, want the untouched %v", got, slog.LevelWarn)
	}
}

func TestApplyRegistersGroupsForLaterRuntimeAdjustment(t *testing.T) {
	manager := NewManager()

	if err := manager.Apply(Configuration{Groups: map[string][]string{
		"persistence": {"github.com/acme/storage", "github.com/acme/cache"},
	}}); err != nil {
		t.Fatalf("apply configuration: %v", err)
	}
	manager.SetLevel("persistence", slog.LevelDebug)

	if got := manager.EffectiveLevel("github.com/acme/cache"); got != slog.LevelDebug {
		t.Errorf("group member resolved %v after a runtime group assignment, want %v", got, slog.LevelDebug)
	}
}

func TestADirectLevelWinsOverAGroupLevelWithinOneDocument(t *testing.T) {
	manager := NewManager()

	if err := manager.Apply(Configuration{
		Groups: map[string][]string{
			"persistence": {"github.com/acme/storage", "github.com/acme/cache"},
		},
		Levels: map[string]string{
			"persistence":             "error",
			"github.com/acme/storage": "debug",
		},
	}); err != nil {
		t.Fatalf("apply configuration: %v", err)
	}

	if got := manager.EffectiveLevel("github.com/acme/storage"); got != slog.LevelDebug {
		t.Errorf("directly configured member resolved %v, want the direct %v", got, slog.LevelDebug)
	}
	if got := manager.EffectiveLevel("github.com/acme/cache"); got != slog.LevelError {
		t.Errorf("other member resolved %v, want the group level %v", got, slog.LevelError)
	}
}

func TestParseConfigurationRejectsUnknownFields(t *testing.T) {
	if _, err := ParseConfiguration([]byte(`{"level": {"root": "debug"}}`)); err == nil {
		t.Fatal("a misspelled field was silently accepted")
	}
}

func TestParseConfigurationRejectsASecondJSONValue(t *testing.T) {
	document := []byte(`{"levels": {"root": "warn"}} {"levels": {"root": "debug"}}`)
	if _, err := ParseConfiguration(document); err == nil {
		t.Fatal("a document with two JSON values was accepted")
	}
}

func TestParseConfigurationRejectsAnInvalidLevelToken(t *testing.T) {
	_, err := ParseConfiguration([]byte(`{"levels": {"root": "loud"}}`))
	if !errors.Is(err, ErrInvalidLevel) {
		t.Fatalf("parse error = %v, want ErrInvalidLevel", err)
	}
}

func TestValidateRejectsAGroupThatDesignatesTheRootLogger(t *testing.T) {
	configuration := Configuration{Groups: map[string][]string{"root": {"github.com/acme"}}}
	if err := configuration.Validate(); err == nil {
		t.Fatal("a group named after the root logger was accepted")
	}
}

func TestValidateRejectsAGroupMemberThatNamesNoLogger(t *testing.T) {
	configuration := Configuration{Groups: map[string][]string{"persistence": {""}}}
	if err := configuration.Validate(); err == nil {
		t.Fatal("a blank group member was accepted")
	}
}

func TestParseLevelsReadsACompactAssignmentList(t *testing.T) {
	levels, err := ParseLevels(" root = warn , github.com/acme/storage=debug ")
	if err != nil {
		t.Fatalf("parse levels: %v", err)
	}
	if len(levels) != 2 {
		t.Fatalf("parsed %d entries, want 2", len(levels))
	}
	if levels["root"] != "warn" || levels["github.com/acme/storage"] != "debug" {
		t.Errorf("parsed entries = %v, want the trimmed names bound to their tokens", levels)
	}
}

func TestParseLevelsOfAnEmptyStringYieldsNoAssignments(t *testing.T) {
	levels, err := ParseLevels("")
	if err != nil {
		t.Fatalf("parse levels: %v", err)
	}
	if len(levels) != 0 {
		t.Errorf("parsed %d entries from an empty input, want none", len(levels))
	}
}

func TestParseLevelsRejectsAMalformedEntry(t *testing.T) {
	if _, err := ParseLevels("github.com/acme"); err == nil {
		t.Fatal("an entry without a level was accepted")
	}
	if _, err := ParseLevels("github.com/acme=loud"); !errors.Is(err, ErrInvalidLevel) {
		t.Fatalf("parse error = %v, want ErrInvalidLevel", err)
	}
}
