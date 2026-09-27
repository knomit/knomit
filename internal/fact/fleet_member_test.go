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

// TestMember_AdvertisedFields (F10): addresses, git and capabilities are one
// line each after the F09 fields, round-trip, and render deterministically
// (capabilities sorted) so an unchanged instance renders the same bytes.
func TestMember_AdvertisedFields(t *testing.T) {
	m := Member{Agent: "a", State: MemberActive, Host: "h", Branch: "agent/a",
		Addresses:    []string{"https://h1v302.tail5113a7.ts.net", "http://10.0.0.5:19278"},
		Git:          "/git",
		Capabilities: map[string]string{"os": "linux", "arch": "amd64", "version": "0.5.0.abc", "read_only": "false"}}
	k, err := ParseMember("agent: x\nstate: active\nkey: " + testSignerKey + "\n")
	if err != nil {
		t.Fatal(err)
	}
	m.Key = k.Key
	body := RenderMember(m)
	for _, want := range []string{
		"branch: agent/a\naddresses: https://h1v302.tail5113a7.ts.net http://10.0.0.5:19278\ngit: /git\ncapabilities: arch=amd64 os=linux read_only=false version=0.5.0.abc\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %q lacks %q", body, want)
		}
	}
	back, err := ParseMember(body)
	if err != nil {
		t.Fatal(err)
	}
	if !m.SameAdvertised(back) {
		t.Fatalf("round trip lost advertised fields: %+v", back)
	}
	if RenderMember(back) != body {
		t.Fatalf("render is not stable:\n%s\n%s", RenderMember(back), body)
	}
	// Empty addresses: no line, parsed back as an empty (non-nil) list.
	m.Addresses = nil
	body = RenderMember(m)
	if strings.Contains(body, "addresses:") {
		t.Fatalf("empty addresses must not render a line: %q", body)
	}
	back, _ = ParseMember(body)
	if back.Addresses == nil || len(back.Addresses) != 0 {
		t.Fatalf("addresses = %#v, want []", back.Addresses)
	}
}

// TestMember_SameAdvertised: each advertised field is compared; agent, state
// and notes are not (the instance never rewrites them on its own).
func TestMember_SameAdvertised(t *testing.T) {
	k, _ := ParseMember("agent: x\nstate: active\nkey: " + testSignerKey + "\n")
	base := Member{Agent: "a", State: MemberActive, Key: k.Key, Host: "h", Branch: "b", Addresses: []string{"https://x"}, Git: "/git",
		Capabilities: map[string]string{"os": "linux"}}
	same := base
	same.State, same.Notes = MemberLeft, "n"
	if !base.SameAdvertised(same) || base.AdvertisedDiff(same) != nil {
		t.Fatal("state and notes are not advertised fields")
	}
	for field, mut := range map[string]func(*Member){
		"host":         func(m *Member) { m.Host = "other" },
		"branch":       func(m *Member) { m.Branch = "other" },
		"addresses":    func(m *Member) { m.Addresses = []string{"https://y"} },
		"git":          func(m *Member) { m.Git = "" },
		"capabilities": func(m *Member) { m.Capabilities = map[string]string{"os": "darwin"} },
		"key":          func(m *Member) { m.Key = nil },
	} {
		o := base
		mut(&o)
		if d := base.AdvertisedDiff(o); len(d) != 1 || d[0] != field {
			t.Errorf("%s: diff = %v", field, d)
		}
	}
}

// TestParseMember_OddAdvertisedValues: someone else's odd advertised values
// are kept as written (displayed as-is), never a reason to skip the record.
func TestParseMember_OddAdvertisedValues(t *testing.T) {
	m, err := ParseMember("agent: a\nstate: active\nkey: " + testSignerKey + "\naddresses: not-a-url  ftp://x/y\ncapabilities: weird os=plan9 =x\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(m.Addresses, "|") != "not-a-url|ftp://x/y" {
		t.Fatalf("addresses %q", m.Addresses)
	}
	if m.Capabilities["weird"] != "" || m.Capabilities["os"] != "plan9" {
		t.Fatalf("capabilities %v", m.Capabilities)
	}
}
