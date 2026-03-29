package web

import (
	"encoding/json"
	"net/http"
	"testing"

	"go.uber.org/mock/gomock"
	"knomit/internal/repos"
	"knomit/internal/store"
)

func TestHandleExplain(t *testing.T) {
	explainResult := store.ExplainResult{
		Incoming: []store.RefSummary{{Path: "kb/d.md", Title: "Fact D"}},
		Outgoing: []store.RefSummary{{Path: "kb/b.md", Title: "Fact B", Deleted: false}},
	}

	tests := []struct {
		name       string
		query      string
		useIdx     bool
		result     store.ExplainResult
		wantStatus int
	}{
		{
			name:       "missing path returns 400",
			query:      "/api/v1/knomit/explain",
			useIdx:     false,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "returns incoming and outgoing",
			query:      "/api/v1/knomit/explain?path=kb/a.md",
			useIdx:     true,
			result:     explainResult,
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			gs := NewMockGitStore(ctrl)

			var idx repos.SearchIndex
			if tc.useIdx {
				mockIdx := NewMockSearchIndex(ctrl)
				mockIdx.EXPECT().ExplainFact("kb/a.md").Return(tc.result, nil)
				idx = mockIdx
			}

			handler := newTestRouter(gs, idx)
			rr := doRequest(t, handler, http.MethodGet, tc.query, "")

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}

			if tc.wantStatus == http.StatusOK {
				var resp map[string]any
				if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if resp["incoming"] == nil || resp["outgoing"] == nil {
					t.Errorf("response missing incoming/outgoing: %v", resp)
				}
			}
		})
	}
}
