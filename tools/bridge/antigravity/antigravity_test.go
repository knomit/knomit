package antigravity

import (
	"strings"
	"testing"
)

func TestRun_NoSubcommand_Errors(t *testing.T) {
	err := Run(nil)
	if err == nil {
		t.Fatal("Run(nil) = nil, want usage error")
	}
	if !strings.Contains(err.Error(), "init") || !strings.Contains(err.Error(), "hook") {
		t.Errorf("usage error should name both subcommands; got %q", err)
	}
}

func TestRun_UnknownSubcommand_Errors(t *testing.T) {
	err := Run([]string{"frobnicate"})
	if err == nil {
		t.Fatal("Run([frobnicate]) = nil, want error")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error should quote the unknown subcommand; got %q", err)
	}
	if !strings.Contains(err.Error(), "init") || !strings.Contains(err.Error(), "hook") {
		t.Errorf("error should name the valid subcommands; got %q", err)
	}
}

func TestRunHook_UnknownEvent_Errors(t *testing.T) {
	err := runHook([]string{"post-compact"})
	if err == nil {
		t.Fatal("runHook([post-compact]) = nil, want error")
	}
	if !strings.Contains(err.Error(), "pre-invocation") {
		t.Errorf("error should name the valid event; got %q", err)
	}
}

func TestRunHook_NoEvent_Errors(t *testing.T) {
	if err := runHook(nil); err == nil {
		t.Fatal("runHook(nil) = nil, want usage error")
	}
}
