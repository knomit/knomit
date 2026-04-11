package testenv

// BranchModel is a test-only reference for the P6 branch-isolation
// property test. It tracks per-branch fact contents and commit count
// with zero git awareness. NEVER extend this beyond
// {path -> content, commitCount}; if a test needs more, drop P6 instead.
type BranchModel struct {
	Facts       map[string]string
	CommitCount int
}

// StoreModel is a collection of branch models. Test-only reference
// for P6. The model mirrors the subset of store operations the property
// test performs (Write, Update, Delete, CreateBranch) and ignores
// everything else (merges, syncs, remotes). A divergence between the
// model and the real store is how P6 detects branch-isolation
// regressions.
type StoreModel struct {
	Branches map[string]*BranchModel
}

// NewStoreModel returns a StoreModel pre-seeded with a "main" branch
// (empty fact set). Every property-test scenario starts from this
// baseline, so creating child branches via CreateBranch(..., "main")
// matches the Storyboard's default behavior where main exists at repo
// boot time.
func NewStoreModel() *StoreModel {
	m := &StoreModel{Branches: map[string]*BranchModel{}}
	m.Branches["main"] = &BranchModel{Facts: map[string]string{}}
	return m
}

// CreateBranch clones the parent branch's fact map and commit count
// into a new child. Matches store.CreateBranch semantics: the child
// inherits everything visible on the parent at creation time. No-op if
// the parent doesn't exist.
func (m *StoreModel) CreateBranch(name, from string) {
	parent, ok := m.Branches[from]
	if !ok {
		return
	}
	clone := &BranchModel{Facts: map[string]string{}, CommitCount: parent.CommitCount}
	for k, v := range parent.Facts {
		clone.Facts[k] = v
	}
	m.Branches[name] = clone
}

// Write sets a fact's content on a branch and bumps the branch's
// commit count. Acts as both create and update — the real store has
// the same behavior for Facts().WriteFact. No-op if the branch
// doesn't exist.
func (m *StoreModel) Write(branch, path, content string) {
	b, ok := m.Branches[branch]
	if !ok {
		return
	}
	b.Facts[path] = content
	b.CommitCount++
}

// Update is a semantic alias for Write — the real store treats
// overwrites the same as creates.
func (m *StoreModel) Update(branch, path, content string) {
	m.Write(branch, path, content)
}

// Delete removes a fact from a branch and bumps the commit count iff
// the fact existed. This matches store.DeleteFact which errors on
// missing paths — the property test is expected to guard deletes with
// an existence check, so a missing-path delete is a caller bug and
// should be a silent no-op at the model level.
func (m *StoreModel) Delete(branch, path string) {
	b, ok := m.Branches[branch]
	if !ok {
		return
	}
	if _, exists := b.Facts[path]; !exists {
		return
	}
	delete(b.Facts, path)
	b.CommitCount++
}
