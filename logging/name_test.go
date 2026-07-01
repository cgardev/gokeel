package logging

import (
	"slices"
	"testing"
)

// sampleService exists so the tests can exercise type-level logger names.
type sampleService struct{}

func TestCanonicalNameAliasesRootAndTrimsSeparators(t *testing.T) {
	names := []struct {
		name      string
		canonical string
	}{
		{"root", ""},
		{"ROOT", ""},
		{" Root ", ""},
		{"", ""},
		{"/github.com/acme/", "github.com/acme"},
		{"a.b.", "a.b"},
		{" github.com/acme ", "github.com/acme"},
		{"///", ""},
		{" . ", ""},
		{" / root / ", ""},
	}
	for _, entry := range names {
		if got := canonicalName(entry.name); got != entry.canonical {
			t.Errorf("canonicalName(%q) = %q, want %q", entry.name, got, entry.canonical)
		}
	}
}

func TestAncestryCutsWholeSegmentsFromTheRight(t *testing.T) {
	got := ancestry("github.com/acme/application.Service")
	want := []string{
		"github.com/acme/application.Service",
		"github.com/acme/application",
		"github.com/acme",
		"github.com",
		"github",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ancestry = %q, want %q", got, want)
	}
}

func TestAncestryOfTheRootIsEmpty(t *testing.T) {
	if got := ancestry("root"); got != nil {
		t.Errorf("ancestry of the root = %q, want an empty chain", got)
	}
}

func TestNameForBuildsThePackageQualifiedTypeName(t *testing.T) {
	want := "github.com/cgardev/gokeel/logging.sampleService"
	if got := NameFor[sampleService](); got != want {
		t.Errorf("NameFor of the value type = %q, want %q", got, want)
	}
	if got := NameFor[*sampleService](); got != want {
		t.Errorf("NameFor of the pointer type = %q, want %q", got, want)
	}
	if got := NameFor[int](); got != "int" {
		t.Errorf("NameFor of a predeclared type = %q, want %q", got, "int")
	}
}
