package httpx

import "knomit/internal/corpus/repos/control"

// ArchiveSummary is the one-line wording of an archive reason.
func ArchiveSummary(why *control.ArchiveReason) string {
	if why == nil {
		return "No reason recorded"
	}
	if why.Source == control.ArchiveBySystem {
		if why.Reason == "" {
			return "Archived automatically"
		}
		return "Archived automatically: " + why.Reason
	}
	if why.Note != "" {
		return "Archived by the user: " + why.Note
	}
	return "Archived by the user"
}
