package version

import "testing"

func TestString(t *testing.T) {
	tests := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{
			name:    "version and commit joined with a dot",
			version: "0.5.0",
			commit:  "2a7ae9d",
			want:    "0.5.0.2a7ae9d",
		},
		{
			name:    "no commit falls back to bare version",
			version: "dev",
			commit:  "",
			want:    "dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig, origCommit := Version, Commit
			t.Cleanup(func() { Version, Commit = orig, origCommit })

			Version, Commit = tt.version, tt.commit
			if got := String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	// Bare `go build` (no ldflags) must compile and report a dev version,
	// never an empty string.
	if Version == "" {
		t.Error("Version default must not be empty")
	}
}
