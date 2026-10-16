package fact

import (
	"fmt"
	"strings"
)

// The host record (FleetHostPath) is one document: the header line, then one
// "url: <URL>" line per fleet repository URL, in the order written.

const fleetHostHeader = "# knomit fleet host v1"

// RenderFleetHost is the host record holding urls.
func RenderFleetHost(urls []string) string {
	var b strings.Builder
	b.WriteString(fleetHostHeader + "\n")
	for _, u := range urls {
		b.WriteString("url: " + u + "\n")
	}
	return b.String()
}

// ParseFleetHost reads a host record: the header, then url lines (blank
// lines are skipped). Anything else is an error: a record a reader cannot
// read says nothing about the host.
func ParseFleetHost(raw []byte) ([]string, error) {
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if strings.TrimSpace(lines[0]) != fleetHostHeader {
		return nil, fmt.Errorf("the host record does not start with %q", fleetHostHeader)
	}
	urls := []string{}
	for _, l := range lines[1:] {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		u, ok := strings.CutPrefix(l, "url: ")
		if !ok || strings.TrimSpace(u) == "" {
			return nil, fmt.Errorf("the host record's line %q is not a url line", l)
		}
		urls = append(urls, strings.TrimSpace(u))
	}
	return urls, nil
}

// The master marker (FleetMasterPath) is the acting master's agent id on one
// line, and nothing else.

// RenderFleetMaster is the master marker naming agentID.
func RenderFleetMaster(agentID string) string {
	return agentID + "\n"
}

// ParseFleetMaster reads a master marker: one line holding the agent id
// (surrounding whitespace and a final newline are ignored). Anything else is
// an error.
func ParseFleetMaster(raw []byte) (string, error) {
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fmt.Errorf("the master marker names no agent")
	}
	if strings.ContainsAny(id, " \t\r\n") {
		return "", fmt.Errorf("the master marker is not one agent id on one line")
	}
	return id, nil
}
