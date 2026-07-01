package logging

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// recordingHandler captures every record delivered through it so tests can
// assert on what would have been emitted. Attributes and groups are
// deliberately ignored: tests that need them use a JSON handler instead. It
// is safe for concurrent use.
type recordingHandler struct {
	mutex   sync.Mutex
	records []slog.Record
}

func (handler *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (handler *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	handler.mutex.Lock()
	handler.records = append(handler.records, record)
	handler.mutex.Unlock()
	return nil
}

func (handler *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return handler }

func (handler *recordingHandler) WithGroup(string) slog.Handler { return handler }

func (handler *recordingHandler) count() int {
	handler.mutex.Lock()
	defer handler.mutex.Unlock()
	return len(handler.records)
}

func (handler *recordingHandler) messages() []string {
	handler.mutex.Lock()
	defer handler.mutex.Unlock()
	messages := make([]string, 0, len(handler.records))
	for _, record := range handler.records {
		messages = append(messages, record.Message)
	}
	return messages
}

func (handler *recordingHandler) levels() []slog.Level {
	handler.mutex.Lock()
	defer handler.mutex.Unlock()
	levels := make([]slog.Level, 0, len(handler.records))
	for _, record := range handler.records {
		levels = append(levels, record.Level)
	}
	return levels
}

func TestEffectiveLevelInheritsFromTheNearestConfiguredAncestor(t *testing.T) {
	manager := NewManager(
		WithLevel("github.com/acme/application", slog.LevelWarn),
		WithLevel("github.com/acme/application/storage", slog.LevelDebug),
	)

	if got := manager.EffectiveLevel("github.com/acme/application/storage/postgres.Store"); got != slog.LevelDebug {
		t.Errorf("descendant of the storage package resolved %v, want %v", got, slog.LevelDebug)
	}
	if got := manager.EffectiveLevel("github.com/acme/application/web"); got != slog.LevelWarn {
		t.Errorf("sibling under the application package resolved %v, want %v", got, slog.LevelWarn)
	}
	if got := manager.EffectiveLevel("github.com/other"); got != slog.LevelInfo {
		t.Errorf("unconfigured name resolved %v, want the root default %v", got, slog.LevelInfo)
	}
}

func TestASiblingWithACommonPrefixDoesNotInheritTheLevel(t *testing.T) {
	manager := NewManager(WithLevel("github.com/acme/event", slog.LevelDebug))

	if got := manager.EffectiveLevel("github.com/acme/eventbus"); got != slog.LevelInfo {
		t.Errorf("eventbus resolved %v, want the root default %v: the hierarchy must cut whole segments", got, slog.LevelInfo)
	}
}

func TestSetLevelRetunesLoggersAlreadyCreated(t *testing.T) {
	recorder := &recordingHandler{}
	manager := NewManager(WithHandler(recorder))
	logger := manager.Logger("github.com/acme/application")

	logger.Debug("dropped while the root level holds")
	manager.SetLevel("github.com/acme/application", slog.LevelDebug)
	logger.Debug("emitted after the runtime change")

	if got := recorder.count(); got != 1 {
		t.Fatalf("recorded %d records, want exactly the one emitted after SetLevel", got)
	}
	if got := recorder.messages()[0]; got != "emitted after the runtime change" {
		t.Errorf("recorded message %q, want the record emitted after SetLevel", got)
	}
}

func TestResetLevelReinheritsFromTheNearestConfiguredAncestor(t *testing.T) {
	manager := NewManager(WithLevel("github.com/acme", slog.LevelWarn))
	manager.SetLevel("github.com/acme/application", slog.LevelDebug)

	manager.ResetLevel("github.com/acme/application")

	if got := manager.EffectiveLevel("github.com/acme/application"); got != slog.LevelWarn {
		t.Errorf("effective level after reset = %v, want the ancestor level %v", got, slog.LevelWarn)
	}
	if _, configured := manager.ConfiguredLevel("github.com/acme/application"); configured {
		t.Error("the name still reports a configured level after reset")
	}
}

func TestResetLevelOnTheRootRestoresTheConstructionLevel(t *testing.T) {
	manager := NewManager(WithRootLevel(slog.LevelWarn))
	manager.SetLevel("root", slog.LevelDebug)

	manager.ResetLevel("root")

	if got := manager.EffectiveLevel("root"); got != slog.LevelWarn {
		t.Errorf("root level after reset = %v, want the construction level %v", got, slog.LevelWarn)
	}
}

func TestATypeLevelAssignmentOverridesItsPackageLevel(t *testing.T) {
	manager := NewManager(WithLevel("github.com/acme/application/storage", slog.LevelWarn))

	manager.SetLevel("github.com/acme/application/storage.Store", LevelTrace)

	if got := manager.EffectiveLevel("github.com/acme/application/storage.Store"); got != LevelTrace {
		t.Errorf("type-level name resolved %v, want %v", got, LevelTrace)
	}
	if got := manager.EffectiveLevel("github.com/acme/application/storage.Cache"); got != slog.LevelWarn {
		t.Errorf("sibling type resolved %v, want the package level %v", got, slog.LevelWarn)
	}
}

func TestGroupAssignmentFansOutToEveryMember(t *testing.T) {
	manager := NewManager(WithGroup("persistence", "github.com/acme/storage", "github.com/acme/cache"))

	manager.SetLevel("persistence", slog.LevelDebug)

	if got := manager.EffectiveLevel("github.com/acme/storage"); got != slog.LevelDebug {
		t.Errorf("first member resolved %v, want %v", got, slog.LevelDebug)
	}
	if got := manager.EffectiveLevel("github.com/acme/cache"); got != slog.LevelDebug {
		t.Errorf("second member resolved %v, want %v", got, slog.LevelDebug)
	}
	if _, configured := manager.ConfiguredLevel("persistence"); configured {
		t.Error("the group name itself must stay unconfigured: a group with members shadows a logger of the same name")
	}
}

func TestResetLevelOnAGroupClearsEveryMember(t *testing.T) {
	manager := NewManager(WithGroup("persistence", "github.com/acme/storage", "github.com/acme/cache"))
	manager.SetLevel("persistence", slog.LevelDebug)

	manager.ResetLevel("persistence")

	if _, configured := manager.ConfiguredLevel("github.com/acme/storage"); configured {
		t.Error("first member still reports a configured level after the group reset")
	}
	if _, configured := manager.ConfiguredLevel("github.com/acme/cache"); configured {
		t.Error("second member still reports a configured level after the group reset")
	}
}

func TestConfiguredLevelReportsOnlyDirectAssignments(t *testing.T) {
	manager := NewManager(WithLevel("github.com/acme", slog.LevelWarn))

	if level, configured := manager.ConfiguredLevel("github.com/acme"); !configured || level != slog.LevelWarn {
		t.Errorf("configured level = %v, %t, want %v, true", level, configured, slog.LevelWarn)
	}
	if _, configured := manager.ConfiguredLevel("github.com/acme/application"); configured {
		t.Error("a descendant reports a configured level it only inherits")
	}
	if level, configured := manager.ConfiguredLevel("root"); !configured || level != slog.LevelInfo {
		t.Errorf("root configured level = %v, %t, want %v, true: the root always has one", level, configured, slog.LevelInfo)
	}
}

func TestLoggersListsTheRootFirstWithConfiguredAndEffectiveLevels(t *testing.T) {
	manager := NewManager(
		WithHandler(&recordingHandler{}),
		WithLevel("github.com/acme/storage", slog.LevelDebug),
	)
	manager.Logger("github.com/acme/web")

	loggers := manager.Loggers()

	if len(loggers) != 3 {
		t.Fatalf("listed %d loggers, want the root plus two names", len(loggers))
	}
	root := loggers[0]
	if root.Name != "root" || !root.IsConfigured || root.Effective != slog.LevelInfo {
		t.Errorf("first entry = %+v, want the root logger configured at %v", root, slog.LevelInfo)
	}
	storage := loggers[1]
	if storage.Name != "github.com/acme/storage" || !storage.IsConfigured || storage.Configured != slog.LevelDebug {
		t.Errorf("storage entry = %+v, want a direct assignment of %v", storage, slog.LevelDebug)
	}
	web := loggers[2]
	if web.Name != "github.com/acme/web" || web.IsConfigured || web.Effective != slog.LevelInfo {
		t.Errorf("web entry = %+v, want an inherited effective level of %v", web, slog.LevelInfo)
	}
}

func TestGroupsReturnsACopyOfTheDeclaredGroups(t *testing.T) {
	manager := NewManager(WithGroup("persistence", "github.com/acme/storage"))

	groups := manager.Groups()
	groups["persistence"][0] = "mutated"

	if got := manager.Groups()["persistence"][0]; got != "github.com/acme/storage" {
		t.Errorf("group member = %q after mutating the returned copy, want the original name", got)
	}
}

func TestLevelOffSilencesEvenErrorRecords(t *testing.T) {
	recorder := &recordingHandler{}
	manager := NewManager(WithHandler(recorder))
	logger := manager.Logger("github.com/acme/noisy")

	manager.SetLevel("github.com/acme/noisy", LevelOff)
	logger.Error("silenced by OFF")

	if got := recorder.count(); got != 0 {
		t.Errorf("recorded %d records, want none: OFF silences every level", got)
	}
}

func TestLevelTraceEnablesRecordsBelowDebug(t *testing.T) {
	recorder := &recordingHandler{}
	manager := NewManager(WithHandler(recorder))
	logger := manager.Logger("github.com/acme/verbose")

	logger.Log(t.Context(), LevelTrace, "dropped while the root level holds")
	manager.SetLevel("github.com/acme/verbose", LevelTrace)
	logger.Log(t.Context(), LevelTrace, "emitted at trace")

	if got := recorder.count(); got != 1 {
		t.Fatalf("recorded %d records, want exactly the trace record emitted after SetLevel", got)
	}
}

func TestWithHandlerIgnoresANilHandler(t *testing.T) {
	recorder := &recordingHandler{}
	manager := NewManager(WithHandler(recorder), WithHandler(nil))

	manager.Logger("github.com/acme").Info("delivered to the earlier delegate")

	if got := recorder.count(); got != 1 {
		t.Fatalf("recorded %d records, want 1: a nil handler must not replace the delegate", got)
	}
}

func TestWithGroupIgnoresARootNameAndBlankMembers(t *testing.T) {
	manager := NewManager(
		WithGroup("root", "github.com/acme/storage"),
		WithGroup("persistence", "", "github.com/acme/cache"),
	)

	groups := manager.Groups()
	if _, present := groups[""]; present {
		t.Error("a group named after the root logger was registered")
	}
	if got := groups["persistence"]; len(got) != 1 || got[0] != "github.com/acme/cache" {
		t.Errorf("persistence members = %q, want only the non-blank member", got)
	}
}

func TestAGroupWithoutMembersDoesNotShadowALogger(t *testing.T) {
	manager := NewManager(WithGroup("solo"))

	manager.SetLevel("solo", slog.LevelDebug)

	if level, configured := manager.ConfiguredLevel("solo"); !configured || level != slog.LevelDebug {
		t.Errorf("configured level = %v, %t, want %v, true: an empty group must not swallow the assignment", level, configured, slog.LevelDebug)
	}
}

func TestOptionsApplyInOrderSoAGroupPrecedesItsLevel(t *testing.T) {
	manager := NewManager(
		WithGroup("persistence", "github.com/acme/storage"),
		WithLevel("persistence", slog.LevelDebug),
	)

	if got := manager.EffectiveLevel("github.com/acme/storage"); got != slog.LevelDebug {
		t.Errorf("group member resolved %v, want %v: the group declared first must receive the later level", got, slog.LevelDebug)
	}
}

func TestLoggerForUsesThePackageQualifiedTypeName(t *testing.T) {
	recorder := &recordingHandler{}
	manager := NewManager(WithHandler(recorder))

	manager.SetLevel(NameFor[sampleService](), slog.LevelDebug)
	logger := LoggerFor[sampleService](manager)
	logger.Debug("enabled through the type-level name")

	if got := recorder.count(); got != 1 {
		t.Fatalf("recorded %d records, want the debug record the type-level assignment enables", got)
	}
}
