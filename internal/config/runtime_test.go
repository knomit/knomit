package config

import "testing"

func TestDefaults_RuntimeAddrOff(t *testing.T) {
	if got := Defaults().Runtime.Addr; got != "" {
		t.Fatalf("Runtime.Addr default = %q, want empty (diagnostics port off)", got)
	}
}
