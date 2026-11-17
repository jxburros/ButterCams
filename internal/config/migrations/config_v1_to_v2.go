package migrations

// Schema 2 (spec 16.8) adds the "live" section: live view and playback are
// on, the main-stream restream is on, and there are no shared views yet.
// Cameras keep their subStreamPath, which v1 already stored.
func init() {
	Register(Migration{From: 1, Apply: func(doc map[string]any) error {
		if _, ok := doc["live"].(map[string]any); !ok {
			doc["live"] = map[string]any{"enabled": true, "mainStream": true, "views": []any{}}
		}
		return nil
	}})
}
