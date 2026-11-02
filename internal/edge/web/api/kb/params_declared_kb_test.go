package kb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Every other knowledge-base GET route's declared set IS its openapi.yaml
// parameter list (the four list routes are TestListRouteParams_MatchOpenAPI).
// The lens /facts/{path}/commits sub-resource has no spec path of its own and
// shares factCommitsParams with the repo route; the materialize export's set
// is materializeParams, the one its own refusal reads.
//
// Sabotage: drop "axis" from statsParams (or "repo" from lensTopicsParams) and
// that route's row differs.
func TestKBRouteParams_MatchOpenAPI(t *testing.T) {
	materialize := declaredParams{names: materializeParams}
	for route, d := range map[string]declaredParams{
		"/repos/{repo}/branches/{branch}/topics":                      topicsParams,
		"/repos/{repo}/branches/{branch}/topics/{path}":               topicsParams,
		"/repos/{repo}/branches/{branch}/changes":                     changesParams,
		"/repos/{repo}/branches/{branch}/commits":                     commitsParams,
		"/repos/{repo}/branches/{branch}/commits/{sha}":               commitDetailParams,
		"/repos/{repo}/branches/{branch}/commits/{sha}/facts/{path}":  commitFactParams,
		"/repos/{repo}/branches/{branch}/commits/{sha}/topics":        commitTopicsParams,
		"/repos/{repo}/branches/{branch}/commits/{sha}/topics/{path}": commitTopicsParams,
		"/repos/{repo}/branches/{branch}/activity":                    activityParams,
		"/repos/{repo}/branches/{branch}/completions":                 completionsParams,
		"/repos/{repo}/branches/{branch}/domains":                     domainsParams,
		"/repos/{repo}/branches/{branch}/domains/{name}":              domainFactsParams,
		"/repos/{repo}/branches/{branch}/motifs":                      motifsParams,
		"/repos/{repo}/branches/{branch}/motifs/{key}":                motifClusterParams,
		"/repos/{repo}/branches/{branch}/stats":                       statsParams,
		"/repos/{repo}/branches/{branch}/events":                      eventsParams,
		"/repos/{repo}/branches/{branch}/triggers":                    triggersParams,
		"/repos/{repo}/branches/{branch}/facts/{path}":                factReadParams,
		"/repos/{repo}/branches/{branch}/facts/{path}/commits":        factCommitsParams,
		"/repos/{repo}/branches/{branch}/facts/{path}/materialize":    materialize,
		"/lenses/{lens}/facts/{path}":                                 factReadParams,
		"/lenses/{lens}/completions":                                  lensCompletionsParams,
		"/lenses/{lens}/motifs":                                       lensMotifsParams,
		"/lenses/{lens}/motifs/{key}":                                 lensMotifClusterParams,
		"/lenses/{lens}/stats":                                        lensStatsParams,
		"/lenses/{lens}/topics":                                       lensTopicsParams,
		"/lenses/{lens}/topics/{path}":                                lensTopicsParams,
	} {
		want := openAPIQueryParams(t, route)
		got := d.list()
		if len(want) == 0 && len(got) == 0 {
			continue
		}
		require.Equal(t, want, got, route)
	}
}
