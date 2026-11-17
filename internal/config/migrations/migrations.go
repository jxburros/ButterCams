// Package migrations upgrades config.json documents written by older builds.
//
// Each schema step lives in its own file named after the step, for example
// config_v1_to_v2.go, and registers itself from init():
//
//	func init() {
//		Register(Migration{From: 1, Apply: func(doc map[string]any) error {
//			doc["alerts"] = map[string]any{}
//			return nil
//		}})
//	}
//
// Migrations work on the raw JSON document so that they do not depend on Go
// types that keep changing.
package migrations

import (
	"fmt"
	"sort"
)

// Migration upgrades a document from schema version From to From+1.
type Migration struct {
	From  int
	Apply func(doc map[string]any) error
}

var registry = map[int]Migration{}

// Register adds a migration. It panics on a duplicate, which is a programming
// error caught by the first test run.
func Register(m Migration) {
	if _, dup := registry[m.From]; dup {
		panic(fmt.Sprintf("migrations: duplicate migration from v%d", m.From))
	}
	registry[m.From] = m
}

// Registered lists the source versions that have a migration, in order.
func Registered() []int {
	out := make([]int, 0, len(registry))
	for v := range registry {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// Version reads the schemaVersion of a raw document.
func Version(doc map[string]any) (int, error) {
	v, ok := doc["schemaVersion"]
	if !ok {
		return 0, fmt.Errorf("config has no schemaVersion")
	}
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) || f < 1 {
		return 0, fmt.Errorf("config has an invalid schemaVersion %v", v)
	}
	return int(f), nil
}

// Run upgrades doc in place to the target schema version. It returns the
// version the document had. A document newer than target is refused: an older
// build must never rewrite a newer configuration.
func Run(doc map[string]any, target int) (from int, err error) {
	return run(doc, target, registry)
}

func run(doc map[string]any, target int, reg map[int]Migration) (from int, err error) {
	from, err = Version(doc)
	if err != nil {
		return 0, err
	}
	if from > target {
		return from, fmt.Errorf("config schema v%d is newer than this build supports (v%d)", from, target)
	}
	for v := from; v < target; v++ {
		m, ok := reg[v]
		if !ok {
			return from, fmt.Errorf("no migration from config schema v%d to v%d", v, v+1)
		}
		if err := m.Apply(doc); err != nil {
			return from, fmt.Errorf("migrating config v%d to v%d: %w", v, v+1, err)
		}
		doc["schemaVersion"] = float64(v + 1)
	}
	return from, nil
}
