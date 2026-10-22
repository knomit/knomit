package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/facts"
	"knomit/internal/engine/store/index"
)

// The per-owner fail-only read hooks: index.Facade.SetReadFaultForTest,
// facts.Index.Set{ReadFact,ListDir,Write}FaultForTest and
// vocab.MotifIndex.SetReadFaultForTest. They live here, beside the other
// child-owner tests, because a child's own package cannot build a *Service.
// Each test pins four things: unset runs the real read; set returns the
// injected error from exactly that read; a second Service is unaffected; the
// restore func brings the real read back.

const faultPath = "kb/t/a.md"

var errInjected = errors.New("injected read fault")

// faultService opens a store with one live fact on main.
func faultService(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "main"))
	writeFact(t, svc, "main", faultPath, nil)
	writeFact(t, svc, "main", "kb/t/b.md", nil) // so the head is not the commit that wrote faultPath
	return svc
}

func TestIndexReadFault_FailsExactlyTheNamedReadOnExactlyThatService(t *testing.T) {
	ctx := context.Background()
	a, b := faultService(t), faultService(t)
	head, err := a.HeadCommit(ctx, "main")
	require.NoError(t, err)

	reads := map[string]func(*Service) error{
		"Stats":    func(s *Service) error { _, err := s.Search().Stats(ctx, "main", "", ""); return err },
		"Activity": func(s *Service) error { _, err := s.Search().Activity(ctx, "main", faultPath); return err },
		"Completions": func(s *Service) error {
			_, err := s.Search().Completions(ctx, "main", "domain", "", 5)
			return err
		},
		"RecentFacts": func(s *Service) error {
			_, _, err := s.Search().RecentFacts(ctx, "main", index.SearchOptions{Limit: 5})
			return err
		},
		"Search": func(s *Service) error {
			_, err := s.Search().Search(ctx, "main", index.SearchOptions{Limit: 5})
			return err
		},
		"ExplainFact": func(s *Service) error { _, err := s.Search().ExplainFact(ctx, "main", faultPath); return err },
		"FactLiveAtCommit": func(s *Service) error {
			_, err := s.Search().FactLiveAtCommit(ctx, "main", faultPath, head)
			return err
		},
	}
	require.Len(t, reads, 7)

	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			// Unset: the real read runs and succeeds on both services.
			require.NoError(t, read(a))
			require.NoError(t, read(b))

			var seen []string
			restore := a.Search().SetReadFaultForTest(func(method string) error {
				seen = append(seen, method)
				if method == name {
					return errInjected
				}
				return nil
			})
			require.ErrorIs(t, read(a), errInjected, "the named read returns the injected error")
			require.NoError(t, read(b), "a second Service is unaffected")
			require.Contains(t, seen, name)
			for other, otherRead := range reads {
				if other != name && other != "RecentFacts" { // RecentFacts nests no other faultable read without Text
					require.NoError(t, otherRead(a), "%s must not fail when only %s is faulted", other, name)
				}
			}

			restore()
			require.NoError(t, read(a), "restore brings the real read back")
		})
	}
}

// A fault that returns nil lets the read through, and a nil fault removes the
// hook, so a test can leave one installed and decide per call.
func TestIndexReadFault_NilResultAndNilHook(t *testing.T) {
	ctx := context.Background()
	svc := faultService(t)
	restore := svc.Search().SetReadFaultForTest(func(string) error { return nil })
	defer restore()
	_, err := svc.Search().Stats(ctx, "main", "", "")
	require.NoError(t, err)

	svc.Search().SetReadFaultForTest(func(string) error { return errInjected })
	svc.Search().SetReadFaultForTest(nil)
	_, err = svc.Search().Stats(ctx, "main", "", "")
	require.NoError(t, err, "a nil hook removes the fault")
}

// A fault is checked in the read itself, so the nested Search a text
// RecentFacts makes is failed by a Search fault: documented on the setter.
func TestIndexReadFault_NestedSearchFailsTextRecentFacts(t *testing.T) {
	ctx := context.Background()
	svc := faultService(t)
	defer svc.Search().SetReadFaultForTest(func(m string) error {
		if m == "Search" {
			return errInjected
		}
		return nil
	})()
	_, _, err := svc.Search().RecentFacts(ctx, "main", index.SearchOptions{Text: "a", Limit: 5})
	require.ErrorIs(t, err, errInjected)
	_, _, err = svc.Search().RecentFacts(ctx, "main", index.SearchOptions{Limit: 5})
	require.NoError(t, err, "RecentFacts without Text does not reach Search")
}

// The delay tests of the lens fan-out need per-mount latency, which the
// existing Overrides.Reads observer already provides: it runs at the top of the
// read, may sleep, and composes with the fault (installing Overrides does not
// clear it, nor the reverse).
func TestIndexReadFault_ComposesWithOverridesObserverThatSleeps(t *testing.T) {
	ctx := context.Background()
	svc := faultService(t)
	var mu sync.Mutex
	var observed []string
	const delay = 30 * time.Millisecond
	defer svc.Search().SetOverridesForTest(index.Overrides{Reads: func(m string) error {
		mu.Lock()
		observed = append(observed, m)
		mu.Unlock()
		time.Sleep(delay)
		return nil
	}})()
	defer svc.Search().SetReadFaultForTest(func(m string) error {
		if m == "Stats" {
			return errInjected
		}
		return nil
	})()

	start := time.Now()
	_, err := svc.Search().Stats(ctx, "main", "", "")
	require.ErrorIs(t, err, errInjected)
	require.GreaterOrEqual(t, time.Since(start), delay, "the observer slept before the fault returned")
	require.Equal(t, []string{"Stats"}, observed)
}

func TestFactsReadFactFault(t *testing.T) {
	ctx := context.Background()
	a, b := faultService(t), faultService(t)
	read := func(s *Service, opts *facts.ReadFactOpts) error {
		_, err := s.Facts().ReadFact(ctx, "main", faultPath, opts)
		return err
	}
	walkBack := func(s *Service) error {
		head, err := s.HeadCommit(ctx, "main")
		require.NoError(t, err)
		return read(s, &facts.ReadFactOpts{BeforeCommit: head})
	}
	require.NoError(t, read(a, nil))
	require.NoError(t, walkBack(a))

	var gotPath string
	var gotOpts []*facts.ReadFactOpts
	restore := a.Facts().SetReadFactFaultForTest(func(path string, opts *facts.ReadFactOpts) error {
		gotPath = path
		gotOpts = append(gotOpts, opts)
		if opts.BeforeCommit != "" {
			return errInjected // only the walk-back read fails
		}
		return nil
	})
	require.NoError(t, read(a, nil), "a nil result lets the read through")
	require.ErrorIs(t, walkBack(a), errInjected)
	require.Equal(t, faultPath, gotPath)
	require.NotContains(t, gotOpts, (*facts.ReadFactOpts)(nil), "opts is defaulted before the hook sees it")
	require.NoError(t, walkBack(b), "a second Service is unaffected")

	// Only ReadFact is failed: ListDir and a write still run.
	_, err := a.Facts().ListDir(ctx, "main", "kb")
	require.NoError(t, err)

	restore()
	require.NoError(t, walkBack(a), "restore brings the real read back")
}

func TestFactsListDirFault(t *testing.T) {
	ctx := context.Background()
	a, b := faultService(t), faultService(t)
	list := func(s *Service) error { _, err := s.Facts().ListDir(ctx, "main", "kb"); return err }
	require.NoError(t, list(a))

	restore := a.Facts().SetListDirFaultForTest(func(path string) error {
		require.Equal(t, "kb", path)
		return errInjected
	})
	require.ErrorIs(t, list(a), errInjected)
	require.NoError(t, list(b), "a second Service is unaffected")
	_, err := a.Facts().ReadFact(ctx, "main", faultPath, nil)
	require.NoError(t, err, "only ListDir is failed")

	restore()
	require.NoError(t, list(a))
}

func TestFactsWriteFault(t *testing.T) {
	ctx := context.Background()
	a, b := faultService(t), faultService(t)
	write := func(s *Service, path string) error {
		_, err := s.Facts().WriteFact(ctx, "main", path, testFactBody(path, 0.5, nil), "w "+path, "update")
		return err
	}
	require.NoError(t, write(a, "kb/t/w1.md"))

	restore := a.Facts().SetWriteFaultForTest(func(path string) error {
		if path == "kb/t/w2.md" {
			return errInjected
		}
		return nil
	})
	headBefore, err := a.HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.ErrorIs(t, write(a, "kb/t/w2.md"), errInjected)
	headAfter, err := a.HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, headBefore, headAfter, "a faulted write commits nothing")
	require.NoError(t, write(a, "kb/t/w3.md"), "a nil result lets the write through")
	require.NoError(t, write(b, "kb/t/w2.md"), "a second Service is unaffected")

	restore()
	require.NoError(t, write(a, "kb/t/w2.md"), "restore brings the real write back")
}

func TestVocabReadFault(t *testing.T) {
	ctx := context.Background()
	a, b := faultService(t), faultService(t)
	reads := map[string]func(*Service) error{
		"ClustersUnder": func(s *Service) error { _, err := s.Motifs().ClustersUnder(ctx, "main", ""); return err },
		"Definitions": func(s *Service) error {
			_, err := s.Motifs().Definitions(ctx, "main", []string{"x"})
			return err
		},
		"VocabularyHealthUnder": func(s *Service) error {
			_, err := s.Motifs().VocabularyHealthUnder(ctx, "main", "")
			return err
		},
		"AliasRows": func(s *Service) error { _, err := s.Motifs().AliasRows(ctx, "main"); return err },
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, read(a))
			restore := a.Motifs().SetReadFaultForTest(func(method string) error {
				if method == name {
					return errInjected
				}
				return nil
			})
			require.ErrorIs(t, read(a), errInjected)
			require.NoError(t, read(b), "a second Service is unaffected")
			for other, otherRead := range reads {
				if other != name {
					require.NoError(t, otherRead(a), "%s must not fail when only %s is faulted", other, name)
				}
			}
			restore()
			require.NoError(t, read(a))
		})
	}
}
