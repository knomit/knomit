package fact

import "testing"

// TestReadVerifySettings covers the read the acceptance gate does at the
// upstream tip: the ROOT attributes only, values through the registry's
// validators, explicit off equal to absent, anything else UNKNOWN (Valid=false
// or an error).
func TestReadVerifySettings(t *testing.T) {
	const topics = "topics:\n  notes:\n    description: d\n"
	cases := []struct {
		name  string
		src   string
		mode  string
		valid bool
		err   bool
	}{
		{"absent", "id: x\nname: X\n" + topics, "off", true, false},
		{"explicit off is absent", "id: x\nname: X\nattributes:\n  verify_signatures: off\n" + topics, "off", true, false},
		{"log", "id: x\nname: X\nattributes:\n  verify_signatures: log\n" + topics, "log", true, false},
		{"enforce", "id: x\nname: X\nattributes:\n  verify_signatures: enforce\n" + topics, "enforce", true, false},
		// verify_signers no longer exists: the key list is never read, so a
		// dev-built ontology that carries one reads its mode unchanged.
		{"verify_signers is ignored", "id: x\nname: X\nattributes:\n  verify_signatures: log\n  verify_signers:\n    - " + testSignerKey + "\n" + topics, "log", true, false},
		{"bad mode is UNKNOWN", "id: x\nname: X\nattributes:\n  verify_signatures: yes\n" + topics, "", false, false},
		// An unrelated TOPIC error must not make the verify setting unknown:
		// only the root block is read.
		{"unrelated topic error", "id: x\nname: X\nattributes:\n  verify_signatures: log\ntopics:\n  notes:\n    description: d\n    attributes:\n      learn_dedup: maybe\n", "log", true, false},
		{"not yaml", "id: [x\n", "", false, true},
		{"root attributes not a mapping", "id: x\nname: X\nattributes: 3\n" + topics, "", false, true},
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
			if c.valid && got.Mode != c.mode {
				t.Fatalf("got %+v, want mode %q", got, c.mode)
			}
		})
	}
}
