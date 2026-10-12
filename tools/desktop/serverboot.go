//go:build desktop

package main

// bootPhase names the stage the supervisor (supervisor.go) is in. It is what
// the boot screen shows while there is no API to ask anything else of — see
// GET /boot/status in configInjectingHandler.
//
// These strings are a WIRE FORMAT: web/src/boot.ts maps each one to the
// sentence the user reads, and an unknown value there falls back to a generic
// "Starting…". Renaming one without changing that map silently degrades the
// boot screen rather than breaking it, so change both together. The web side
// also knows "starting-server", which the in-process server used to report;
// the supervisor has no such step (the server process is "starting-engine"
// until its socket answers), and the web map keeps it harmlessly.
type bootPhase string

const (
	phaseStarting          bootPhase = "starting"
	phaseInstallingTools   bootPhase = "installing-tools"
	phaseDownloadingModels bootPhase = "downloading-models"
	phaseStartingEngine    bootPhase = "starting-engine"
	phaseReady             bootPhase = "ready"
	phaseFailed            bootPhase = "failed"
)

// bootStatus is the JSON GET /boot/status answers with.
//
// APIBase is carried here rather than making the client re-fetch /config.js
// once the boot finishes: the client already has to poll this endpoint, and one
// field costs nothing next to a second script fetch whose only job is to set the
// same string. It is the page's own origin (see uiStatusFrom), never a port.
type bootStatus struct {
	Ready   bool   `json:"ready"`
	Phase   string `json:"phase"`
	APIBase string `json:"api_base,omitempty"`
	Error   string `json:"error,omitempty"`
}
