package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/foundation/auth"
)

// TestFleetCLI_ThinClientOverTheLocalListener: `knomit fleet status` only
// GETs /api/v1/fleet on the local listener and prints what the server says,
// including last_error, the role and where the fleet is hosted; a refusal's
// detail comes back as the error.
func TestFleetCLI_ThinClientOverTheLocalListener(t *testing.T) {
	sock := oauthLocalListenerPath(t)
	ln, cleanup, err := auth.ListenLocal(sock)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	var methods []string
	refuse := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/fleet", func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if refuse {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"control.db is not open"}`))
			return
		}
		_, _ = w.Write([]byte(`{"state":"registered","agent_id":"box","fleet_repo":"fleet","record_state":"active","last_error":"dial tcp: refused",
			"master":true,"hosting":"none","root_dir":"/h/fleet"}`))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	hc := localAPIClient(sock)
	ctx := context.Background()

	var out bytes.Buffer
	require.NoError(t, fleetCall(ctx, hc, &out))
	require.Contains(t, out.String(), "registered")
	require.Contains(t, out.String(), "dial tcp: refused", "last_error is shown verbatim")
	require.Contains(t, out.String(), "hosting: none")
	require.Contains(t, out.String(), "fleet master: true (fleet root directory: /h/fleet)")

	refuse = true
	err = fleetCall(ctx, hc, &out)
	require.ErrorContains(t, err, "control.db is not open")
	require.Equal(t, []string{http.MethodGet, http.MethodGet}, methods, "status only ever GETs")
}

// F10: status prints the configured addresses, the record's advertised
// fields, whether it is current or pending, and the no-address notice.
func TestFleetCLI_PrintsAddressesAndRecord(t *testing.T) {
	var st fleetStatusJSON
	require.NoError(t, json.Unmarshal([]byte(`{"state":"registered","agent_id":"box","fleet_repo":"fleet","record_state":"active",
		"host":{"external_addresses":["https://new.example"]},
		"record":{"addresses":["https://old.example"],"capabilities":{"os":"linux","arch":"amd64"}},
		"record_current":false,"record_pending_update":false}`), &st))
	var out bytes.Buffer
	printFleetStatus(&out, st)
	require.Contains(t, out.String(), "addresses (knomit.toml): https://new.example")
	require.Contains(t, out.String(), "addresses (record):      https://old.example")
	require.Contains(t, out.String(), "capabilities: arch=amd64 os=linux")
	require.Contains(t, out.String(), "restart to update it")

	st = fleetStatusJSON{}
	require.NoError(t, json.Unmarshal([]byte(`{"state":"registered","agent_id":"box","fleet_repo":"fleet","record_state":"active",
		"host":{"external_addresses":[]},"record":{"addresses":[],"capabilities":{}},"record_current":true,"record_pending_update":true,
		"notice":"no external addresses configured; set `+"`external_addresses`"+` in knomit.toml"}`), &st))
	out.Reset()
	printFleetStatus(&out, st)
	require.Contains(t, out.String(), "pending update")
	require.Contains(t, out.String(), "notice: no external addresses configured")
}

// TestFleetCLI_EveryVerbIsOneRequestToItsRoute: each `knomit fleet` verb sends
// exactly one request, with the method, path and body its route takes, and
// prints the server's answer; a 204 prints nothing, and a refusal's detail is
// the error. The table is checked against the server's RequireLocalVia group
// so a route added there without a verb (or a verb whose route went) fails.
func TestFleetCLI_EveryVerbIsOneRequestToItsRoute(t *testing.T) {
	bundleFile := filepath.Join(t.TempDir(), "box.bundle")
	require.NoError(t, os.WriteFile(bundleFile, []byte("BUNDLE-FROM-FILE"), 0o600))
	cases := []struct {
		args   []string
		stdin  string
		method string
		path   string
		body   string // "" for no body
	}{
		{args: []string{"create"}, method: "POST", path: "/fleet/create", body: `{}`},
		{args: []string{"create", "--name", "f", "--origin", "https://forge/x.git", "--branch", "main", "--auth-method", "token", "--auth-token", "s3"},
			method: "POST", path: "/fleet/create",
			body: `{"name":"f","origin":{"url":"https://forge/x.git","branch":"main","auth_method":"token","auth_token":"s3"}}`},
		{args: []string{"enroll", "box", "ssh-ed25519 AAAA box", "--address", "https://m:7443"}, method: "POST", path: "/fleet/enroll",
			body: `{"agent_id":"box","public_key":"ssh-ed25519 AAAA box","address":"https://m:7443"}`},
		{args: []string{"join", bundleFile, "--auth-method", "token", "--auth-token", "s3"}, method: "POST", path: "/fleet/join",
			body: `{"bundle":"BUNDLE-FROM-FILE","auth_method":"token","auth_token":"s3"}`},
		{args: []string{"join", "-"}, stdin: "BUNDLE-FROM-STDIN", method: "POST", path: "/fleet/join",
			body: `{"bundle":"BUNDLE-FROM-STDIN","auth_method":"","auth_token":""}`},
		{args: []string{"join-check", bundleFile}, method: "POST", path: "/fleet/join:check",
			body: `{"bundle":"BUNDLE-FROM-FILE","auth_method":"","auth_token":""}`},
		{args: []string{"leave"}, method: "POST", path: "/fleet/leave"},
		{args: []string{"revoke", "box", "--reason", "lost"}, method: "POST", path: "/fleet/revoke", body: `{"agent_id":"box","reason":"lost"}`},
		{args: []string{"decommission"}, method: "POST", path: "/fleet/decommission"},
		{args: []string{"root", "get"}, method: "GET", path: "/fleet/root"},
		{args: []string{"root", "put", "-"}, stdin: `{"files":{"root.key":"K","root.crt":"C"}}`, method: "PUT", path: "/fleet/root",
			body: `{"files":{"root.key":"K","root.crt":"C"}}`},
		{args: []string{"root", "delete"}, method: "DELETE", path: "/fleet/root"},
		{args: []string{"host", "--confirm-loss"}, method: "POST", path: "/fleet/host", body: `{"confirm_loss":true}`},
		{args: []string{"follow", "https://new:7443/fleet", "--auth-method", "basic", "--user", "u", "--password", "p"},
			method: "POST", path: "/fleet/follow",
			body: `{"url":"https://new:7443/fleet","auth_method":"basic","auth_token":"","user":"u","password":"p"}`},
		{args: []string{"index-rebuild"}, method: "POST", path: "/fleet/index-rebuild"},
		{args: []string{"drop-commit", "abc", "--leave-out-merge", "--left-out", "h1,h2"}, method: "POST", path: "/fleet/drop-commit",
			body: `{"commit":"abc","leave_out_merge":true,"left_out":["h1","h2"]}`},
	}

	// Every route in the server's local-only fleet group has a verb here.
	group := fleetLocalOnlyRoutes(t)
	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.method+" "+c.path] = true
	}
	require.Equal(t, group, covered, "the verbs and the server's RequireLocalVia fleet group differ")

	type got struct{ method, path, ctype, body string }
	var reqs []got
	refuse := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs = append(reqs, got{r.Method, strings.TrimPrefix(r.URL.Path, "/api/v1"), r.Header.Get("Content-Type"), string(b)})
		switch {
		case refuse:
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"this instance is the fleet's master","code":"master"}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`{"state":"registered","root_fingerprint":"ab12"}`))
		}
	})
	sock := oauthLocalListenerPath(t)
	ln, cleanup, err := auth.ListenLocal(sock)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Setenv("KNOMIT_HOME", t.TempDir())
	t.Setenv("KNOMIT_SOCKET", sock)

	run := func(args []string, stdin string) (string, error) {
		c := fleetCmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(io.Discard)
		c.SetIn(strings.NewReader(stdin))
		c.SetArgs(args)
		err := c.ExecuteContext(context.Background())
		return out.String(), err
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			reqs = nil
			out, err := run(c.args, c.stdin)
			require.NoError(t, err)
			require.Len(t, reqs, 1, "one request per verb")
			require.Equal(t, c.method, reqs[0].method)
			require.Equal(t, c.path, reqs[0].path)
			if c.body == "" {
				require.Empty(t, reqs[0].body)
			} else {
				require.Equal(t, "application/json", reqs[0].ctype)
				require.JSONEq(t, c.body, reqs[0].body)
			}
			if c.method == http.MethodDelete {
				require.Empty(t, out, "a 204 prints nothing")
			} else {
				require.Equal(t, "{\n  \"state\": \"registered\",\n  \"root_fingerprint\": \"ab12\"\n}\n", out, "the answer, indented")
			}
		})
	}

	refuse = true
	reqs = nil
	_, err = run([]string{"leave"}, "")
	require.ErrorContains(t, err, "this instance is the fleet's master", "the refusal is the server's detail")
	require.Len(t, reqs, 1)
}

// fleetLocalOnlyRoutes reads the `r.<Method>("/fleet/...")` lines of the
// RequireLocalVia group in the manage router's source, as "METHOD /path".
func fleetLocalOnlyRoutes(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "internal", "edge", "web", "api", "manage", "mount.go"))
	require.NoError(t, err)
	re := regexp.MustCompile(`(?m)^\s*r\.(Get|Post|Put|Delete)\("(/fleet[^"]*)"`)
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out[strings.ToUpper(m[1])+" "+m[2]] = true
	}
	require.NotEmpty(t, out, "no fleet route found in the RequireLocalVia group: the pattern no longer matches mount.go")
	return out
}

// A server that drops the connection without answering leaves the outcome of a
// destructive verb unknown: that must be an error, never a silent exit 0.
func TestFleetCLI_DroppedConnectionIsAnError(t *testing.T) {
	sock := oauthLocalListenerPath(t)
	ln, cleanup, err := auth.ListenLocal(sock)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	hijacked := false
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, herr := w.(http.Hijacker).Hijack()
		if herr != nil {
			return
		}
		hijacked = true
		_ = conn.Close()
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	var out bytes.Buffer
	err = fleetAPI(context.Background(), localAPIClient(sock), &out, http.MethodPost, "/fleet/decommission", nil)
	require.True(t, hijacked, "the handler ran and dropped the connection")
	require.Error(t, err, "a dropped connection is not success")
	require.Empty(t, out.String())
}
