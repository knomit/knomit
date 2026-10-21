package synthesize

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"go.uber.org/mock/gomock"

	"knomit/internal/store"
	"knomit/internal/store/index"
)

// This file is the synthesize tests' scripted stand-in for the store's search
// handle. It replaces the gomock MockSearchIndex that stood in for the deleted
// store.SearchIndex interface.
//
// newScriptedSearch opens a real (in-memory) store and returns its real
// *index.Facade with every read the synthesize tests ever scripted routed
// through index.Overrides to a per-test script. The script keeps what the mock
// enforced: an expected call matches by argument matchers, runs a canned
// Return or DoAndReturn, is counted against Times/AnyTimes (default exactly
// once), and a call nothing expects fails the test, as does an expectation
// that was not met by the end of the test.

var errUnexpectedCall = errors.New("scripted search: unexpected call")

var scriptRegistry sync.Map // *index.Facade -> *searchScript

// newScriptedSearch returns a real *index.Facade whose scripted reads fail the
// test unless the test scripts them with scripted(idx).X(...).
func newScriptedSearch(t *testing.T) *index.Facade {
	t.Helper()
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	f := svc.Search()
	s := &searchScript{t: t, byMethod: map[string][]*expectation{}}
	restore := f.SetOverridesForTest(index.Overrides{
		Search: func(index.SearchFunc) index.SearchFunc {
			return func(ctx context.Context, branch string, q index.SearchOptions) ([]index.SearchResult, error) {
				res, ok := s.dispatch("Search", ctx, branch, q)
				if !ok {
					return nil, errUnexpectedCall
				}
				r, _ := res[0].([]index.SearchResult)
				return r, errOf(res[1])
			}
		},
		FactExistsAt: func(ctx context.Context, branch, path, commit string) (bool, error) {
			res, ok := s.dispatch("FactExistsAt", ctx, branch, path, commit)
			if !ok {
				return false, errUnexpectedCall
			}
			r, _ := res[0].(bool)
			return r, errOf(res[1])
		},
		SimilarityAdjacency: func(ctx context.Context, paths []string) (index.SimilarityGraph, error) {
			res, ok := s.dispatch("SimilarityAdjacency", ctx, paths)
			if !ok {
				return index.NewSimilarityGraph(nil), errUnexpectedCall
			}
			r, _ := res[0].(index.SimilarityGraph)
			return r, errOf(res[1])
		},
		ReverseDependentPaths: func(ctx context.Context, path string) (map[string]struct{}, error) {
			res, ok := s.dispatch("ReverseDependentPaths", ctx, path)
			if !ok {
				return nil, errUnexpectedCall
			}
			r, _ := res[0].(map[string]struct{})
			return r, errOf(res[1])
		},
		TokenDF: func(ctx context.Context, branch, token, kind string) (int, error) {
			res, ok := s.dispatch("TokenDF", ctx, branch, token, kind)
			if !ok {
				return 0, errUnexpectedCall
			}
			r, _ := res[0].(int)
			return r, errOf(res[1])
		},
		SubgraphEdges: func(ctx context.Context, paths []string) ([][2]string, error) {
			res, ok := s.dispatch("SubgraphEdges", ctx, paths)
			if !ok {
				return nil, errUnexpectedCall
			}
			r, _ := res[0].([][2]string)
			return r, errOf(res[1])
		},
	})
	scriptRegistry.Store(f, s)
	t.Cleanup(func() {
		restore()
		scriptRegistry.Delete(f)
		svc.Close()
		s.finish()
	})
	return f
}

// scripted returns the script of an index made by newScriptedSearch.
func scripted(f *index.Facade) *searchScript {
	s, ok := scriptRegistry.Load(f)
	if !ok {
		panic("scripted: index was not made by newScriptedSearch")
	}
	return s.(*searchScript)
}

func errOf(v any) error {
	err, _ := v.(error)
	return err
}

type expectation struct {
	method   string
	matchers []gomock.Matcher
	ret      []any
	do       any
	min, max int
	calls    int
}

type searchScript struct {
	t        *testing.T
	mu       sync.Mutex
	byMethod map[string][]*expectation
}

func (s *searchScript) add(method string, args []any) *expectation {
	e := &expectation{method: method, min: 1, max: 1}
	for _, a := range args {
		if m, ok := a.(gomock.Matcher); ok {
			e.matchers = append(e.matchers, m)
		} else {
			e.matchers = append(e.matchers, gomock.Eq(a))
		}
	}
	s.mu.Lock()
	s.byMethod[method] = append(s.byMethod[method], e)
	s.mu.Unlock()
	return e
}

// dispatch finds the first expectation of method whose matchers accept args and
// that still has calls left, runs it, and returns its two results.
func (s *searchScript) dispatch(method string, args ...any) ([]any, bool) {
	s.mu.Lock()
	var hit *expectation
	for _, e := range s.byMethod[method] {
		if e.calls >= e.max || len(e.matchers) != len(args) {
			continue
		}
		ok := true
		for i, m := range e.matchers {
			if !m.Matches(args[i]) {
				ok = false
				break
			}
		}
		if ok {
			hit = e
			e.calls++
			break
		}
	}
	s.mu.Unlock()
	if hit == nil {
		s.t.Errorf("scripted search: unexpected call %s(%v), or its expectation was already used up", method, args)
		return nil, false
	}
	if hit.do == nil {
		return hit.ret, true
	}
	in := make([]reflect.Value, len(args))
	for i, a := range args {
		in[i] = reflect.ValueOf(a)
	}
	out := reflect.ValueOf(hit.do).Call(in)
	res := make([]any, len(out))
	for i, o := range out {
		res[i] = o.Interface()
	}
	return res, true
}

func (s *searchScript) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for m, es := range s.byMethod {
		for _, e := range es {
			if e.calls < e.min {
				s.t.Errorf("scripted search: missing call(s) to %s: want %d, got %d", m, e.min, e.calls)
			}
		}
	}
}

// call is one scripted expectation: F is the shape DoAndReturn takes and R the
// first result Return takes.
type call[F, R any] struct{ e *expectation }

func (c *call[F, R]) Return(r R, err error) *call[F, R] {
	c.e.ret = []any{r, err}
	return c
}

func (c *call[F, R]) DoAndReturn(f F) *call[F, R] {
	c.e.do = f
	return c
}

func (c *call[F, R]) Times(n int) *call[F, R] {
	c.e.min, c.e.max = n, n
	return c
}

func (c *call[F, R]) AnyTimes() *call[F, R] {
	c.e.min, c.e.max = 0, int(^uint(0)>>1)
	return c
}

func newCall[F, R any](e *expectation) *call[F, R] {
	e.ret = []any{*new(R), error(nil)}
	return &call[F, R]{e}
}

func (s *searchScript) Search(ctx, branch, q any) *call[func(context.Context, string, index.SearchOptions) ([]index.SearchResult, error), []index.SearchResult] {
	return newCall[func(context.Context, string, index.SearchOptions) ([]index.SearchResult, error), []index.SearchResult](s.add("Search", []any{ctx, branch, q}))
}

func (s *searchScript) FactExistsAt(ctx, branch, path, commit any) *call[func(context.Context, string, string, string) (bool, error), bool] {
	return newCall[func(context.Context, string, string, string) (bool, error), bool](s.add("FactExistsAt", []any{ctx, branch, path, commit}))
}

func (s *searchScript) SimilarityAdjacency(ctx, paths any) *call[func(context.Context, []string) (index.SimilarityGraph, error), index.SimilarityGraph] {
	return newCall[func(context.Context, []string) (index.SimilarityGraph, error), index.SimilarityGraph](s.add("SimilarityAdjacency", []any{ctx, paths}))
}

func (s *searchScript) ReverseDependentPaths(ctx, path any) *call[func(context.Context, string) (map[string]struct{}, error), map[string]struct{}] {
	return newCall[func(context.Context, string) (map[string]struct{}, error), map[string]struct{}](s.add("ReverseDependentPaths", []any{ctx, path}))
}

func (s *searchScript) TokenDF(ctx, branch, token, kind any) *call[func(context.Context, string, string, string) (int, error), int] {
	return newCall[func(context.Context, string, string, string) (int, error), int](s.add("TokenDF", []any{ctx, branch, token, kind}))
}

func (s *searchScript) SubgraphEdges(ctx, paths any) *call[func(context.Context, []string) ([][2]string, error), [][2]string] {
	return newCall[func(context.Context, []string) ([][2]string, error), [][2]string](s.add("SubgraphEdges", []any{ctx, paths}))
}
