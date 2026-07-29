package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/homelock"
)

// TestServeRefusesAContendedHome. A second server on one KNOMIT_HOME does not
// collide and stop — it boots through Bootstrap and tracks control.db, every
// repo and every archive, spawning a second litestream agent against the SAME
// replica prefix. That is two writers on one LTX chain, which knomit
// deliberately never auto-repairs. The port collision does not save it: that is
// detected asynchronously, after all the tracking, and a second server on a
// different --port never collides at all.
//
// So this refusal is the only thing standing between a duplicated deployment
// and a corrupted backup, and it has to happen before Bootstrap.
func TestServeRefusesAContendedHome(t *testing.T) {
	// serve reconfigures the process logger; put it back for the rest of the
	// package.
	defer func(l zerolog.Logger, lvl zerolog.Level) {
		log.Logger = l
		zerolog.SetGlobalLevel(lvl)
	}(log.Logger, zerolog.GlobalLevel())

	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	t.Setenv("KNOMIT_BACKUP_ENABLED", "false")

	held, err := homelock.Acquire(home)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	_, err = runCmd(t, serveCmd())
	if err == nil {
		t.Fatal("serve started while another process held KNOMIT_HOME")
	}
	if !errors.Is(err, homelock.ErrHeld) {
		t.Fatalf("error = %v, want it to wrap homelock.ErrHeld", err)
	}
	// The message has to say why this matters and that a crash does not wedge a
	// restart — otherwise the first instinct is to delete the lock file.
	for _, want := range []string{"same prefix", "crashed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

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
