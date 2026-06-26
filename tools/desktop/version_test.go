//go:build desktop

package main

import "testing"

func TestWantsVersion(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"version subcommand", []string{"version"}, true},
		{"--version flag", []string{"--version"}, true},
		{"-version flag", []string{"-version"}, true},
		{"no args", nil, false},
		{"other flag", []string{"--debug"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wantsVersion(tt.args); got != tt.want {
				t.Errorf("wantsVersion(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
