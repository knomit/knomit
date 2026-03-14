package synthesize

import (
	"regexp"
	"strconv"
)

// Profile captures capability-dependent settings for LLM interactions.
type Profile struct {
	Name           string // "large" or "small"
	ForceJSON      bool   // constrain output to JSON (kills chain-of-thought in small models)
	RetryOnPassive bool   // retry with sharper prompt if response is passive
	MaxChunkBytes  int    // max facts payload size per LLM call
}

// smallPattern matches model size markers like ":8b", ":7b", ":3b" where the
// number is ≤ 14. Models above 14b are treated as large.
var smallPattern = regexp.MustCompile(`:(\d+)b`)

// smallProfileThreshold is the maximum parameter count (in billions) for a model
// to be classified as "small".
const smallProfileThreshold = 14

// LargeProfile is the canonical large model profile.
var LargeProfile = Profile{
	Name:           "large",
	ForceJSON:      true,
	RetryOnPassive: false,
	MaxChunkBytes:  100_000,
}

// SmallProfile is the canonical small model profile.
var SmallProfile = Profile{
	Name:           "small",
	ForceJSON:      false,
	RetryOnPassive: true,
	MaxChunkBytes:  50_000,
}

// ResolveProfile returns the appropriate profile for a given model name.
// Models with size markers ≤ 14b are "small"; everything else is "large".
func ResolveProfile(model string) Profile {
	if isSmallModel(model) {
		return SmallProfile
	}
	return LargeProfile
}

func isSmallModel(model string) bool {
	m := smallPattern.FindStringSubmatch(model)
	if m == nil {
		return false
	}
	size, err := strconv.Atoi(m[1])
	if err != nil {
		return false
	}
	return size <= smallProfileThreshold
}
