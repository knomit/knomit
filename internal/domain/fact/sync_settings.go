package fact

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// The keys and values of the root attribute `sync` (AttrSync), an object
// with two keys, each set independently:
//
//	sync:
//	  push: realtime    # realtime | interval
//	  pull: realtime    # realtime | interval
const (
	SyncPush = "push" // how soon this instance's own commits go out
	SyncPull = "pull" // how often a sync round runs

	// SyncRealtime: push — every commit on this instance's agent branch opens
	// the 1 s push countdown, as a `do: push` trigger does for the commits it
	// matches; pull — a full sync round every [git].realtime_pull_interval.
	SyncRealtime = "realtime"
	// SyncInterval is today's behaviour, and what an absent key means: push
	// waits for the next round, pull runs at the loop's own interval.
	SyncInterval = "interval"
)

func validSyncValue(s string) bool { return s == SyncRealtime || s == SyncInterval }

// validSync is the registry's validator: a mapping whose keys are only
// `push` and `pull`, each "realtime" or "interval". A scalar (`sync:
// realtime`) is not a value of this attribute.
func validSync(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for k, x := range m {
		s, ok := x.(string)
		if !ok || !validSyncValue(s) {
			return false
		}
		if k != SyncPush && k != SyncPull {
			return false
		}
	}
	return true
}

// SyncSettings is what the sync loops read from one ontology file.
//
// Valid=false means the key is present with a value the registry rejects (a
// scalar, an unknown inner key, a value a newer knomit understands). Like
// consensus and conflicts, an unknown value is SAFE to read as today's
// behaviour — interval only means "sync at the loop's own pace" — so the
// caller does exactly that and warns once. Push and Pull are both "interval"
// whenever Valid is false: a bad value never turns realtime on.
type SyncSettings struct {
	Push  string // SyncRealtime or SyncInterval; absent reads SyncInterval
	Pull  string // SyncRealtime or SyncInterval; absent reads SyncInterval
	Valid bool
	Raw   any // the value as written when Valid is false, for the warning
}

// RealtimePush reports whether every agent-branch commit wakes the sync loop.
func (s SyncSettings) RealtimePush() bool { return s.Push == SyncRealtime }

// RealtimePull reports whether the loop's wait is the realtime pull interval.
func (s SyncSettings) RealtimePull() bool { return s.Pull == SyncRealtime }

// SyncAbsent is the setting of a repo that does not set `sync`: today's
// behaviour for both keys.
func SyncAbsent() SyncSettings {
	return SyncSettings{Push: SyncInterval, Pull: SyncInterval, Valid: true}
}

// ReadSync reads ONLY the root attributes block of an ontology file, the way
// ReadConsensus does: an unrelated problem in a topic must not change how
// often this instance syncs. Values go through the registry validator, never
// the raw map.
//
// An error means the file is not YAML, or its root attributes are not a
// mapping; the caller reads that as absent.
func ReadSync(data []byte) (SyncSettings, error) {
	absent := SyncAbsent()
	var doc struct {
		Attributes yaml.Node `yaml:"attributes"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return absent, fmt.Errorf("read sync: %w", err)
	}
	if doc.Attributes.Kind == 0 {
		return absent, nil
	}
	if doc.Attributes.Kind != yaml.MappingNode {
		return absent, errors.New("read sync: root attributes is not a mapping")
	}
	var attrs map[string]any
	if err := doc.Attributes.Decode(&attrs); err != nil {
		return absent, fmt.Errorf("read sync: %w", err)
	}
	v, ok := attrs[AttrSync]
	if !ok {
		return absent, nil
	}
	if !attributeRegistry[AttrSync].valid(v) {
		bad := absent
		bad.Valid, bad.Raw = false, v
		return bad, nil
	}
	out := absent
	m := v.(map[string]any)
	if s, ok := m[SyncPush]; ok {
		out.Push = s.(string)
	}
	if s, ok := m[SyncPull]; ok {
		out.Pull = s.(string)
	}
	return out, nil
}
