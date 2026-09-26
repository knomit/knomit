package fact

import "testing"

// TestReadVerifySettings covers the read F09 does at every commit: the ROOT
// attributes only, values through the registry's validators, explicit off
// equal to absent, anything else UNKNOWN (Valid=false or an error).
func TestReadVerifySettings(t *testing.T) {
	const topics = "topics:\n  notes:\n    description: d\n"
	cases := []struct {
		name    string
		src     string
		mode    string
		signers int
		valid   bool
		err     bool
	}{
		{"absent", "id: x\nname: X\n" + topics, "off", 0, true, false},
		{"explicit off is absent", "id: x\nname: X\nattributes:\n  verify_signatures: off\n" + topics, "off", 0, true, false},
		{"log", "id: x\nname: X\nattributes:\n  verify_signatures: log\n" + topics, "log", 0, true, false},
		{"enforce with a signer", "id: x\nname: X\nattributes:\n  verify_signatures: enforce\n  verify_signers:\n    - " + testSignerKey + "\n" + topics, "enforce", 1, true, false},
		{"empty signer list is absent", "id: x\nname: X\nattributes:\n  verify_signers: []\n" + topics, "off", 0, true, false},
		{"bad mode is UNKNOWN", "id: x\nname: X\nattributes:\n  verify_signatures: yes\n" + topics, "", 0, false, false},
		{"bad signer is UNKNOWN", "id: x\nname: X\nattributes:\n  verify_signers:\n    - nope\n" + topics, "off", 0, false, false},
		// An unrelated TOPIC error must not make the verify setting unknown:
		// only the root block is read.
		{"unrelated topic error", "id: x\nname: X\nattributes:\n  verify_signatures: log\ntopics:\n  notes:\n    description: d\n    attributes:\n      learn_dedup: maybe\n", "log", 0, true, false},
		{"not yaml", "id: [x\n", "", 0, false, true},
		{"root attributes not a mapping", "id: x\nname: X\nattributes: 3\n" + topics, "", 0, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ReadVerifySettings([]byte(c.src))
			if (err != nil) != c.err {
				t.Fatalf("err = %v, want error %v", err, c.err)
			}
			if err != nil {
				return
			}
			if got.Valid != c.valid {
				t.Fatalf("Valid = %v, want %v", got.Valid, c.valid)
			}
			if !c.valid {
				return
			}
			if got.Mode != c.mode || len(got.Signers) != c.signers {
				t.Fatalf("got %+v, want mode %q with %d signer(s)", got, c.mode, c.signers)
			}
		})
	}
}

// TestVerifySettings_Equal: equality is what change detection uses, and
// explicit off must equal absent.
func TestVerifySettings_Equal(t *testing.T) {
	a := VerifySettings{Mode: "off", Valid: true}
	b := VerifySettings{Mode: "off", Signers: []string{}, Valid: true}
	if !a.Equal(b) {
		t.Fatal("off with no signers and off with an empty list must be equal")
	}
	c := VerifySettings{Mode: "log", Valid: true}
	if a.Equal(c) {
		t.Fatal("off and log must differ")
	}
	d := VerifySettings{Mode: "log", Signers: []string{testSignerKey}, Valid: true}
	if c.Equal(d) {
		t.Fatal("a signer list difference is a difference")
	}
}
