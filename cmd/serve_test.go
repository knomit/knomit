package cmd

import (
	"testing"
)

func TestServeCmd_PortFlagRegistered(t *testing.T) {
	c := serveCmd()
	f := c.Flags().Lookup("port")
	if f == nil {
		t.Fatal("serve command missing --port flag")
	}
	if f.DefValue != "" {
		t.Errorf("--port default = %q, want empty (meaning: fall back to config)", f.DefValue)
	}
}

func TestServeCmd_HostFlagRegistered(t *testing.T) {
	c := serveCmd()
	f := c.Flags().Lookup("host")
	if f == nil {
		t.Fatal("serve command missing --host flag")
	}
	if f.DefValue != "" {
		t.Errorf("--host default = %q, want empty (meaning: fall back to config)", f.DefValue)
	}
}
