package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
)

// Configuration is the externally provided document that overrides the
// levels a program compiled in, the analog of the logging.level and
// logging.group property families of Spring Boot. Its JSON form is:
//
//	{
//	    "levels": {
//	        "root": "warn",
//	        "github.com/cgardev/gokeel/outbox": "debug"
//	    },
//	    "groups": {
//	        "persistence": [
//	            "github.com/cgardev/gokeel/outbox",
//	            "github.com/cgardev/gokeel/transaction"
//	        ]
//	    }
//	}
type Configuration struct {
	// Levels assigns a level token to each logger name; the pseudo-name
	// "root" addresses the root logger. A name that designates a group
	// with members fans the level out to every member.
	Levels map[string]string `json:"levels,omitempty"`

	// Groups declares named groups of loggers, so one Levels entry can
	// retune several packages at once.
	Groups map[string][]string `json:"groups,omitempty"`
}

// ParseConfiguration decodes a JSON Configuration document and validates it.
// Unknown fields are rejected, so a misspelled key fails loudly instead of
// being silently ignored.
func ParseConfiguration(document []byte) (Configuration, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	var configuration Configuration
	if err := decoder.Decode(&configuration); err != nil {
		return Configuration{}, fmt.Errorf("decode logging configuration: %w", err)
	}
	if decoder.More() {
		return Configuration{}, errors.New("logging configuration document contains more than one JSON value")
	}
	if err := configuration.Validate(); err != nil {
		return Configuration{}, err
	}
	return configuration, nil
}

// ParseLevels parses a compact list of name=level assignments separated by
// commas, the shape an environment variable or a command-line flag carries,
// for example "root=warn,github.com/cgardev/gokeel/outbox=debug". The result
// plugs directly into the Levels field of a Configuration. An empty input
// yields an empty map, so an unset environment variable applies nothing.
func ParseLevels(list string) (map[string]string, error) {
	levels := map[string]string{}
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, token, found := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		token = strings.TrimSpace(token)
		if !found || name == "" || token == "" {
			return nil, fmt.Errorf("level entry %q must have the form name=level", entry)
		}
		if _, err := ParseLevel(token); err != nil {
			return nil, fmt.Errorf("level of %q: %w", name, err)
		}
		levels[name] = token
	}
	return levels, nil
}

// Validate reports the first problem that would prevent the Configuration
// from being applied: a level token that does not parse, a group that
// designates the root logger, or a group member that does not name a logger.
// Entries are checked in lexicographic order, so the reported problem is
// deterministic.
func (configuration Configuration) Validate() error {
	for _, name := range slices.Sorted(maps.Keys(configuration.Levels)) {
		if _, err := ParseLevel(configuration.Levels[name]); err != nil {
			return fmt.Errorf("level of %q: %w", name, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(configuration.Groups)) {
		if canonicalName(name) == "" {
			return fmt.Errorf("group %q designates the root logger", name)
		}
		for _, member := range configuration.Groups[name] {
			if canonicalName(member) == "" {
				return fmt.Errorf("group %q member %q must name a logger other than the root", name, member)
			}
		}
	}
	return nil
}

// Apply overlays the Configuration over the levels the Manager currently
// holds, the moment Spring Boot applies logging.level properties over the
// levels its logging file declares: every name the document configures is
// overwritten, every other name keeps its current assignment. Groups the
// document declares are registered before any level is applied and remain
// available to later SetLevel and ResetLevel calls.
//
// Two rules make the outcome deterministic where Spring leaves it to
// property iteration order: levels are applied in lexicographic name order,
// and a level assigned directly to a name wins over a level the name
// receives through a group. The Configuration is validated up front, and a
// validation failure leaves the Manager untouched.
func (manager *Manager) Apply(configuration Configuration) error {
	if err := configuration.Validate(); err != nil {
		return fmt.Errorf("apply logging configuration: %w", err)
	}

	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	for _, name := range slices.Sorted(maps.Keys(configuration.Groups)) {
		members := configuration.Groups[name]
		canonicalMembers := make([]string, 0, len(members))
		for _, member := range members {
			canonicalMembers = append(canonicalMembers, canonicalName(member))
		}
		manager.groups[canonicalName(name)] = canonicalMembers
	}

	fanned := map[string]slog.Level{}
	direct := map[string]slog.Level{}
	for _, name := range slices.Sorted(maps.Keys(configuration.Levels)) {
		level, err := ParseLevel(configuration.Levels[name])
		if err != nil {
			return fmt.Errorf("level of %q: %w", name, err)
		}
		canonical := canonicalName(name)
		if members, ok := manager.groups[canonical]; ok && len(members) > 0 {
			for _, member := range members {
				fanned[member] = level
			}
			continue
		}
		direct[canonical] = level
	}

	manager.rebuild(func(tree *levelTree) {
		for _, assignments := range []map[string]slog.Level{fanned, direct} {
			for name, level := range assignments {
				if name == "" {
					tree.root = level
				} else {
					tree.assigned[name] = level
				}
			}
		}
	})
	return nil
}
