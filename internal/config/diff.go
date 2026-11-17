package config

import (
	"encoding/json"
	"reflect"
)

// Changes describes what differs between two configurations, so the guardian
// restarts only what changed (spec 5.3).
type Changes struct {
	CamerasAdded   []string
	CamerasRemoved []string
	CamerasChanged []string // any field of the camera changed; its worker decides whether to reconnect

	DestinationsChanged bool // any destination added, removed or modified
	StagingPathChanged  bool
	StagingFloorChanged bool
	ServerChanged       bool
	LoggingChanged      bool

	// LiveChanged: live view was switched on or off, or the main stream was.
	// Shared views are not part of it: saving one touches nothing that runs.
	LiveChanged bool
}

// Empty reports whether nothing changed.
func (c Changes) Empty() bool {
	return len(c.CamerasAdded) == 0 && len(c.CamerasRemoved) == 0 && len(c.CamerasChanged) == 0 &&
		!c.DestinationsChanged && !c.StagingPathChanged && !c.StagingFloorChanged &&
		!c.ServerChanged && !c.LoggingChanged && !c.LiveChanged
}

func equalJSON(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// Diff compares two configurations.
func Diff(old, new *Config) Changes {
	var ch Changes

	oldCams := map[string]Camera{}
	for _, c := range old.Cameras {
		oldCams[c.ID] = c
	}
	newCams := map[string]bool{}
	for _, c := range new.Cameras {
		newCams[c.ID] = true
		prev, ok := oldCams[c.ID]
		switch {
		case !ok:
			ch.CamerasAdded = append(ch.CamerasAdded, c.ID)
		case !equalJSON(prev, c):
			ch.CamerasChanged = append(ch.CamerasChanged, c.ID)
		}
	}
	for _, c := range old.Cameras {
		if !newCams[c.ID] {
			ch.CamerasRemoved = append(ch.CamerasRemoved, c.ID)
		}
	}

	ch.DestinationsChanged = !equalJSON(old.Destinations, new.Destinations)
	ch.StagingPathChanged = old.Staging.Path != new.Staging.Path
	ch.StagingFloorChanged = old.Staging.MinFreePercent != new.Staging.MinFreePercent ||
		old.Staging.MinFreeGB != new.Staging.MinFreeGB
	ch.ServerChanged = !reflect.DeepEqual(old.Server, new.Server)
	ch.LoggingChanged = old.Logging != new.Logging
	ch.LiveChanged = old.Live.Enabled != new.Live.Enabled || old.Live.MainStream != new.Live.MainStream
	return ch
}
