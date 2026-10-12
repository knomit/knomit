package knomitapi

import (
	"os"
	"path/filepath"
	"testing"
)

// readLockfileBaseURL understands both file generations:
//
//	v2 (`knomit serve`): "http" is a full base URL, taken in preference to
//	"port"; "http": "" with "port": 0 means the server has no TCP listener.
//	v1 (the desktop's, older servers): only "port".
//
// "No TCP" must come back as ("", nil) -- the same answer as no file -- so
// ResolveServer falls through to the default address, which dials the local
// socket first. Each row asserts the exact URL, and the v1 row would also
// pass for a reader that ignored "http", which is why the v2 rows carry a
// port that DIFFERS from the http URL's.
func TestReadLockfileBaseURL_Generations(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"v1 port", `{"pid":1,"port":4242,"version":"0.5"}`, "http://127.0.0.1:4242"},
		{"v2 http wins over port", `{"schema":2,"pid":1,"port":4242,"http":"http://10.1.2.3:9000"}`, "http://10.1.2.3:9000"},
		{"v2 http alone", `{"schema":2,"pid":1,"http":"http://127.0.0.1:19391"}`, "http://127.0.0.1:19391"},
		{"v2 no TCP: http empty, port 0", `{"schema":2,"pid":1,"port":0,"http":""}`, ""},
		{"v1 port 0", `{"pid":1,"port":0}`, ""},
		{"v2 http empty falls back to port (desktop writes both)", `{"pid":1,"port":4242,"http":""}`, "http://127.0.0.1:4242"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := isolateLockfile(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readLockfileBaseURL()
			if err != nil {
				t.Fatalf("readLockfileBaseURL: %v", err)
			}
			if got != c.want {
				t.Fatalf("readLockfileBaseURL = %q, want %q", got, c.want)
			}
		})
	}
}
