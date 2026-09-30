package repos_test

// The claim protocol's rank, run from the SHIPPED claims.js in a plain goja
// runtime (the script's dispatch does nothing when `change` is undefined).
// Every machine must compute the same order, so it must be total.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dop251/goja"
	"github.com/stretchr/testify/require"
)

func shippedClaims(t *testing.T) *goja.Runtime {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(templateDir, ".knomit", "triggers", "claims.js"))
	require.NoError(t, err)
	vm := goja.New()
	_, err = vm.RunString(string(src))
	require.NoError(t, err, "claims.js must load with no fire (change undefined)")
	return vm
}

func jsBool(t *testing.T, vm *goja.Runtime, expr string) bool {
	t.Helper()
	v, err := vm.RunString(expr)
	require.NoError(t, err, expr)
	return v.ToBoolean()
}

func jsString(t *testing.T, vm *goja.Runtime, expr string) string {
	t.Helper()
	v, err := vm.RunString(expr)
	require.NoError(t, err, expr)
	return v.String()
}

// taskIDWonBy is a task id whose first claimer, by the shipped rank, is
// winner (among H and P).
func taskIDWonBy(t *testing.T, winner string) string {
	t.Helper()
	vm := shippedClaims(t)
	for i := 1; i < 100; i++ {
		id := fmt.Sprintf("task-%d", i)
		if jsString(t, vm, fmt.Sprintf("firstOf(%q, [%q, %q])", id, mHostID, mPeerID)) == winner {
			return id
		}
	}
	t.Fatalf("no task id is won by %s", winner)
	return ""
}

// TestMissionTemplate_RankIsTotal: with every hash colliding (fnv1a replaced
// by a constant), the order falls back to the claimer ids as strings: it is
// antisymmetric, irreflexive and independent of the order the claims were
// listed in, so every machine picks the same first claimer.
//
// SABOTAGE: drop the id tie-break in ranksBefore (`return ha < hb;` only) →
// with colliding hashes neither id ranks before the other, and firstOf keeps
// whichever it saw first → the listing-order assertion goes red.
func TestMissionTemplate_RankIsTotal(t *testing.T) {
	vm := shippedClaims(t)
	require.Equal(t, "2166136261", jsString(t, vm, `String(fnv1a(""))`), "FNV-1a 32-bit offset basis")
	require.Equal(t, "3826002220", jsString(t, vm, `String(fnv1a("a"))`), "FNV-1a 32-bit of \"a\"")

	_, err := vm.RunString(`fnv1a = function () { return 7; };`)
	require.NoError(t, err)
	a, b := mHostID, mPeerID
	require.True(t, jsBool(t, vm, fmt.Sprintf("ranksBefore('t', %q, %q)", a, b)))
	require.False(t, jsBool(t, vm, fmt.Sprintf("ranksBefore('t', %q, %q)", b, a)), "antisymmetric")
	require.False(t, jsBool(t, vm, fmt.Sprintf("ranksBefore('t', %q, %q)", a, a)), "irreflexive")
	require.Equal(t, a, jsString(t, vm, fmt.Sprintf("firstOf('t', [%q, %q])", a, b)))
	require.Equal(t, a, jsString(t, vm, fmt.Sprintf("firstOf('t', [%q, %q])", b, a)), "independent of listing order")
	require.Equal(t, "a-1", jsString(t, vm, `firstOf('t', ['c-3', 'a-1', 'b-2'])`))
	require.Equal(t, "a-1", jsString(t, vm, `firstOf('t', ['b-2', 'c-3', 'a-1'])`))
}
