package configuration

// The tests in this file are written to be run under the race detector in CI
// (go test -race): they pin that one Loader is safely shared across
// goroutines and that pathological placeholder inputs stay bounded.

import (
	"errors"
	"strconv"
	"sync"
	"testing"
)

func TestConcurrentLoadsShareOneLoaderSafely(t *testing.T) {
	const workers = 8
	const iterations = 50

	loader := NewLoader(
		WithDocument([]byte(`{"name": "${SHOP_NAME:shop}", "server": {"host": "${HOST}", "port": "${PORT:8080}"}}`)),
		WithLookup(mapLookup(map[string]string{"HOST": "shop.internal"})),
	)

	var group sync.WaitGroup
	failures := make(chan error, workers)
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for range iterations {
				var target applicationConfiguration
				if err := loader.Load(&target); err != nil {
					failures <- err
					return
				}
				if target.Server.Host != "shop.internal" || target.Server.Port != 8080 {
					failures <- errors.New("unexpected bound values")
					return
				}
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent load failed: %v", err)
	}
}

func TestDeeplyNestedDefaultsResolveWithoutBlowingUp(t *testing.T) {
	const depth = 64

	text := "x"
	for range depth {
		text = "${MISSING:" + text + "}"
	}

	resolved, err := resolveText(text, emptyLookup, map[string]bool{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != "x" {
		t.Errorf("resolved = %q, want the innermost default", resolved)
	}
}

func TestALongChainOfValueReferencesResolves(t *testing.T) {
	const links = 64

	variables := map[string]string{"KEY0": "end"}
	for index := 1; index < links; index++ {
		variables["KEY"+strconv.Itoa(index)] = "${KEY" + strconv.Itoa(index-1) + "}"
	}

	resolved, err := resolveText("${KEY"+strconv.Itoa(links-1)+"}", mapLookup(variables), map[string]bool{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != "end" {
		t.Errorf("resolved = %q, want the value at the end of the chain", resolved)
	}
}

func TestConcurrentSchemaGenerationIsSafe(t *testing.T) {
	const workers = 8

	var group sync.WaitGroup
	failures := make(chan error, workers)
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := GenerateSchema(documentedConfiguration{}); err != nil {
				failures <- err
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent schema generation failed: %v", err)
	}
}
