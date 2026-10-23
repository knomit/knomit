package web

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
)

// fatalRecorder stands in for the *testing.T a helper is given, so a test can
// observe the t.Fatalf the helper raises without failing itself. Fatalf records
// the message and unwinds with a panic that run recovers; everything else
// (Cleanup, Errorf, ...) goes to the real test.
type fatalRecorder struct {
	testing.TB
}

type recordedFatal struct{ msg string }

func (fatalRecorder) Helper() {}

func (fatalRecorder) Fatalf(format string, args ...any) {
	panic(recordedFatal{fmt.Sprintf(format, args...)})
}

// fatalOf runs f and returns the message of the t.Fatalf it raised ("" if none).
func fatalOf(f func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			rf, ok := r.(recordedFatal)
			if !ok {
				panic(r)
			}
			msg = rf.msg
		}
	}()
	f()
	return ""
}

// SetOverridesForTest replaces the WHOLE index.Overrides of a store, so a second
// observer on one mount would silently drop the first, and a "0 reads" assertion
// on the dropped one would pass vacuously. The three helpers that install an
// Overrides share one guard (installMountOverrides) that fails the test instead.
// Every ordered pair of the three is tried, on a fresh store each, and a store
// of another test is not affected.
func TestMountOverrideHelpersRefuseASecondObserver(t *testing.T) {
	helpers := map[string]func(tb testing.TB, m *repos.Manager){
		"countMountReads":    func(tb testing.TB, m *repos.Manager) { countMountReads(tb, m, []string{"alpha"}, "Search") },
		"slowMountRead":      func(tb testing.TB, m *repos.Manager) { slowMountRead(tb, m, "alpha", "Search", time.Millisecond) },
		"observeMountSearch": func(tb testing.TB, m *repos.Manager) { observeMountSearch(tb, m, "alpha") },
	}
	for first, install := range helpers {
		for second, installAgain := range helpers {
			t.Run(first+" then "+second, func(t *testing.T) {
				m, _ := newRealStoreManager(t, "alpha")
				install(t, m)

				msg := fatalOf(func() { installAgain(fatalRecorder{t}, m) })

				require.NotEmpty(t, msg, "%s installed a second observer on the store %s already observes", second, first)
				require.Contains(t, msg, second)
				require.Contains(t, msg, "installed by "+first)

				// A store nothing observes takes either helper.
				other, _ := newRealStoreManager(t, "alpha")
				require.Empty(t, fatalOf(func() { installAgain(fatalRecorder{t}, other) }))
			})
		}
	}
}
