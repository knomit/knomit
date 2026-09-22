package app

import (
	"context"
	"strings"
	"testing"

	"knomit/internal/config"
)

// [auth].require = true turns the anonymous loopback path off. With no local
// authenticated listener configured there is then no way in at all, and a
// server that booted anyway would look healthy while answering every request
// 403. Boot must refuse instead, and say why.
func TestBoot_RequireWithoutLocalListenerFails(t *testing.T) {
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	cfg.Auth.Require = true
	cfg.Socket = ""

	a, err := New(context.Background(), cfg, Options{APIOnly: true})
	if err == nil {
		a.Close()
		t.Fatal("boot must fail when require = true and no local listener is configured")
	}
	// "[auth].require", not "require": the embedder's own failure says
	// "embeddings are required", and a bare needle would pass on that.
	for _, want := range []string{"[auth].require", "socket"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must name %q so an operator knows what to change; got: %v", want, err)
		}
	}
}

// Positive control: the same check passes once a listener is configured, and
// never fires with require off. Without this the test above would pass for a
// New that failed on anything at all. It calls the check directly because a
// full New needs the embedder, which this one question does not.
func TestCheckLocalListener(t *testing.T) {
	for _, tc := range []struct {
		name    string
		require bool
		socket  string
		wantErr bool
	}{
		{"require, no listener", true, "", true},
		{"require, listener", true, "/tmp/k/knomit.sock", false},
		{"no require, no listener", false, "", false},
		{"no require, listener", false, "/tmp/k/knomit.sock", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Auth.Require = tc.require
			cfg.Socket = tc.socket
			if err := checkLocalListener(cfg); (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
