package app

import (
	"os"
	"testing"
)

func TestAgentBranchNameOverride(t *testing.T) {
	host, _ := os.Hostname()
	tests := []struct {
		name      string
		agentName string
		fp        string
		want      string
	}{
		{"empty falls back to hostname", "", "abc123", "agent/" + sanitizeRefComponent(host) + "-abc123"},
		{"explicit name wins", "prod-1", "abc123", "agent/prod-1-abc123"},
		{"name is sanitized", "prod 1:x", "abc123", "agent/prod-1-x-abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentBranch(tt.agentName, tt.fp); got != tt.want {
				t.Errorf("agentBranch(%q, %q) = %q, want %q", tt.agentName, tt.fp, got, tt.want)
			}
		})
	}
}
