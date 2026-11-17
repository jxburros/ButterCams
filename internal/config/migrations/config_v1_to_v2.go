package migrations

// v2 adds live view and timeline playback (spec 16.8): a "live" section that
// turns them on, with the restream of main streams and no shared views yet.
func init() {
	Register(Migration{From: 1, Apply: func(doc map[string]any) error {
		if _, ok := doc["live"].(map[string]any); !ok {
			doc["live"] = map[string]any{"enabled": true, "mainStream": true, "views": []any{}}
		}
		return nil
	}})
}
