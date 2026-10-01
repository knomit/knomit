package fact

import (
	"strings"
	"testing"
)

// T8 (F21 S2): ReadSync reads the root block only. `sync` is an object with
// two independent keys, `push` and `pull`, each realtime|interval; an absent
// key reads interval. Any value the registry rejects — the scalar form, an
// unknown inner key, an unknown value, a non-string — reads INTERVAL for both
// keys with Valid=false: a bad value never turns realtime on. A topic-level
// `sync` is not the setting.
//
// SABOTAGE: accept an unknown value as realtime (ReadSync maps any value
// other than "interval" to realtime, or validSync accepts it) → the "unknown
// value" rows red.
func TestReadSync(t *testing.T) {
	cases := []struct {
		name       string
		src        string
		push, pull string
		valid      bool
		err        bool
	}{
		{"absent", conflictsOnt(""), SyncInterval, SyncInterval, true, false},
		{"no attributes block, other root keys", conflictsOnt("  consensus: auto\n"), SyncInterval, SyncInterval, true, false},
		{"both realtime", conflictsOnt("  sync:\n    push: realtime\n    pull: realtime\n"), SyncRealtime, SyncRealtime, true, false},
		{"push only", conflictsOnt("  sync:\n    push: realtime\n"), SyncRealtime, SyncInterval, true, false},
		{"pull only", conflictsOnt("  sync:\n    pull: realtime\n"), SyncInterval, SyncRealtime, true, false},
		{"explicit interval", conflictsOnt("  sync:\n    push: interval\n    pull: interval\n"), SyncInterval, SyncInterval, true, false},
		{"empty object", conflictsOnt("  sync: {}\n"), SyncInterval, SyncInterval, true, false},

		{"unknown value", conflictsOnt("  sync:\n    push: always\n"), SyncInterval, SyncInterval, false, false},
		{"unknown value beside a good one", conflictsOnt("  sync:\n    push: realtime\n    pull: fast\n"), SyncInterval, SyncInterval, false, false},
		{"unknown inner key", conflictsOnt("  sync:\n    push: realtime\n    fetch: realtime\n"), SyncInterval, SyncInterval, false, false},
		{"scalar form", conflictsOnt("  sync: realtime\n"), SyncInterval, SyncInterval, false, false},
		{"yaml bool", conflictsOnt("  sync:\n    push: true\n"), SyncInterval, SyncInterval, false, false},

		{"topic-level sync is not the setting", "id: x\nname: X\ntopics:\n  notes:\n    description: d\n    attributes:\n      sync:\n        push: realtime\n", SyncInterval, SyncInterval, true, false},
		{"not yaml", "id: [", SyncInterval, SyncInterval, false, true},
		{"attributes not a mapping", "id: x\nattributes: [a]\n", SyncInterval, SyncInterval, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := ReadSync([]byte(c.src))
			if c.err != (err != nil) {
				t.Fatalf("err = %v, want error %v", err, c.err)
			}
			if s.Push != c.push || s.Pull != c.pull {
				t.Fatalf("push/pull = %q/%q, want %q/%q", s.Push, s.Pull, c.push, c.pull)
			}
			if !c.err && s.Valid != c.valid {
				t.Fatalf("Valid = %v, want %v", s.Valid, c.valid)
			}
			if s.RealtimePush() != (c.push == SyncRealtime) || s.RealtimePull() != (c.pull == SyncRealtime) {
				t.Fatalf("RealtimePush/RealtimePull disagree with Push/Pull: %+v", s)
			}
			if !c.valid && !c.err && s.Raw == nil {
				t.Fatal("a rejected value carries Raw for the warning")
			}
		})
	}
}

// TestSyncAttribute_Registry (T8): every valid shape is accepted by a new
// ontology and opens; a bad value is a WARNING on the open path (the repo
// keeps opening, ReadSync reads it as absent) and FATAL only for a NEW
// ontology, exactly as for consensus and conflicts; on a topic it is the
// wrong scope.
//
// SABOTAGE: validSync accepts unknown inner keys or values → the bad rows are
// accepted by ParseNewOntology → red.
func TestSyncAttribute_Registry(t *testing.T) {
	for _, body := range []string{
		"  sync:\n    push: realtime\n",
		"  sync:\n    pull: realtime\n",
		"  sync:\n    push: interval\n    pull: realtime\n",
		"  sync: {}\n",
	} {
		src := conflictsOnt(body)
		if _, err := ParseNewOntology([]byte(src)); err != nil {
			t.Fatalf("%q must be accepted by a new ontology: %v", body, err)
		}
		o, err := ParseOntology([]byte(src))
		if err != nil {
			t.Fatalf("open %q: %v", body, err)
		}
		if _, ok := o.Attributes[AttrSync].(map[string]any); !ok {
			t.Fatalf("root attributes = %v, want a sync mapping", o.Attributes)
		}
	}
	for _, body := range []string{
		"  sync: realtime\n",
		"  sync:\n    push: always\n",
		"  sync:\n    pull: off\n",
		"  sync:\n    push: realtime\n    fetch: realtime\n", // an unknown KEY with a valid value
	} {
		bad := conflictsOnt(body)
		o, diags := ValidateOntologyYAML([]byte(bad))
		if o == nil {
			t.Fatalf("%q: no ontology", body)
		}
		warned := false
		for _, d := range diags {
			if d.IsError() {
				t.Fatalf("%q: a bad root value must not be an error on the open path: %s", body, d.Message)
			}
			if strings.Contains(d.Message, `root attribute "sync" must be a mapping with "push"`) {
				warned = true
			}
		}
		if !warned {
			t.Fatalf("%q: ValidateOntologyYAML must warn naming the shape, got %+v", body, diags)
		}
		if _, err := ParseOntology([]byte(bad)); err != nil {
			t.Fatalf("%q: a bad root value must not fail the open path: %v", body, err)
		}
		if _, err := ParseNewOntology([]byte(bad)); err == nil {
			t.Fatalf("%q: a new ontology with a bad sync value must be refused", body)
		}
	}
	onTopic := "id: x\nname: X\ntopics:\n  notes:\n    description: d\n    attributes:\n      sync:\n        push: realtime\n"
	if _, err := ParseNewOntology([]byte(onTopic)); err == nil {
		t.Fatal("sync on a topic is the wrong scope and must be refused for a new ontology")
	}
}
