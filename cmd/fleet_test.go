package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/auth"
)

// TestFleetCLI_ThinClientOverTheLocalListener: `knomit fleet` only calls
// /api/v1/fleet on the local listener and prints what the server says,
// including last_error; a refusal's detail comes back as the error.
func TestFleetCLI_ThinClientOverTheLocalListener(t *testing.T) {
	sock := oauthLocalListenerPath(t)
	ln, cleanup, err := auth.ListenLocal(sock)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	var gotMethod string
	var gotBody map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/fleet", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if r.Method == http.MethodDelete {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"this instance is standalone","code":"not_registered"}`))
			return
		}
		_, _ = w.Write([]byte(`{"state":"unregistering","agent_id":"box","fleet_repo":"fleet","record_state":"active","last_error":"dial tcp: refused"}`))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	hc := localAPIClient(sock)
	ctx := context.Background()

	var out bytes.Buffer
	require.NoError(t, fleetCall(ctx, hc, &out, http.MethodGet, nil))
	require.Equal(t, http.MethodGet, gotMethod)
	require.Contains(t, out.String(), "unregistering")
	require.Contains(t, out.String(), "dial tcp: refused", "last_error is shown verbatim")

	out.Reset()
	require.NoError(t, fleetCall(ctx, hc, &out, http.MethodPut, map[string]string{"url": "https://h/fleet.git"}))
	require.Equal(t, http.MethodPut, gotMethod)
	require.Equal(t, "https://h/fleet.git", gotBody["url"])

	err = fleetCall(ctx, hc, &out, http.MethodDelete, nil)
	require.ErrorContains(t, err, "this instance is standalone")
}

// F10: status prints the configured addresses, the record's advertised
// fields, whether it is current or pending, and the no-address notice.
func TestFleetCLI_PrintsAddressesAndRecord(t *testing.T) {
	var st fleetStatusJSON
	require.NoError(t, json.Unmarshal([]byte(`{"state":"registered","agent_id":"box","fleet_repo":"fleet","record_state":"active",
		"external_addresses":["https://new.example"],
		"record":{"addresses":["https://old.example"],"capabilities":{"os":"linux","arch":"amd64"}},
		"record_current":false,"record_pending_update":false}`), &st))
	var out bytes.Buffer
	printFleetStatus(&out, st)
	require.Contains(t, out.String(), "addresses (knomit.toml): https://new.example")
	require.Contains(t, out.String(), "addresses (record):      https://old.example")
	require.Contains(t, out.String(), "capabilities: arch=amd64 os=linux")
	require.Contains(t, out.String(), "restart or re-register")

	st = fleetStatusJSON{}
	require.NoError(t, json.Unmarshal([]byte(`{"state":"registered","agent_id":"box","fleet_repo":"fleet","record_state":"active",
		"external_addresses":[],"record":{"addresses":[],"capabilities":{}},"record_current":true,"record_pending_update":true,
		"notice":"no external addresses configured; set `+"`external_addresses`"+` in knomit.toml"}`), &st))
	out.Reset()
	printFleetStatus(&out, st)
	require.Contains(t, out.String(), "pending update")
	require.Contains(t, out.String(), "notice: no external addresses configured")
}
