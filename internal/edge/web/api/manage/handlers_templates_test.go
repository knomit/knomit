package manage

import (
	"knomit/internal/corpus/repos"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// A 422 for a template that is there and unusable: covered per rule in
// internal/corpus/repos; here only the status mapping of each sentinel.
func TestCreateErrStatus_TemplateSentinels(t *testing.T) {
	for _, err := range []error{repos.ErrTemplateNotRegular, repos.ErrTemplateTooLarge, repos.ErrTemplateLayout,
		repos.ErrTemplateDescribedTwice, repos.ErrTemplateNoOntology, repos.ErrTemplateOntology, repos.ErrTemplatePresetID} {
		status, title := createErrStatus(err)
		require.Equal(t, http.StatusUnprocessableEntity, status, err.Error())
		require.Equal(t, "Template invalid", title)
	}
	status, _ := createErrStatus(repos.ErrTemplateSourceUnavailable)
	require.Equal(t, http.StatusServiceUnavailable, status)
}
