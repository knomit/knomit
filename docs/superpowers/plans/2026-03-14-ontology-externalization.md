# Ontology Externalization Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace hardcoded Go topic constants with a YAML-defined ontology loaded from git, customizable at repo init time.

**Architecture:** The `internal/fact/` package owns the `Ontology` struct, YAML parsing, anchored validation, and an embedded default YAML. `git.InitWithStorer` accepts extra files to include in the init commit. On open, the ontology is read from git and passed to handlers.

**Tech Stack:** Go, `gopkg.in/yaml.v3`, `//go:embed`

**Spec:** `docs/superpowers/specs/2026-03-14-ontology-externalization-design.md`

---

## Chunk 1: Ontology Model and Default YAML

### Task 1: Create `Ontology` and `OntologyNode` structs with YAML parsing

**Files:**
- Create: `internal/fact/ontology.go`
- Create: `internal/fact/ontology_test.go`

- [ ] **Step 1: Write tests for `ParseOntology`**

```go
// internal/fact/ontology_test.go
package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseOntology_Valid(t *testing.T) {
	yaml := []byte(`
id: test
name: Test Ontology
description: A test ontology.
topics:
  people:
    description: About people
    children:
      individuals:
        description: Specific persons
  technology:
    description: About technology
`)
	ont, err := ParseOntology(yaml)
	require.NoError(t, err)
	require.Equal(t, "test", ont.ID)
	require.Equal(t, "Test Ontology", ont.Name)
	require.Len(t, ont.Topics, 2)
	require.NotNil(t, ont.Topics["people"])
	require.Equal(t, "About people", ont.Topics["people"].Description)
	require.Len(t, ont.Topics["people"].Children, 1)
	require.Equal(t, "Specific persons", ont.Topics["people"].Children["individuals"].Description)
}

func TestParseOntology_MissingID(t *testing.T) {
	yaml := []byte(`
name: No ID
topics:
  people:
    description: About people
`)
	_, err := ParseOntology(yaml)
	require.ErrorContains(t, err, "id is required")
}

func TestParseOntology_MissingName(t *testing.T) {
	yaml := []byte(`
id: test
topics:
  people:
    description: About people
`)
	_, err := ParseOntology(yaml)
	require.ErrorContains(t, err, "name is required")
}

func TestParseOntology_NoTopics(t *testing.T) {
	yaml := []byte(`
id: test
name: Test
topics: {}
`)
	_, err := ParseOntology(yaml)
	require.ErrorContains(t, err, "at least one topic")
}

func TestParseOntology_InvalidTopicKey(t *testing.T) {
	yaml := []byte(`
id: test
name: Test
topics:
  Bad Topic:
    description: Has spaces
`)
	_, err := ParseOntology(yaml)
	require.ErrorContains(t, err, "invalid key")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/fact/ -run TestParseOntology -v`
Expected: FAIL — `ParseOntology` not defined

- [ ] **Step 2b: Ensure `gopkg.in/yaml.v3` dependency is available**

Run: `cd /Users/knomit/data/mine/knomit && go get gopkg.in/yaml.v3 && go mod tidy`
Expected: `go.mod` updated (or already present)

- [ ] **Step 3: Implement `ParseOntology`**

```go
// internal/fact/ontology.go
package fact

import (
	"fmt"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// Ontology defines the topic hierarchy for a knomit knowledge base.
type Ontology struct {
	ID          string                   `yaml:"id"`
	Name        string                   `yaml:"name"`
	Description string                   `yaml:"description"`
	Topics      map[string]*OntologyNode `yaml:"topics"`
}

// OntologyNode is a single node in the ontology tree.
type OntologyNode struct {
	Description string                   `yaml:"description"`
	Children    map[string]*OntologyNode `yaml:"children,omitempty"`
}

// validKeyRe matches lowercase alphanumeric slugs with hyphens.
var validKeyRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ParseOntology parses and validates a YAML ontology definition.
func ParseOntology(data []byte) (*Ontology, error) {
	var ont Ontology
	if err := yaml.Unmarshal(data, &ont); err != nil {
		return nil, fmt.Errorf("parse ontology: %w", err)
	}
	if ont.ID == "" {
		return nil, fmt.Errorf("parse ontology: id is required")
	}
	if ont.Name == "" {
		return nil, fmt.Errorf("parse ontology: name is required")
	}
	if len(ont.Topics) == 0 {
		return nil, fmt.Errorf("parse ontology: at least one topic is required")
	}
	for key, node := range ont.Topics {
		if err := validateKey(key); err != nil {
			return nil, fmt.Errorf("parse ontology: topic %s", err)
		}
		if err := validateChildren(key, node); err != nil {
			return nil, err
		}
	}
	return &ont, nil
}

func validateKey(key string) error {
	if !validKeyRe.MatchString(key) {
		return fmt.Errorf("invalid key %q: must be lowercase alphanumeric with hyphens", key)
	}
	return nil
}

func validateChildren(parentPath string, node *OntologyNode) error {
	for key, child := range node.Children {
		path := parentPath + "/" + key
		if err := validateKey(key); err != nil {
			return fmt.Errorf("parse ontology: %s: %s", path, err)
		}
		if err := validateChildren(path, child); err != nil {
			return err
		}
	}
	return nil
}

// TopicNames returns all top-level topic keys in sorted order.
func (o *Ontology) TopicNames() []string {
	names := make([]string, 0, len(o.Topics))
	for k := range o.Topics {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/fact/ -run TestParseOntology -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/fact/ontology.go internal/fact/ontology_test.go
git commit -m "feat(fact): add Ontology struct and ParseOntology"
```

### Task 2: Add `ValidatePath` with anchored validation

**Files:**
- Modify: `internal/fact/ontology.go`
- Modify: `internal/fact/ontology_test.go`

- [ ] **Step 1: Write tests for `ValidatePath`**

Add to `internal/fact/ontology_test.go`:

```go
func TestValidatePath(t *testing.T) {
	yaml := []byte(`
id: test
name: Test
topics:
  technology:
    description: Tech
    children:
      software:
        description: Software systems
      hardware:
        description: Physical devices
  people:
    description: People
`)
	ont, err := ParseOntology(yaml)
	require.NoError(t, err)

	tests := []struct {
		path    string
		wantErr bool
	}{
		{"technology", false},
		{"technology/software", false},
		{"technology/software/go/concurrency", false},
		{"technology/quantum", false},
		{"people", false},
		{"people/alice", false},
		{"cooking", true},
		{"", true},
		{"TECHNOLOGY", true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			err := ont.ValidatePath(tt.path)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/fact/ -run TestValidatePath -v`
Expected: FAIL — `ValidatePath` not defined

- [ ] **Step 3: Implement `ValidatePath`**

Add to `internal/fact/ontology.go`:

```go
// ValidatePath checks that path is valid against the ontology tree.
// It walks path segments as deep as the tree defines, then allows freeform below.
func (o *Ontology) ValidatePath(path string) error {
	if path == "" {
		return fmt.Errorf("validate path: empty path")
	}
	parts := strings.Split(path, "/")

	// First segment must match a topic.
	node, ok := o.Topics[parts[0]]
	if !ok {
		return fmt.Errorf("validate path: unknown topic %q: must be one of %s",
			parts[0], strings.Join(o.TopicNames(), ", "))
	}

	// Walk remaining segments as deep as the tree defines.
	for _, seg := range parts[1:] {
		if node.Children == nil {
			return nil // no more defined children — rest is freeform
		}
		child, ok := node.Children[seg]
		if !ok {
			return nil // segment not defined — freeform from here
		}
		node = child
	}
	return nil
}
```

Add `"strings"` to the import block.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/fact/ -run TestValidatePath -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/fact/ontology.go internal/fact/ontology_test.go
git commit -m "feat(fact): add ValidatePath with anchored validation"
```

### Task 3: Add `Serialize` with deterministic output

**Files:**
- Modify: `internal/fact/ontology.go`
- Modify: `internal/fact/ontology_test.go`

- [ ] **Step 1: Write round-trip test**

Add to `internal/fact/ontology_test.go`:

```go
func TestSerialize_RoundTrip(t *testing.T) {
	input := []byte(`
id: test
name: Test
description: A test.
topics:
  alpha:
    description: First
    children:
      sub-b:
        description: Sub B
      sub-a:
        description: Sub A
  beta:
    description: Second
`)
	ont, err := ParseOntology(input)
	require.NoError(t, err)

	out, err := ont.Serialize()
	require.NoError(t, err)

	// Parse again — must produce identical struct.
	ont2, err := ParseOntology(out)
	require.NoError(t, err)
	require.Equal(t, ont.ID, ont2.ID)
	require.Equal(t, ont.Name, ont2.Name)
	require.Equal(t, ont.Description, ont2.Description)
	require.Equal(t, len(ont.Topics), len(ont2.Topics))

	// Serialize again — must produce identical bytes (stable ordering).
	out2, err := ont2.Serialize()
	require.NoError(t, err)
	require.Equal(t, string(out), string(out2))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/fact/ -run TestSerialize -v`
Expected: FAIL — `Serialize` not defined

- [ ] **Step 3: Implement `Serialize`**

Add to `internal/fact/ontology.go`:

```go
// Serialize produces deterministic YAML output with sorted keys.
func (o *Ontology) Serialize() ([]byte, error) {
	// Build an ordered structure for deterministic output.
	doc := &yaml.Node{Kind: yaml.MappingNode}
	addScalar(doc, "id", o.ID)
	addScalar(doc, "name", o.Name)
	if o.Description != "" {
		addScalar(doc, "description", o.Description)
	}

	topicsNode := &yaml.Node{Kind: yaml.MappingNode}
	for _, key := range o.TopicNames() {
		nodeYAML := serializeNode(o.Topics[key])
		topicsNode.Content = append(topicsNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key},
			nodeYAML,
		)
	}
	doc.Content = append(doc.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "topics"},
		topicsNode,
	)

	return yaml.Marshal(doc)
}

func serializeNode(n *OntologyNode) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode}
	if n.Description != "" {
		addScalar(m, "description", n.Description)
	}
	if len(n.Children) > 0 {
		childNode := &yaml.Node{Kind: yaml.MappingNode}
		keys := sortedKeys(n.Children)
		for _, k := range keys {
			childNode.Content = append(childNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: k},
				serializeNode(n.Children[k]),
			)
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "children"},
			childNode,
		)
	}
	return m
}

func addScalar(m *yaml.Node, key, value string) {
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value},
	)
}

func sortedKeys(m map[string]*OntologyNode) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/fact/ -run TestSerialize -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/fact/ontology.go internal/fact/ontology_test.go
git commit -m "feat(fact): add Serialize with deterministic key ordering"
```

### Task 4: Embed default ontology YAML and add `DefaultOntology()`

**Files:**
- Create: `internal/fact/ontology_default.yaml`
- Modify: `internal/fact/ontology.go`
- Modify: `internal/fact/ontology_test.go`

- [ ] **Step 1: Write test for `DefaultOntology`**

Add to `internal/fact/ontology_test.go`:

```go
func TestDefaultOntology(t *testing.T) {
	ont := DefaultOntology()
	require.Equal(t, "general", ont.ID)
	require.Equal(t, "General Knowledge", ont.Name)
	require.Len(t, ont.Topics, 12)

	// Spot-check a few topics.
	require.NotNil(t, ont.Topics["technology"])
	require.NotNil(t, ont.Topics["technology"].Children["software"])
	require.NotNil(t, ont.Topics["people"])
	require.NotNil(t, ont.Topics["reference"])
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/fact/ -run TestDefaultOntology -v`
Expected: FAIL — `DefaultOntology` not defined

- [ ] **Step 3: Create `internal/fact/ontology_default.yaml`**

Write the full default ontology YAML file (12 topics, 2 levels deep) as defined in the spec.

- [ ] **Step 4: Add embed and `DefaultOntology()` to `ontology.go`**

Add to `internal/fact/ontology.go`:

```go
import (
	_ "embed"
	"sync"
	// ... existing imports
)

//go:embed ontology_default.yaml
var defaultOntologyYAML []byte

var (
	defaultOntology     *Ontology
	defaultOntologyOnce sync.Once
)

// DefaultOntology returns the embedded default ontology, parsed once.
func DefaultOntology() *Ontology {
	defaultOntologyOnce.Do(func() {
		ont, err := ParseOntology(defaultOntologyYAML)
		if err != nil {
			panic("embedded default ontology is invalid: " + err.Error())
		}
		defaultOntology = ont
	})
	return defaultOntology
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/fact/ -run TestDefaultOntology -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/fact/ontology_default.yaml internal/fact/ontology.go internal/fact/ontology_test.go
git commit -m "feat(fact): embed default ontology YAML and add DefaultOntology()"
```

---

## Chunk 2: Git Init and Config Changes

### Task 5: Update `git.InitWithStorer` to accept init files

**Files:**
- Modify: `internal/git/store.go` (lines 79-132, 158-185)
- Modify: `internal/git/store_test.go`

- [ ] **Step 1: Write test for `InitWithStorer` with init files**

Add to `internal/git/store_test.go`:

```go
func TestInitWithStorer_WritesInitFiles(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(gitSchema)
	require.NoError(t, err)

	s := storegit.NewStorer(db)
	initFiles := map[string]string{
		"domains/ontology.yaml": "id: test\nname: Test\n",
	}
	gs, err := InitWithStorer(s, initFiles)
	require.NoError(t, err)

	// The init file should be readable.
	content, err := gs.ReadFile("domains/ontology.yaml")
	require.NoError(t, err)
	require.Contains(t, content, "id: test")

	// The root manifest should also exist.
	_, err = gs.ReadFile("kb.md")
	require.NoError(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/git/ -run TestInitWithStorer_WritesInitFiles -v`
Expected: FAIL — wrong number of arguments to `InitWithStorer`

- [ ] **Step 3: Update `InitWithStorer` signature and implementation**

In `internal/git/store.go`, change `InitWithStorer`:

1. Add `initFiles map[string]string` parameter.
2. Change `"general.md"` to `"kb.md"`.
3. After writing `kb.md`, iterate `initFiles` and call `writeFileToStore` for each, chaining the commit hash.

```go
func InitWithStorer(s *storegit.Storer, initFiles map[string]string) (*Store, error) {
	// ... existing repo init and config ...

	rootManifest := "# Knowledge Base\n\nRoot manifest.\n"
	lastCommit, _, err := writeFileToStore(s, plumbing.ZeroHash, "kb.md", rootManifest, "init: create knowledge base")
	if err != nil {
		return nil, fmt.Errorf("git.Init: initial commit: %w", err)
	}

	// Write additional init files.
	for path, content := range initFiles {
		lastCommit, _, err = writeFileToStore(s, lastCommit, path, content, "init: "+path)
		if err != nil {
			return nil, fmt.Errorf("git.Init: write %s: %w", path, err)
		}
	}

	// ... rest of branch setup uses lastCommit instead of initCommitHash ...
}
```

- [ ] **Step 4: Update `Init` (deprecated path) to also accept `initFiles`**

Change signature to `Init(dbPath string, initFiles map[string]string)` and pass through to `InitWithStorer`.

- [ ] **Step 5: Fix all callers and update `"general.md"` references to `"kb.md"`**

Update all calls to `InitWithStorer` and `Init` to pass the new parameter. Pass `nil` at each call site for now — ontology wiring comes in Task 7.

Callers in production code:
- `cmd/knomit/main.go:72` — `git.InitWithStorer(svc.GitStorer(), nil)`
- `cmd/knomit/main.go:208` — `git.InitWithStorer(svc.GitStorer(), nil)`

Callers in test code (every test calling `git.Init(...)` needs the second param):
- `internal/git/store_test.go` — every test function calls `Init(path)`, change to `Init(path, nil)`
- `internal/git/git_extra_test.go` — same pattern

Also update all test assertions referencing `"general.md"` to `"kb.md"`:
- `internal/git/store_test.go`: `TestFileExists`, `TestListDirRoot`, `TestDiffFilesFromEmpty`, `TestListAll`, and any others asserting the root manifest filename.

This must be done in the same task to keep the codebase green between commits.

- [ ] **Step 6: Run test to verify it passes**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/git/ -run TestInitWithStorer_WritesInitFiles -v`
Expected: PASS

- [ ] **Step 7: Run all git tests to check nothing broke**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/git/ -v`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/git/store.go internal/git/store_test.go internal/git/git_extra_test.go cmd/knomit/main.go
git commit -m "feat(git): accept initFiles in InitWithStorer, rename root manifest to kb.md"
```

### Task 6: Change config default from `"general"` to `"kb"`

**Files:**
- Modify: `internal/config/config.go` (line 41)
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Update default**

In `internal/config/config.go`, change line 41:
```go
OntologyRoot: "kb",
```

- [ ] **Step 2: Update config test**

Update any test in `internal/config/config_test.go` that asserts `OntologyRoot == "general"` to assert `"kb"`.

- [ ] **Step 3: Run config tests**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): change OntologyRoot default to kb"
```

---

## Chunk 3: Wire Ontology Into Handlers

### Task 7: Wire ontology loading into `serveCmd` and `initCmd`

**Files:**
- Modify: `cmd/knomit/main.go` (lines 43-77 for serve, 190-215 for init)

- [ ] **Step 1: Update `initCmd` to write ontology on init**

In `initCmd`, after opening the store and before calling `InitWithStorer`:

```go
ontology := fact.DefaultOntology()
ontologyYAML, err := ontology.Serialize()
if err != nil {
	return fmt.Errorf("serialize ontology: %w", err)
}
initFiles := map[string]string{
	"domains/ontology.yaml": string(ontologyYAML),
}
if _, err := git.InitWithStorer(svc.GitStorer(), initFiles); err != nil {
	return fmt.Errorf("init git: %w", err)
}
```

Restructure `initCmd` to use a named `cmd` variable so flags can be registered. Add `--ontology` flag:

```go
func initCmd() *cobra.Command {
	var ontologyPath string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialise a new knomit repo",
		RunE: func(cmd *cobra.Command, args []string) error {
			// ... load ontology from ontologyPath or default ...
		},
	}
	cmd.Flags().StringVar(&ontologyPath, "ontology", "", "path to custom ontology YAML file")
	return cmd
}
```

When `ontologyPath` is set, read the file, parse with `fact.ParseOntology()`, and use it instead of the default.

- [ ] **Step 2: Update `serveCmd` auto-init path to write ontology**

In `serveCmd`, when `OpenWithStorer` fails and falls back to init:

```go
ontology := fact.DefaultOntology()
ontologyYAML, err := ontology.Serialize()
if err != nil {
	return fmt.Errorf("serialize ontology: %w", err)
}
initFiles := map[string]string{
	"domains/ontology.yaml": string(ontologyYAML),
}
gs, err = git.InitWithStorer(svc.GitStorer(), initFiles)
```

- [ ] **Step 3: Add ontology loading on open path in `serveCmd`**

After `git.OpenWithStorer()` succeeds, load the ontology:

```go
var ontology *fact.Ontology
ontologyYAML, err := gs.ReadFile("domains/ontology.yaml")
if err != nil {
	log.Warn().Msg("domains/ontology.yaml not found, using default ontology")
	ontology = fact.DefaultOntology()
} else {
	ontology, err = fact.ParseOntology([]byte(ontologyYAML))
	if err != nil {
		return fmt.Errorf("parse ontology: %w", err)
	}
}
```

Pass `ontology` to `mcp.NewServer` and `web.NewRouter`.

- [ ] **Step 4: Build to verify compilation**

Run: `cd /Users/knomit/data/mine/knomit && go build ./cmd/knomit/`
Expected: Success (may fail until Task 8 updates handler signatures)

- [ ] **Step 5: Commit**

```bash
git add cmd/knomit/main.go
git commit -m "feat(cmd): wire ontology loading into serve and init"
```

### Task 8: Update `mcp.NewServer` and `LearnHandler` to accept `*fact.Ontology`

**Files:**
- Modify: `internal/mcp/server.go` (line 73)
- Modify: `internal/mcp/learn.go` (lines 96, 143-152)
- Modify: `internal/mcp/learn_test.go`

- [ ] **Step 1: Write test for learn with ontology validation**

Add to `internal/mcp/learn_test.go`:

```go
func TestLearnHandler_InvalidTopic(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)

	ontology := fact.DefaultOntology()
	handler := LearnHandler(gs, idx, "kb", ontology)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "test",
		"facts": []interface{}{
			map[string]interface{}{
				"topic":    "cooking",  // invalid topic
				"category": "pasta",
				"title":    "Test",
				"body":     "Test body",
			},
		},
	}

	result, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.True(t, result.IsError)
	// Error should mention unknown topic.
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/mcp/ -run TestLearnHandler_InvalidTopic -v`
Expected: FAIL — wrong arguments

- [ ] **Step 3: Update `NewServer` signature**

In `internal/mcp/server.go`:

```go
func NewServer(gs GitStore, idx SearchIndex, llmAdapter llm.LLMAdapter, profile, ontologyRoot string, ontology *fact.Ontology) *server.MCPServer {
```

Update `LearnHandler` call:
```go
s.AddTool(learnTool(), LearnHandler(gs, idx, ontologyRoot, ontology))
```

Add import for `"knomit/internal/fact"`.

- [ ] **Step 4: Update `LearnHandler` signature and validation**

In `internal/mcp/learn.go`:

Change signature:
```go
func LearnHandler(gs GitStore, idx SearchIndex, ontologyRoot string, ontology *fact.Ontology) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
```

Replace topic validation (lines 143-146):
```go
// Old:
// topic := fact.Topic(fi.Topic)
// if err := topic.Validate(); err != nil { ... }

// New:
topicCategory := fi.Topic
if fi.Category != "" {
	topicCategory = fi.Topic + "/" + fi.Category
}
if err := ontology.ValidatePath(topicCategory); err != nil {
	return mcpgo.NewToolResultError(fmt.Sprintf("fact %d: %v", i, err)), nil
}
```

Change `buildFactPath` signature and update its call site:
```go
func buildFactPath(ontologyRoot string, topic string, category string) string {
```

Update the call at learn.go line 152 to pass the string directly:
```go
path := buildFactPath(ontologyRoot, fi.Topic, fi.Category)
```

- [ ] **Step 5: Update `ProfileInstructions` to accept `*fact.Ontology`**

In `internal/mcp/instructions.go`:

```go
func ProfileInstructions(profile, ontologyRoot string, ontology *fact.Ontology) string {
```

Update `baseInstructionsText` to accept `*fact.Ontology` and dynamically generate the topic list:

```go
func baseInstructionsText(ontologyRoot string, ontology *fact.Ontology) string {
```

Replace the hardcoded topic list (lines 15-16) with a dynamically generated list from `ontology.TopicNames()` and their descriptions.

Update the `NewServer` call to `ProfileInstructions(profile, ontologyRoot, ontology)`.

- [ ] **Step 6: Fix all test files that call `NewServer`, `LearnHandler`, or `ProfileInstructions`**

Update calls in test files to pass `fact.DefaultOntology()` as the new parameter:
- `internal/mcp/learn_test.go`
- `internal/mcp/mcp_extra_test.go`
- `internal/mcp/instructions_test.go`
- Any other test calling these functions

- [ ] **Step 7: Run all mcp tests**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/mcp/ -v`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/mcp/server.go internal/mcp/learn.go internal/mcp/instructions.go internal/mcp/learn_test.go internal/mcp/mcp_extra_test.go internal/mcp/instructions_test.go
git commit -m "feat(mcp): accept *fact.Ontology in NewServer and LearnHandler"
```

### Task 9: Update web handlers to pass ontology

**Files:**
- Modify: `internal/web/server.go`
- Modify: `internal/web/handlers.go`
- Modify: `internal/web/handlers_test.go`
- Modify: `internal/web/handlers_extra_test.go`

- [ ] **Step 1: Update `web.NewRouter` to accept `*fact.Ontology`**

Add `ontology *fact.Ontology` parameter to `NewRouter`. Pass it through wherever `ontologyRoot` was used alone and ontology validation is needed.

- [ ] **Step 2: Fix web test files**

Update test calls to `NewRouter` to pass `fact.DefaultOntology()`.

- [ ] **Step 3: Run web tests**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/web/ -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/web/server.go internal/web/handlers.go internal/web/handlers_test.go internal/web/handlers_extra_test.go
git commit -m "feat(web): pass *fact.Ontology through web router"
```

---

## Chunk 4: Cleanup and Full Verification

### Task 10: Delete `topic.go` and `topic_test.go`

**Note:** `internal/fact/epistemic_type.go` is unrelated and remains unchanged.

**Files:**
- Delete: `internal/fact/topic.go`
- Delete: `internal/fact/topic_test.go`

- [ ] **Step 1: Remove all remaining references to `fact.Topic`**

Search for any remaining references to `fact.Topic`, `fact.AllTopics`, or any `Topic` constant. By this point, `learn.go` should be the only file that imported it, and that was already updated in Task 8.

- [ ] **Step 2: Delete the files**

```bash
cd /Users/knomit/data/mine/knomit
git rm internal/fact/topic.go internal/fact/topic_test.go
```

- [ ] **Step 3: Build to verify no compilation errors**

Run: `cd /Users/knomit/data/mine/knomit && go build ./...`
Expected: Success

- [ ] **Step 4: Commit**

```bash
git commit -m "refactor(fact): delete hardcoded topic.go in favor of ontology YAML"
```

### Task 11: Update test fixtures from `"general"` to `"kb"`

**Files:**
- Modify: test files that use `"general"` as ontologyRoot in fixtures

Known files (from grep):
- `internal/config/config_test.go` (handled in Task 6)
- `internal/web/handlers_test.go`
- `internal/web/handlers_extra_test.go`
- `internal/mcp/explore_test.go`
- `internal/mcp/mcp_extra_test.go`
- `internal/mcp/retract_test.go`
- `internal/mcp/why_test.go`
- `internal/mcp/update_test.go`
- `internal/mcp/instructions_test.go`
- `internal/mcp/learn_test.go`
- `internal/git/store_test.go`
- `internal/store/graph_test.go`
- `internal/store/index_test.go`
- `internal/store/integration_test.go`
- `internal/store/parsefact_test.go`

- [ ] **Step 1: Replace `"general"` with `"kb"` in all test fixtures**

In each test file, replace ontologyRoot references from `"general"` to `"kb"`. Also update any path fixtures like `"general/technology/..."` to `"kb/technology/..."`.

- [ ] **Step 2: Run full test suite**

Run: `cd /Users/knomit/data/mine/knomit && go test ./... 2>&1 | tail -30`
Expected: All PASS

- [ ] **Step 3: Commit**

Stage all modified test files explicitly (list each file) and commit:

```bash
git commit -m "test: update fixtures from general to kb ontology root"
```

### Task 12: Full build and test verification

- [ ] **Step 1: Build the binary**

Run: `cd /Users/knomit/data/mine/knomit && go build ./cmd/knomit/`
Expected: Success

- [ ] **Step 2: Run full test suite**

Run: `cd /Users/knomit/data/mine/knomit && go test ./...`
Expected: All PASS

- [ ] **Step 3: Verify the init flow end-to-end**

Run: `cd /Users/knomit/data/mine/knomit && go run ./cmd/knomit/ init` (with a temp repo path)
Expected: Repo initialized, `domains/ontology.yaml` exists in git with default ontology content.
