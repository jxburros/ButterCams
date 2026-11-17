package migrations

import (
	"errors"
	"testing"
)

func TestRunAppliesStepsInOrder(t *testing.T) {
	reg := map[int]Migration{
		1: {From: 1, Apply: func(d map[string]any) error { d["alerts"] = map[string]any{}; return nil }},
		2: {From: 2, Apply: func(d map[string]any) error { d["ai"] = "on"; return nil }},
	}
	doc := map[string]any{"schemaVersion": float64(1), "cameras": []any{}}
	from, err := run(doc, 3, reg)
	if err != nil {
		t.Fatal(err)
	}
	if from != 1 || doc["schemaVersion"] != float64(3) || doc["ai"] != "on" || doc["alerts"] == nil {
		t.Fatalf("from=%d doc=%v", from, doc)
	}
}

func TestRunNoOpAtCurrentVersion(t *testing.T) {
	doc := map[string]any{"schemaVersion": float64(1)}
	from, err := Run(doc, 1)
	if err != nil || from != 1 {
		t.Fatalf("from=%d err=%v", from, err)
	}
}

func TestRunRefusesNewerAndBrokenDocuments(t *testing.T) {
	if _, err := Run(map[string]any{"schemaVersion": float64(9)}, 1); err == nil {
		t.Fatal("a newer schema must be refused")
	}
	if _, err := Run(map[string]any{}, 1); err == nil {
		t.Fatal("a missing schemaVersion must be refused")
	}
	if _, err := Run(map[string]any{"schemaVersion": "1"}, 1); err == nil {
		t.Fatal("a non-numeric schemaVersion must be refused")
	}
	if _, err := run(map[string]any{"schemaVersion": float64(1)}, 2, map[int]Migration{}); err == nil {
		t.Fatal("a missing step must be reported")
	}
	boom := errors.New("boom")
	reg := map[int]Migration{1: {From: 1, Apply: func(map[string]any) error { return boom }}}
	if _, err := run(map[string]any{"schemaVersion": float64(1)}, 2, reg); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestRegisteredMigrations(t *testing.T) {
	if got := Registered(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("expected exactly the v1 to v2 migration, found %v", got)
	}
}

// Schema 2 adds the live section with live view switched on (spec 16.8).
func TestV1ToV2AddsLive(t *testing.T) {
	sub := "/sub"
	doc := map[string]any{"schemaVersion": float64(1),
		"cameras": []any{map[string]any{"id": "a", "subStreamPath": sub}}}
	from, err := Run(doc, 2)
	if err != nil || from != 1 {
		t.Fatalf("from %d, %v", from, err)
	}
	live, ok := doc["live"].(map[string]any)
	if !ok || live["enabled"] != true || live["mainStream"] != true {
		t.Fatalf("live section: %#v", doc["live"])
	}
	if views, ok := live["views"].([]any); !ok || len(views) != 0 {
		t.Fatalf("views: %#v", live["views"])
	}
	if cam := doc["cameras"].([]any)[0].(map[string]any); cam["subStreamPath"] != sub {
		t.Fatal("the sub stream path of a camera was not kept")
	}
	// A document that already has the section (a hand-edited file) keeps it.
	doc = map[string]any{"schemaVersion": float64(1), "live": map[string]any{"enabled": false}}
	if _, err := Run(doc, 2); err != nil || doc["live"].(map[string]any)["enabled"] != false {
		t.Fatalf("an existing live section was replaced: %#v, %v", doc["live"], err)
	}
}
