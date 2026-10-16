package fact

import (
	"fmt"
	"strings"
)

// The host record (FleetHostPath) is one document: the header line, then one
// "url: <URL>" line per fleet repository URL, in the order written, and,
// when the master that wrote it stood down, one "stood-down: <agent id>" line.

const fleetHostHeader = "# knomit fleet host v1"

// FleetHostRecord is the host record read. StoodDown is the agent id of the
// master that stood down with this record (its last commit as master; the
// URLs still say where main was served), "" while that master is the master.
type FleetHostRecord struct {
	URLs      []string
	StoodDown string
}

// RenderFleetHost is the host record holding urls.
func RenderFleetHost(urls []string) string {
	return RenderFleetHostRecord(FleetHostRecord{URLs: urls})
}

// RenderFleetHostRecord is the host record r.
func RenderFleetHostRecord(r FleetHostRecord) string {
	var b strings.Builder
	b.WriteString(fleetHostHeader + "\n")
	for _, u := range r.URLs {
		b.WriteString("url: " + u + "\n")
	}
	if r.StoodDown != "" {
		b.WriteString("stood-down: " + r.StoodDown + "\n")
	}
	return b.String()
}

// ParseFleetHost reads a host record's URLs (ParseFleetHostRecord).
func ParseFleetHost(raw []byte) ([]string, error) {
	r, err := ParseFleetHostRecord(raw)
	return r.URLs, err
}

// ParseFleetHostRecord reads a host record: the header, then url lines and at
// most one stood-down line (blank lines are skipped). Anything else is an
// error: a record a reader cannot read says nothing about the host.
func ParseFleetHostRecord(raw []byte) (FleetHostRecord, error) {
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if strings.TrimSpace(lines[0]) != fleetHostHeader {
		return FleetHostRecord{}, fmt.Errorf("the host record does not start with %q", fleetHostHeader)
	}
	r := FleetHostRecord{URLs: []string{}}
	for _, l := range lines[1:] {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if id, ok := strings.CutPrefix(l, "stood-down: "); ok && strings.TrimSpace(id) != "" && r.StoodDown == "" {
			r.StoodDown = strings.TrimSpace(id)
			continue
		}
		u, ok := strings.CutPrefix(l, "url: ")
		if !ok || strings.TrimSpace(u) == "" {
			return FleetHostRecord{}, fmt.Errorf("the host record's line %q is not a url line", l)
		}
		r.URLs = append(r.URLs, strings.TrimSpace(u))
	}
	return r, nil
}
