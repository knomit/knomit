package fact

import (
	"errors"
	"strings"
	"testing"
)

// TestFleetPreset: the fleet preset is embedded, is recognised by its id, is
// reachable by preset name and by id (boot refresh), and holds only members.
func TestFleetPreset(t *testing.T) {
	o := FleetOntology()
	if !IsFleetOntology(o) {
		t.Fatalf("fleet preset id = %q", o.ID)
	}
	if byName, err := OntologyByPreset("fleet"); err != nil || byName != o {
		t.Fatalf("OntologyByPreset(fleet) = %v, %v", byName, err)
	}
	if EmbeddedPresetByID(FleetOntologyID) != o {
		t.Fatal("EmbeddedPresetByID must return the fleet preset")
	}
	if _, ok := o.Topics[MembersTopic]; !ok || len(o.Topics) != 1 {
		t.Fatalf("topics = %v, want exactly members", o.Topics)
	}
	if IsFleetOntology(DefaultOntology()) || IsFleetOntology(CodeOntology()) {
		t.Fatal("a KB preset must not be a fleet")
	}
}

// TestParseMember_RoundTrip: a record renders and parses back, including an
// opaque agent id with no fingerprint pattern at all.
func TestParseMember_RoundTrip(t *testing.T) {
	body := "agent: research-box\nstate: active\nkey: " + testSignerKey + "\nhost: laptop\nbranch: agent/research-box\n\nsome notes\n"
	m, err := ParseMember(body)
	if err != nil {
		t.Fatal(err)
	}
	if m.Agent != "research-box" || m.State != MemberActive || m.Host != "laptop" || m.Branch != "agent/research-box" || m.Notes != "some notes" {
		t.Fatalf("parsed %+v", m)
	}
	back, err := ParseMember(RenderMember(m))
	if err != nil {
		t.Fatal(err)
	}
	if back.Agent != m.Agent || back.State != m.State || !SameKey(back.Key, m.Key) || back.Notes != m.Notes {
		t.Fatalf("round trip %+v != %+v", back, m)
	}
}

// TestParseMember_Malformed: each missing or bad field is ErrMalformedMember.
func TestParseMember_Malformed(t *testing.T) {
	for name, body := range map[string]string{
		"no agent":      "state: active\nkey: " + testSignerKey + "\n",
		"no state":      "agent: a\nkey: " + testSignerKey + "\n",
		"unknown state": "agent: a\nstate: paused\nkey: " + testSignerKey + "\n",
		"no key":        "agent: a\nstate: active\n",
		"bad key":       "agent: a\nstate: active\nkey: ssh-ed25519 notbase64\n",
		"rsa key":       "agent: a\nstate: active\nkey: ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC7 x\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseMember(body); !errors.Is(err, ErrMalformedMember) {
				t.Fatalf("err = %v, want ErrMalformedMember", err)
			}
		})
	}
	if !strings.Contains(RenderMember(Member{Agent: "a", State: MemberLeft}), "state: left") {
		t.Fatal("render must write the state")
	}
}
