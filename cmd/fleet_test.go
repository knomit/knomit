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
		{args: []string{"members", "enroll", "box", "ssh-ed25519 AAAA box", "--address", "https://m:7443"}, method: "POST", path: "/fleet/enroll",
			body: `{"agent_id":"box","public_key":"ssh-ed25519 AAAA box","address":"https://m:7443"}`},
		{args: []string{"join", bundleFile, "--auth-method", "token", "--auth-token", "s3"}, method: "POST", path: "/fleet/join",
			body: `{"bundle":"BUNDLE-FROM-FILE","auth_method":"token","auth_token":"s3"}`},
		{args: []string{"join", "-"}, stdin: "BUNDLE-FROM-STDIN", method: "POST", path: "/fleet/join",
			body: `{"bundle":"BUNDLE-FROM-STDIN","auth_method":"","auth_token":""}`},
		{args: []string{"join", bundleFile, "--check"}, method: "POST", path: "/fleet/join:check",
			body: `{"bundle":"BUNDLE-FROM-FILE","auth_method":"","auth_token":""}`},
		{args: []string{"leave"}, method: "POST", path: "/fleet/leave"},
		{args: []string{"members", "revoke", "box", "--reason", "lost"}, method: "POST", path: "/fleet/revoke", body: `{"agent_id":"box","reason":"lost"}`},
		{args: []string{"decommission"}, method: "POST", path: "/fleet/decommission"},
		{args: []string{"master", "export"}, method: "GET", path: "/fleet/root"},
		{args: []string{"master", "import", "-"}, stdin: `{"files":{"root.key":"K","root.crt":"C"}}`, method: "PUT", path: "/fleet/root",
			body: `{"files":{"root.key":"K","root.crt":"C"}}`},
		{args: []string{"master", "resign"}, method: "DELETE", path: "/fleet/root"},
		{args: []string{"hosting", "here", "--confirm-loss"}, method: "POST", path: "/fleet/host", body: `{"confirm_loss":true}`},
		{args: []string{"hosting", "follow", "https://new:7443/fleet", "--auth-method", "basic", "--user", "u", "--password", "p"},
			method: "POST", path: "/fleet/follow",
			body: `{"url":"https://new:7443/fleet","auth_method":"basic","auth_token":"","user":"u","password":"p"}`},
		{args: []string{"index", "rebuild"}, method: "POST", path: "/fleet/index-rebuild"},
		{args: []string{"sync", "drop", "abc", "--leave-out", "h1,h2"}, method: "POST", path: "/fleet/drop-commit",
			body: `{"commit":"abc","leave_out_merge":true,"left_out":["h1","h2"]}`},
	}

	// Every route in the server's local-only fleet group has a verb here (the
	// members read is outside the group and has its own test).
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

// localFleetServer serves handler on a local listener and points the fleet verbs
// at it.
func localFleetServer(t *testing.T, handler http.HandlerFunc) func(args []string) (string, error) {
	t.Helper()
	sock := oauthLocalListenerPath(t)
	ln, cleanup, err := auth.ListenLocal(sock)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Setenv("KNOMIT_HOME", t.TempDir())
	t.Setenv("KNOMIT_SOCKET", sock)
	return func(args []string) (string, error) {
		c := fleetCmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(io.Discard)
		c.SetArgs(args)
		err := c.ExecuteContext(context.Background())
		return out.String(), err
	}
}

const heldCommit = "3f2a91c4d5e6f708192a3b4c5d6e7f8091a2b3c4"

func TestFleetCLI_MembersIsAReadAndGroupsRefuseTypos(t *testing.T) {
	var got []string
	run := localFleetServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"members":[]}`))
	})
	out, err := run([]string{"members"})
	require.NoError(t, err)
	require.Equal(t, []string{"GET /api/v1/fleet/members"}, got)
	require.Equal(t, "{\n  \"members\": []\n}\n", out)
	for _, g := range []string{"master", "hosting", "index"} {
		_, err = run([]string{g, "nope"})
		require.Error(t, err, "%s <typo> is a non-zero exit", g)
	}
	require.Len(t, got, 1, "the groups sent nothing")
}

func TestFleetCLI_SyncSaysWhetherItIsBlocked(t *testing.T) {
	body := ""
	run := localFleetServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })

	body = `{"state":"registered","last_attempt":"T1","last_error":"knomit: origin was not moved: commit ` + heldCommit + ` refused by verify_signatures=enforce: host-record: bad",
		"last_error_main_held":true,"last_error_main_held_commit":"` + heldCommit + `"}`
	out, err := run([]string{"sync"})
	require.NoError(t, err)
	require.Equal(t, "sync blocked: the fleet's rules refuse commit 3f2a91c4 on this instance's branch; nothing from this instance reaches the fleet until it is dropped. Retrying does not help.\n"+
		"  refused: knomit: origin was not moved: commit "+heldCommit+" refused by verify_signatures=enforce: host-record: bad\n"+
		"  drop it: knomit fleet sync drop\n", out)

	body = `{"state":"registered","last_attempt":"T2","last_error":"dial tcp: refused"}`
	out, err = run([]string{"sync"})
	require.NoError(t, err)
	require.Equal(t, "sync: not blocked\nlast error (T2): dial tcp: refused\n", out)

	body = `{"state":"registered"}`
	out, err = run([]string{"sync"})
	require.NoError(t, err)
	require.Equal(t, "sync: not blocked\n", out)
}

func TestFleetCLI_SyncDropWithoutACommitDropsTheHeldOne(t *testing.T) {
	status := `{"last_error_main_held":true,"last_error_main_held_commit":"` + heldCommit + `"}`
	var reqs []string
	run := localFleetServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs = append(reqs, r.Method+" "+r.URL.Path+" "+string(b))
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(status))
			return
		}
		_, _ = w.Write([]byte(`{"dropped":1}`))
	})
	_, err := run([]string{"sync", "drop"})
	require.NoError(t, err)
	require.Len(t, reqs, 2)
	require.Equal(t, "GET /api/v1/fleet ", reqs[0])
	require.Equal(t, "POST /api/v1/fleet/drop-commit", reqs[1][:len("POST /api/v1/fleet/drop-commit")])
	require.JSONEq(t, `{"commit":"`+heldCommit+`"}`, strings.TrimPrefix(reqs[1], "POST /api/v1/fleet/drop-commit "))

	// A commit given is sent as given, with no lookup.
	reqs = nil
	_, err = run([]string{"sync", "drop", "abc"})
	require.NoError(t, err)
	require.Len(t, reqs, 1)
	require.JSONEq(t, `{"commit":"abc"}`, strings.TrimPrefix(reqs[0], "POST /api/v1/fleet/drop-commit "))

	// Not blocked: no commit to drop, and nothing is sent to the drop route.
	status = `{"state":"registered"}`
	reqs = nil
	_, err = run([]string{"sync", "drop"})
	require.EqualError(t, err, "sync is not blocked: there is no commit to drop")
	require.Equal(t, []string{"GET /api/v1/fleet "}, reqs)
}

func TestFleetCLI_RefusedDropShowsTheLeaveOutPlan(t *testing.T) {
	var posts []string
	run := localFleetServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		posts = append(posts, string(b))
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"that commit is a merge","code":"merge_commit","leave_out":[
			{"commit":"aaaaaaaa11111111","agent":"box","author":"A","subject":"first"},
			{"commit":"bbbbbbbb22222222","agent":"","author":"Bea","subject":"second"}]}`))
	})
	_, err := run([]string{"sync", "drop", "cafe1234"})
	require.EqualError(t, err, "that commit is a merge\n"+
		"  aaaaaaaa box: first\n"+
		"  bbbbbbbb Bea: second\n"+
		"confirm with: knomit fleet sync drop cafe1234 --leave-out aaaaaaaa11111111,bbbbbbbb22222222")
	require.Len(t, posts, 1)

	// A refusal without a list keeps the quoted detail, as every caller had.
	run = localFleetServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"not unpublished","code":"not_unpublished"}`))
	})
	_, err = run([]string{"sync", "drop", "cafe1234"})
	require.EqualError(t, err, `"not unpublished"`)
}

func TestFleetCLI_JoinCheckHitsTheCheckRoute(t *testing.T) {
	var path string
	run := localFleetServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	})
	f := filepath.Join(t.TempDir(), "b")
	require.NoError(t, os.WriteFile(f, []byte("B"), 0o600))
	_, err := run([]string{"join", f, "--check"})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/fleet/join:check", path)
	_, err = run([]string{"join", f})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/fleet/join", path)
}

// A refusal's text is the server's, which can echo requester-supplied text:
// no control or bidi rune reaches the terminal, and each commit stays on its
// own line.
func TestFleetCLI_RefusedDropTextCarriesNoControlRunes(t *testing.T) {
	run := localFleetServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"bad \u001b[31mred\nline\u009b‮flip","code":"merge_commit","leave_out":[
			{"commit":"aaaaaaaa11111111","agent":"bo\u001bx","author":"A","subject":"s\u001b[31m\nnext\u009bx‮y"}]}`))
	})
	_, err := run([]string{"sync", "drop", "cafe1234"})
	require.EqualError(t, err, "bad  [31mred line  flip\n"+
		"  aaaaaaaa bo x: s [31m next x y\n"+
		"confirm with: knomit fleet sync drop cafe1234 --leave-out aaaaaaaa11111111")
	for _, r := range err.Error() {
		if r == '\n' {
			continue
		}
		require.False(t, r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x202e, "control rune %U in the error", r)
	}
}
