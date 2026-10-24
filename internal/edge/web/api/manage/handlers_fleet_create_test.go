package manage

import (
	"knomit/internal/corpus/repos"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// 201 means main holds this instance's active record: state registered AND
// record_state active. Anything else is 202.
// Sabotage: answer 201 on state registered alone -> the pending row is 201
// -> red.
func TestCreatedFleetStatus(t *testing.T) {
	for _, tc := range []struct {
		state, record string
		want          int
	}{
		{repos.FleetRegistered, "active", http.StatusCreated},
		{repos.FleetRegistered, "pending", http.StatusAccepted},
		{repos.FleetRegistering, "pending", http.StatusAccepted},
		{repos.FleetRegistering, "active", http.StatusAccepted},
	} {
		got := createdFleetStatus(repos.FleetStatus{State: tc.state, RecordState: tc.record})
		require.Equal(t, tc.want, got, "state %s, record %s", tc.state, tc.record)
	}
}
