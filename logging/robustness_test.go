package logging

// The tests in this file are written to be run under the race detector in CI
// (go test -race): they pin that concurrent level changes, configuration
// application, and logging through shared handlers stay consistent.

import (
	"fmt"
	"log/slog"
	"sync"
	"testing"
)

func TestConcurrentLevelChangesAndLoggingAreSafe(t *testing.T) {
	const writers = 8
	const emitters = 8
	const iterations = 200

	recorder := &recordingHandler{}
	manager := NewManager(WithHandler(recorder))
	logger := manager.Logger("github.com/acme/application/storage")

	var group sync.WaitGroup
	for worker := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			for iteration := range iterations {
				if (worker+iteration)%2 == 0 {
					manager.SetLevel("github.com/acme/application", slog.LevelDebug)
				} else {
					manager.ResetLevel("github.com/acme/application")
				}
			}
		}()
	}
	for range emitters {
		group.Add(1)
		go func() {
			defer group.Done()
			for range iterations {
				logger.Debug("visible only while the racing ancestor allows debug")
				logger.Error("always above the root level")
			}
		}()
	}
	group.Wait()

	// Every error record passes regardless of the racing debug toggles, so
	// the recorder holds at least one record per emitter iteration; debug
	// records may add more.
	if got := recorder.count(); got < emitters*iterations {
		t.Errorf("recorded %d records, want at least %d", got, emitters*iterations)
	}
}

func TestConcurrentApplyAndSetLevelConverge(t *testing.T) {
	const appliers = 8
	const iterations = 100

	manager := NewManager()
	verbose := Configuration{Levels: map[string]string{"github.com/acme/application": "debug"}}
	quiet := Configuration{Levels: map[string]string{"github.com/acme/application": "warn"}}

	var group sync.WaitGroup
	failures := make(chan error, appliers)
	for worker := range appliers {
		group.Add(1)
		go func() {
			defer group.Done()
			for iteration := range iterations {
				configuration := verbose
				if (worker+iteration)%2 == 0 {
					configuration = quiet
				}
				if err := manager.Apply(configuration); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("apply failed: %v", err)
	}

	if got := manager.EffectiveLevel("github.com/acme/application"); got != slog.LevelDebug && got != slog.LevelWarn {
		t.Errorf("effective level converged on %v, want the level of one of the applied configurations", got)
	}
}

func TestConcurrentLoggerCreationAndListingIsSafe(t *testing.T) {
	const creators = 8
	const iterations = 100

	manager := NewManager(WithHandler(&recordingHandler{}))

	var group sync.WaitGroup
	for worker := range creators {
		group.Add(1)
		go func() {
			defer group.Done()
			for iteration := range iterations {
				name := fmt.Sprintf("github.com/acme/module%d/component%d", worker, iteration)
				manager.Logger(name).Info("created and used concurrently")
				_ = manager.Loggers()
			}
		}()
	}
	group.Wait()

	// Every creator bound one distinct name per iteration; the listing adds
	// the root entry on top.
	if got := len(manager.Loggers()); got != creators*iterations+1 {
		t.Errorf("listed %d loggers, want %d", got, creators*iterations+1)
	}
}
