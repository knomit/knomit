package web

import (
	"strings"
	"testing"

	"knomit/internal/foundation/pki"
)

// internalForms is the fleet transport's internal URL for an https URL, as
// pki.GitURL derives it, in lower and upper case: the scheme must be refused
// as input however it is spelled (go-git lowercases it). httpx's tests keep
// the same helper for their own table.
func internalForms(t *testing.T, https string) []string {
	t.Helper()
	u, err := pki.GitURL(https, true)
	if err != nil {
		t.Fatal(err)
	}
	scheme, rest, _ := strings.Cut(u, "://")
	return []string{u, strings.ToUpper(scheme) + "://" + rest}
}
