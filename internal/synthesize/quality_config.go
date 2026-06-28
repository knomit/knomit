package synthesize

import "knomit/internal/repos"

// QualityConfigFromRepo reads the six Q-knob accessors from ri and returns a
// QualityConfig populated with those values. Tasks 15/16 call this to thread
// per-repo configuration into the bridge quality scorer.
func QualityConfigFromRepo(ri *repos.RepoInstance) QualityConfig {
	return QualityConfig{
		CohFloor:     ri.DiscoveryCohFloor(),
		QualityFloor: ri.DiscoveryQualityFloor(),
		WCoh:         ri.DiscoveryWCoh(),
		WGap:         ri.DiscoveryWGap(),
		WSpec:        ri.DiscoveryWSpec(),
		MaxMembers:   ri.DiscoveryMaxMembers(),
	}
}
