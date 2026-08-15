package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"knomit/internal/fact"
	"knomit/internal/refs"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/synthesize"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"
)

// localEvidenceRefs returns the subset of a fact's refs that are genuinely-local
// fact edges eligible to contribute evidence weight. It drops:
//   - the fact's own resulting path (a dedup-merge appends it as lineage; a fact
//     is never its own evidence source), and
//   - everything ClassifyRef does not call a local fact: a FOREIGN kb:// ref
//     points into another repo, and a source citation is not a fact edge even
//     when the file it cites is markdown (src://…/plans/x.md ends in ".md").
//
// localRepoID must be the writing repo's id, NOT "". Refs are stored canonical
// (kb://<own-id>/<path>), so a merged fact carries the existing fact's refs in
// qualified form; classifying those with an empty id would read every one of
// them as foreign and silently compute the weight from nothing.
func localEvidenceRefs(f fact.Fact, localRepoID string) []string {
	var localRefs []string
	// The self-path may be stored either bare (as the merge appended it) or
	// canonical (as it was written), so compare on the classified path rather
	// than the raw string.
	self := fact.ClassifyRef(f.Path(), localRepoID).Path
	for _, r := range f.Refs {
		c := fact.ClassifyRef(r, localRepoID)
		if c.Kind != fact.RefLocalFact || c.Path == self {
			continue
		}
		localRefs = append(localRefs, c.Path)
	}
	return localRefs
}

// defaultConfidence and defaultSources are the values knomit_learn assumes
// for a fact that omits them. They are declared here and substituted into the
// input schema below so the number the tool advertises and the number the
// handler applies cannot drift — the failure mode that let the schema promise
// `sources: 1` while every omitting caller wrote 0.
const (
	defaultConfidence = 0.7
	defaultSources    = 1
)

// learnTool returns the Tool definition for knomit_learn.
func learnTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_learn",
		mcpgo.WithDescription("Write one or more facts to the knowledge base in a single commit."),
		mcpgo.WithString("moment_name",
			mcpgo.Required(),
			mcpgo.Description("A short label for this learning moment."),
		),
		mcpgo.WithArray("facts",
			mcpgo.Required(),
			mcpgo.Description("Array of fact objects to write."),
			mcpgo.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					// topic and category are REQUIRED for knowledge, but cannot
					// say so in the schema's `required` list: `path` replaces
					// them for private state, and JSON Schema cannot express
					// "one or the other" in a list. The descriptions carry the
					// rule instead.
					"topic":    map[string]any{"type": "string", "description": "Top-level ontology topic (e.g. technology, people, science). REQUIRED unless you supply path."},
					"category": map[string]any{"type": "string", "description": "Category path within the topic (e.g. languages/go/concurrency). REQUIRED unless you supply path."},
					"path":     map[string]any{"type": "string", "description": "PRIVATE STATE ONLY. An explicit repo path under " + fact.PrivateRoot + "/<area>/, e.g. " + fact.PrivateRoot + "/<area>/<name>.md, where <area> is a directory name containing no dot, for machinery that is not knowledge — a periodic job's bookkeeping. Mutually exclusive with topic/category: supply one or the other, never both. A fact written here is INVISIBLE to knomit_query, the UI and export, by design; address it later by this exact path. Fails if the path already exists — use knomit_update to write a new revision."},
					"title":    map[string]any{"type": "string", "description": "Fact title (short, descriptive)."},
					"body":     map[string]any{"type": "string", "description": "Fact body in natural language. State compound conditions in full — name every component; never abbreviate a multi-part condition into a catchier summary. Include the consequence a consumer should act on. For rules and policies, name the foreseeable misreading (what the fact does NOT mean) if you can see one."},
					// kind/type/origin come from the shared fragments in
					// factschema.go so this schema, knomit_update's, and the
					// server instructions cannot disagree. knomit_learn mints
					// facts, so it declares the defaults it will assume.
					"kind":       kindProperty(fact.DefaultKind),
					"type":       typeProperty(fact.DefaultEpistemicType),
					"origin":     originProperty(),
					"domain":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Cross-cutting domain tags."},
					"confidence": map[string]any{"type": "number", "description": "Certainty level 0.0–1.0.", "default": defaultConfidence},
					"sources":    map[string]any{"type": "integer", "description": "Count of independent corroborations — how many independent agents or observations produced this fact.", "default": defaultSources},
					"entities":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Entities this fact mentions."},
					"refs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "References, in four forms. " +
						"(1) A fact in THIS repo: use the bare path, `kb/<topic>/…/<id>.md` — exactly as it appears in a knomit_query result. You never need this repo's id: the server rewrites the ref to the canonical `kb://<repo-id>/<path>` form on write. The target MUST already exist, or be written in this same call — all facts in one call are committed together, so they may cite each other in any order, including circularly. Citing a fact that will not exist REJECTS the whole call and names every offending ref. " +
						"(2) A fact in ANOTHER repo: `kb://<repo-id>/<path>`. Do not build this yourself — COPY it verbatim from the knomit_query or knomit_explain result that gave you the fact, which already returns other repos' paths in this form. (knomit_repos lists every mounted repo's id if you need to look one up.) Never checked. " +
						"(3) Source code: `src://<source-repo-id>/<path>@<commit>:<blob>`, with FULL 40-hex commit and blob, optionally `#L<start>-L<end>`. This id is the SOURCE repo's, not a knomit repo id — get all three components by running git in the checkout you are citing: `git rev-list --max-parents=0 HEAD | cut -c1-12` (repo id), `git rev-parse HEAD` (commit), `git rev-parse <commit>:<path>` (blob). That last command failing IS the check — the server holds no source objects and cannot verify src refs for you, so never cite source that does not exist in the repo's history. The older `src://<name>/<path>@<commit>` form is still accepted and is never rewritten. " +
						"(4) An external URL: `https://…` or `file:///…`."},
				},
				"required": []string{"title", "body"},
			}),
		),
	)
}

// learnFactInput is the JSON shape of a single fact in the input array.
type learnFactInput struct {
	Topic    string `json:"topic"`
	Category string `json:"category"`
	// Path, when set, is an explicit private-state path under
	// fact.PrivateRoot/<area>/ and REPLACES topic+category: no ontology
	// validation, no UUID minting, no dedup-merge. See learnTool's description.
	Path   string   `json:"path"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Kind   string   `json:"kind"`
	Type   string   `json:"type"`
	Domain []string `json:"domain"`
	// Confidence and Sources are pointers so an ABSENT field can be told
	// apart from an explicit zero. Both advertise a JSON Schema `default`
	// (0.7 and 1), but a schema default is documentation for the client, not
	// server-side coercion — as plain scalars they silently wrote Go's zero
	// whenever a caller omitted them, and a fact with sources 0 contributes
	// nothing to any downstream evidence weight (§5.2 multiplies by sources).
	// An explicit 0 stays 0: §2.2 requires only >= 0.
	Confidence *float64 `json:"confidence"`
	Sources    *int     `json:"sources"`
	Entities   []string `json:"entities"`
	Refs       []string `json:"refs"`
	Origin     string   `json:"origin"`
}

// reserialize re-renders f and overwrites the entry at path in the
// pending-write map.
//
// Every stage after the initial build mutates a fact in place (refs gain a
// subsumed hypothesis, a merge replaces it wholesale, an evidence weight is
// stamped on) and must push the new bytes back into files. Doing that by hand
// at four sites is how a stage silently ends up writing a stale rendering.
//
// path is an explicit parameter rather than f.Path() because the two are NOT
// interchangeable, and conflating them writes the fact to the wrong place.
// fact.NewFact lowercases the ENTIRE path it is given, ontology root
// included, whereas fact.BuildFactPath and fact.NormalizePath pass the root
// through verbatim and normalize only the segments beneath it — and
// config.Validate accepts an ontology_root of "KB". So for an uppercase root
// f.Path() is "kb/tech/x.md" while the fact's real home is "KB/tech/x.md":
// keying off f.Path() commits the file OUTSIDE the ontology root, where every
// ontology-scoped query misses it.
//
// The invariant this encodes, and which the rest of learn.go now follows: the
// on-disk path preserves the configured root verbatim and is carried
// explicitly alongside the fact, while f.Path() is a NORMALIZED IDENTITY —
// good for comparing two facts, never for locating one on disk.
//
// reserialize only ever adds. A merge that retargets a fact to the existing
// fact's path must delete the old key itself.
func reserialize(files map[string]string, path string, f fact.Fact) error {
	serialized, err := fact.SerializeFact(f)
	if err != nil {
		return err
	}
	files[path] = serialized
	return nil
}

// validateBatchTypeConsistency rejects a batch that mixes observed types with
// inferred ones (hypothesis, methodology).
//
// The two arise from different acts: observed facts record something the agent
// saw, inferred facts record something it reasoned to. A single commit that
// contains both makes the moment's provenance ambiguous — a later reader
// cannot tell which half the moment was actually about.
func validateBatchTypeConsistency(inputs []learnFactInput) error {
	hasObserved, hasInferred := false, false
	for _, fi := range inputs {
		eType := fact.Type(fi.Type)
		if eType == "" {
			eType = fact.DefaultEpistemicType
		}
		if eType == fact.Hypothesis || eType == fact.Methodology {
			hasInferred = true
		} else {
			hasObserved = true
		}
	}
	if hasObserved && hasInferred {
		return fmt.Errorf("cannot mix observed types (observation, concept, etc.) and inferred types (hypothesis, methodology) in a single learn call")
	}
	return nil
}

// validateAndBuildFacts validates each input against the ontology, assigns it a
// server-generated path, and renders it into the pending-write map.
//
// It touches neither ctx nor the store: nothing here reads or writes the repo,
// so the whole stage is a pure function of (ontology, inputs). Returns the
// built facts, their ontology topic paths (cached because the dedup-merge stage
// must re-run ValidateFact and would otherwise have to re-derive them from the
// on-disk path), their on-disk paths, and the files to write.
//
// The paths slice is returned rather than recovered from facts[i].Path()
// because those two disagree under an uppercase ontology_root — see
// reserialize for the full reasoning. paths[i] is the authoritative location
// of facts[i]; later stages that retarget a fact (dedup-merge) update it.
func validateAndBuildFacts(ontology *fact.Ontology, ontologyRoot string, inputs []learnFactInput) ([]fact.Fact, []string, []string, map[string]string, error) {
	files := make(map[string]string, len(inputs))
	facts := make([]fact.Fact, len(inputs))
	topicCategories := make([]string, len(inputs))
	paths := make([]string, len(inputs))
	for i, fi := range inputs {
		var path string
		var topicCategory string
		if fi.Path != "" {
			// Private-state fact: an explicit caller-chosen path, no ontology.
			if fi.Topic != "" || fi.Category != "" {
				return nil, nil, nil, nil, fmt.Errorf(
					"fact %d: path cannot be combined with topic or category — supply one or the other", i)
			}
			path = fact.NormalizePath(ontologyRoot, fi.Path)
			if !fact.IsWritablePrivatePath(path) {
				return nil, nil, nil, nil, fmt.Errorf(
					"fact %d: %s is not writable; an explicit path must be under %s/<area>/",
					i, path, fact.PrivateRoot)
			}
			// Empty topicCategory is the downstream signal: skip ontology
			// validation and skip dedup-merge for this fact.
			topicCategory = ""
		} else {
			topicCategory = fi.Topic
			if fi.Category != "" {
				topicCategory = fi.Topic + "/" + fi.Category
			}
			if ontology != nil {
				if err := ontology.ValidatePath(topicCategory); err != nil {
					return nil, nil, nil, nil, fmt.Errorf("fact %d: %v", i, err)
				}
			}
			if strings.TrimSpace(fi.Category) == "" {
				return nil, nil, nil, nil, fmt.Errorf("fact %d: category is required", i)
			}
			path = fact.BuildFactPath(ontologyRoot, fi.Topic, fi.Category)
			// A private path can only be reached deliberately, via `path`.
			// Reaching one from topic/category means the ontology has a
			// dot-prefixed topic, which is a corrupt ontology, not a request.
			if fact.IsPrivatePath(path) {
				return nil, nil, nil, nil, fmt.Errorf(
					"fact %d: topic/category resolves to %s; a path segment beginning with '.' is private and cannot hold a fact", i, path)
			}
		}
		topicCategories[i] = topicCategory

		domain := fi.Domain
		if domain == nil {
			domain = []string{}
		}
		entities := fi.Entities
		if entities == nil {
			entities = []string{}
		}
		refs := fi.Refs
		if refs == nil {
			refs = []string{}
		}
		// Resolve kind and leaf type. SerializeFact (called below)
		// validates the (kind, type) pair via the same path that
		// ParseFact uses, so we don't pre-validate here.
		kind := fact.Kind(fi.Kind)
		if kind == "" {
			kind = fact.DefaultKind
		}
		eType := fact.Type(fi.Type)
		if eType == "" && kind == fact.Epistemic {
			eType = fact.DefaultEpistemicType
		}
		f := fact.NewFact(path)
		f.Title = fi.Title
		f.Body = fi.Body
		f.Kind = kind
		f.Type = eType
		f.Domain = domain
		f.Confidence = defaultConfidence
		if fi.Confidence != nil {
			f.Confidence = *fi.Confidence
		}
		f.Sources = defaultSources
		if fi.Sources != nil {
			f.Sources = *fi.Sources
		}
		f.Entities = entities
		f.Refs = refs
		// Explicit origin: set it and let SerializeFact validate, the
		// same way (kind, type) is handled above — one chokepoint, so
		// synthesize and web write paths get the identical check.
		// Empty is left unset so SerializeFact/ParseFact apply their
		// defaults (authored, or distilled for legacy synthesis facts).
		// This is how an agent persists a previewed discovery proposal
		// as origin=discovered.
		if fi.Origin != "" {
			f.Origin = fact.Origin(fi.Origin)
		}
		// A private-state fact has no ontology placement to validate against.
		if ontology != nil && topicCategory != "" {
			if err := fact.ValidateFact(ontology, topicCategory, f); err != nil {
				return nil, nil, nil, nil, fmt.Errorf("fact %d: %v", i, err)
			}
		}
		facts[i] = f
		paths[i] = path
		if err := reserialize(files, path, f); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("fact %d: serialize: %v", i, err)
		}
	}
	return facts, topicCategories, paths, files, nil
}

// newFactWins reports whether the incoming fact's IDENTITY (title, body, kind,
// type, origin) survives a dedup merge. Higher confidence wins; on a tie the
// incoming fact wins unless it is backed by strictly fewer sources.
//
// Split out from mergeFacts because the caller needs the same answer for a
// different reason: the precomputed dedup vector was embedded over the incoming
// fact's title+body, so it is only donatable when the incoming identity wins.
func newFactWins(newFact, existing fact.Fact) bool {
	if newFact.Confidence != existing.Confidence {
		return newFact.Confidence > existing.Confidence
	}
	return newFact.Sources >= existing.Sources
}

// mergeFacts folds an incoming near-duplicate into the existing fact it
// matched, returning the fact to write at the EXISTING path.
//
// The merge is asymmetric by design: the winner (see newFactWins) contributes
// the identity — title, body, kind, type, origin — while the metadata is
// always pooled, because both facts are evidence of the same thing. Confidence
// takes the max rather than the winner's, and sources add: two independent
// observations of one fact are worth more than either alone. Pure.
//
// existingPath is the existing fact's RAW on-disk path and is deliberately
// distinct from existing.Path(). The two differ in case: existing came from
// ParseFact(existingPath, …), whose final step is NewFact(existingPath) =
// strings.ToLower(existingPath). The merged fact's IDENTITY uses the
// normalized form (that is what identity means here, and lowercasing is
// idempotent, so this matches the pre-refactor behaviour exactly), but the
// lineage REF must use the raw path — a ref is a pointer to a file, and for a
// mixed-case fact file like "kb/Tech/Foo.md" the normalized "kb/tech/foo.md"
// names nothing on disk, so every provenance walk through that edge dangles.
func mergeFacts(newFact, existing fact.Fact, existingPath string) fact.Fact {
	merged := fact.NewFact(existing.Path())
	if newFactWins(newFact, existing) {
		merged.Title = newFact.Title
		merged.Body = newFact.Body
		merged.Kind = newFact.Kind
		merged.Type = newFact.Type
		// New fact's identity wins, so its origin wins too.
		merged.Origin = newFact.Origin
	} else {
		merged.Title = existing.Title
		merged.Body = existing.Body
		merged.Kind = existing.Kind
		merged.Type = existing.Type
		// Existing fact's identity wins, so its origin wins too.
		merged.Origin = existing.Origin
	}
	merged.Domain = fact.UnionStrings(newFact.Domain, existing.Domain)
	merged.Entities = fact.UnionStrings(newFact.Entities, existing.Entities)
	merged.Confidence = max(newFact.Confidence, existing.Confidence)
	merged.Sources = newFact.Sources + existing.Sources
	// Raw path, not existing.Path(): see the doc comment above.
	merged.Refs = fact.AppendUnique(fact.UnionStrings(newFact.Refs, existing.Refs), existingPath)
	return merged
}

// subsumeHypothesis records that incoming fact f supersedes the hypothesis at
// hypothesisPath: f cites it as lineage, and the hypothesis is STAGED for
// retraction rather than deleted now.
//
// The staging is the point. The retraction is applied by the same
// BatchWriteFacts call that writes f, so the two are one commit. A separate
// DeleteFact would be a second commit: if it failed we would have written an
// observation whose refs point at a live hypothesis it claims to have
// subsumed, and if the batch write then failed we would have retracted a fact
// for nothing. AppendUnique on both sides: two incoming observations can match
// the same hypothesis, and deleting one path twice in a batch is at best
// wasted tree work.
func subsumeHypothesis(f fact.Fact, retract []string, hypothesisPath string) (fact.Fact, []string) {
	retract = fact.AppendUnique(retract, hypothesisPath)
	f.Refs = fact.AppendUnique(f.Refs, hypothesisPath)
	return f, retract
}

// dedupEmbed batch-embeds every incoming fact upfront so each dedup Search can
// reuse a precomputed vector instead of paying a fresh ONNX call. Returns nil
// when no embedder is configured or the batch fails — both are non-fatal, and
// callers fall back to per-fact embedding inside Search.
func dedupEmbed(ctx context.Context, batchEmb store.BatchEmbedder, facts []fact.Fact) [][]float32 {
	if batchEmb == nil || len(facts) == 0 {
		return nil
	}
	titles := make([]string, len(facts))
	bodies := make([]string, len(facts))
	for i, f := range facts {
		titles[i] = f.Title
		bodies[i] = f.Body
	}
	vecs, err := batchEmb.EmbedDocuments(ctx, titles, bodies)
	if err != nil {
		log.Warn().Err(err).Int("count", len(titles)).Msg("learn: batch embed failed; dedup falls back to per-fact embedding and donations are skipped")
		return nil
	}
	return vecs
}

// applyDedupMerge searches each incoming fact's own category directory for a
// near-duplicate and folds any match in, mutating facts and files in place.
// Three outcomes per fact: no match (write as-is), the match is a hypothesis
// this fact subsumes (write as-is, stage the hypothesis for retraction), or a
// genuine duplicate (merge and write at the EXISTING path).
//
// Returns the precomputed-embedding donation map keyed by FINAL on-disk path —
// upsert reads it from ctx and skips its own ONNX call — plus the hypothesis
// paths to retract in the same commit as the write that subsumes them.
func applyDedupMerge(
	ctx context.Context,
	s mcpStore,
	agentBranch string,
	ontology *fact.Ontology,
	batchEmb store.BatchEmbedder,
	facts []fact.Fact,
	topicCategories []string,
	paths []string,
	files map[string]string,
) (map[string][]float32, []string, map[string][]string, error) {
	// The near-duplicate cosine floor is model-dependent (see internal/embeddings/params).
	dedupThreshold := store.EmbedderThresholds(batchEmb).Dedup
	dedupVecs := dedupEmbed(ctx, batchEmb, facts)

	// donatePaths[i] is the on-disk path that dedupVecs[i] corresponds to, or
	// "" to suppress donation (used when the merge kept the EXISTING fact's
	// title+body, so our vector — computed over the incoming title+body —
	// would not describe what actually gets written).
	// Seeded from paths, not facts[i].Path(): this map is consumed by upsert to
	// key embeddings by the path actually committed, so it must track the
	// on-disk location. See reserialize.
	donatePaths := make([]string, len(facts))
	copy(donatePaths, paths)
	var retract []string
	// priorRefs[onDiskPath] is what the EXISTING fact at that path already
	// cited, for facts this merge folds into one. Those refs resolved at the
	// commit that wrote them; the ref gate must not re-judge them just because
	// a merge is rewriting the file that carries them.
	priorRefs := make(map[string][]string)

	for i, f := range facts {
		// Private-state facts are not knowledge and have no category
		// neighbourhood to merge with. They are also unindexed, so the search
		// below would return nothing anyway — skipping is explicit rather than
		// accidental, and saves the Search round trip. It saves no embedding:
		// dedupEmbed above batch-embeds the WHOLE input list before this loop
		// starts, private facts included.
		//
		// The donation is dropped for the same reason the "existing wins"
		// branch below drops it: embByPath is consumed by upsert, and nothing
		// ever calls upsert for a private path (every indexing walker skips
		// it), so an entry here would be a vector for a row that is never
		// written. Leaving it in was harmless but described a data flow that
		// does not happen.
		if topicCategories[i] == "" {
			donatePaths[i] = ""
			continue
		}
		// Search scope is derived from the on-disk path so the category
		// directory carries the configured ontology root's real case.
		categoryDir := paths[i][:strings.LastIndex(paths[i], "/")]
		sq := store.SearchOptions{
			Text:          f.Title + " " + f.Body,
			Path:          categoryDir,
			MinSimilarity: dedupThreshold,
			Limit:         1,
		}
		if dedupVecs != nil && i < len(dedupVecs) && len(dedupVecs[i]) > 0 {
			sq.QueryVec = dedupVecs[i]
		}
		results, err := s.factQuery.Search(ctx, agentBranch, sq)
		if err != nil || len(results) == 0 {
			continue
		}

		match := results[0]
		// Read existing fact to get its full metadata (refs, etc.)
		readResult, readErr := s.facts.ReadFact(ctx, agentBranch, match.Path, nil)
		if readErr != nil {
			continue
		}
		existingFact, parseErr := fact.ParseFact(match.Path, readResult.Content)
		if parseErr != nil {
			continue
		}

		// Type-aware dedup: if the existing fact is a hypothesis and the new
		// fact is not, the observation subsumes the hypothesis. The
		// observation is written at its OWN path — it is a new fact, not a
		// revision of the prediction it settles.
		if existingFact.Type == fact.Hypothesis && f.Type != fact.Hypothesis {
			f, retract = subsumeHypothesis(f, retract, match.Path)
			facts[i] = f
			if err := reserialize(files, paths[i], f); err != nil {
				return nil, nil, nil, fmt.Errorf("fact %d: serialize subsumed: %v", i, err)
			}
			continue
		}

		merged := mergeFacts(f, existingFact, match.Path)
		if newFactWins(f, existingFact) {
			// dedup vector still describes the merged content (same title+body
			// as the new fact); just retarget to the existing path it now lives at.
			// match.Path, not merged.Path(): this keys the embedding donation,
			// which upsert looks up by committed on-disk path.
			donatePaths[i] = match.Path
		} else {
			// dedup vector was computed over f's title+body; existing wins so
			// merged content differs. Drop the donation — upsert will fall
			// through to a fresh embed.
			donatePaths[i] = ""
		}

		// Re-validate the merged fact against ontology rules. The merge
		// may union entities/domain or pull values from the existing
		// fact, so the result is not guaranteed to satisfy rules just
		// because the input did. Without this re-check, a violation
		// would surface later as an opaque serialize error.
		if ontology != nil {
			if err := fact.ValidateFact(ontology, topicCategories[i], merged); err != nil {
				return nil, nil, nil, fmt.Errorf("fact %d: dedup-merge: %v", i, err)
			}
		}

		// Retarget the fact from its freshly-minted path to the existing
		// fact's path. Both keys must be the real on-disk strings: deleting
		// f.Path() here removed a key that was never inserted (the build stage
		// keyed by the raw path), so the merge left the new-fact file in place
		// and the commit wrote BOTH the original and the merged file.
		delete(files, paths[i])
		paths[i] = match.Path
		priorRefs[match.Path] = existingFact.Refs
		if err := reserialize(files, match.Path, merged); err != nil {
			return nil, nil, nil, fmt.Errorf("fact %d: serialize merged: %v", i, err)
		}
		facts[i] = merged
	}

	// Build the donation map keyed by final on-disk path. Empty/missing
	// entries simply fall through to embedding at upsert time.
	embByPath := make(map[string][]float32, len(facts))
	for i := range donatePaths {
		if donatePaths[i] == "" {
			continue
		}
		if i >= len(dedupVecs) || len(dedupVecs[i]) == 0 {
			continue
		}
		embByPath[donatePaths[i]] = dedupVecs[i]
	}
	return embByPath, retract, priorRefs, nil
}

// computeEvidenceWeights stamps an evidence weight on machine-origin derived
// facts (distilled/discovered) that cite local facts, mirroring what the
// synthesis and discovery pipelines do when they write directly. Scoped to
// non-authored origins so ordinary learn calls are unaffected — only
// previewed-then-saved pipeline output gets a weight. Mutates facts and files
// in place.
func computeEvidenceWeights(ctx context.Context, s mcpStore, agentBranch, localRepoID string, facts []fact.Fact, paths []string, files map[string]string) error {
	for i := range facts {
		f := facts[i]
		if f.Origin != fact.Distilled && f.Origin != fact.Discovered {
			continue
		}
		localRefs := localEvidenceRefs(f, localRepoID)
		if len(localRefs) == 0 {
			continue
		}
		w := synthesize.ComputeEvidenceWeight(ctx, s.facts, agentBranch, localRepoID, localRefs)
		if w <= 0 || w == f.EvidenceWeight {
			continue
		}
		f.EvidenceWeight = w
		facts[i] = f
		if err := reserialize(files, paths[i], f); err != nil {
			return fmt.Errorf("fact %d: serialize weighted: %v", i, err)
		}
	}
	return nil
}

// LearnHandler returns the handler function for knomit_learn.
// If embedder is non-nil, dedup checks batch-embed all incoming facts upfront
// instead of embedding one-at-a-time inside each Search call.
func LearnHandler(embedders ...store.BatchEmbedder) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	var batchEmb store.BatchEmbedder
	if len(embedders) > 0 {
		batchEmb = embedders[0]
	}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		b := repos.BindingFromContext(ctx)
		if !b.WriteOK() {
			return mcpgo.NewToolResultError(fmt.Sprintf(
				"read-only view: branch %q is not writable; facts are authored on %q",
				b.WriteMountBranch(), b.Write().AgentBranch())), nil
		}
		ri := b.Write()
		s, release, err := storeIndices(ri)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		defer release()
		agentBranch := ri.AgentBranch()
		ontologyRoot := ri.OntologyRoot()
		ontology := ri.Ontology()

		// The one authority over this call's refs: it decides what resolves and
		// what the stored form is. Built once and threaded, because the repo id
		// it carries is also what tells a kb://<own-id>/… ref (a local edge)
		// from a foreign one everywhere below — evidence weight included.
		gate := refs.New(fact.ID12(ri.ID()), refs.FromFactQuery(s.factQuery, agentBranch))

		// 1. Parse arguments.
		momentName := req.GetString("moment_name", "")
		if momentName == "" {
			return mcpgo.NewToolResultError("moment_name is required"), nil
		}

		// Parse facts from the arguments.
		var factInputs []learnFactInput
		if err := unmarshalArg(req, "facts", &factInputs); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if len(factInputs) == 0 {
			return mcpgo.NewToolResultError("facts must not be empty"), nil
		}

		// Validate batch type consistency: cannot mix observed and inferred types.
		if err := validateBatchTypeConsistency(factInputs); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}

		// 3. Validate inputs, build paths, and serialize facts.
		facts, topicCategories, paths, files, err := validateAndBuildFacts(ontology, ontologyRoot, factInputs)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}

		// An explicit path is a NAMED SLOT: learn allocates it once, update
		// writes every revision after. Allowing a second learn would start a
		// second history for one slot, and a history walk over either half
		// silently under-reports.
		//
		// The same slot named TWICE IN ONE CALL is the other half of that rule,
		// and FactExists cannot see it — neither copy exists yet. The pending
		// writes are keyed by path, so the later input would overwrite the
		// earlier one while the response, built from len(facts), still reported
		// both as written: one file, two commit entries, one body silently
		// gone. Checked over every path, not just explicit ones: a collision is
		// the same silent loss whatever minted it.
		seen := make(map[string]int, len(paths))
		for i, fi := range factInputs {
			if first, dup := seen[paths[i]]; dup {
				return mcpgo.NewToolResultError(fmt.Sprintf(
					"fact %d: duplicate path %s, already written by fact %d in this call; one path is one fact per call",
					i, paths[i], first)), nil
			}
			seen[paths[i]] = i

			if fi.Path == "" {
				continue
			}
			exists, eerr := s.facts.FactExists(ctx, agentBranch, paths[i])
			if eerr != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("fact %d: exists check: %v", i, eerr)), nil
			}
			if exists {
				return mcpgo.NewToolResultError(fmt.Sprintf(
					"fact %d: %s already exists; use knomit_update to write a new revision", i, paths[i])), nil
			}
		}

		// 3b. Dedup check: fold each incoming fact into the near-duplicate it
		// matches in its own category directory, if any. Mutates facts and
		// files in place; hands back the embedding donations and the subsumed
		// hypotheses to retract alongside the write.
		embByPath, retract, priorRefs, err := applyDedupMerge(ctx, s, agentBranch, ontology, batchEmb, facts, topicCategories, paths, files)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}

		if err := computeEvidenceWeights(ctx, s, agentBranch, gate.LocalRepoID(), facts, paths, files); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}

		// upsert reads the donation map from ctx and skips its own ONNX call
		// when an entry is present for the path it is writing.
		ctx = store.WithPrecomputedEmbeddings(ctx, embByPath)

		// A local fact ref must resolve once this call lands, or nothing is
		// written. Checked BEFORE BatchWriteFacts so the call stays
		// all-or-nothing, and against the batch as well as the branch — one
		// call is one commit, so these facts may cite each other in any order.
		// An observation that subsumes a hypothesis cites what it retracts; that
		// target is still live at the pre-write head and stays reachable by
		// walk-back, so it satisfies the gate without a special case.
		//
		// priorRefs carries the refs a dedup-merged fact inherited from the fact
		// it merged into: those resolved at their own commit and this write does
		// not re-judge them.
		refsByPath := make(map[string][]string, len(facts))
		for i, f := range facts {
			refsByPath[paths[i]] = f.Refs
		}
		if err := gate.CheckBatch(ctx, refsByPath, priorRefs); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}

		// Qualify local refs AFTER the gate, so a rejection still echoes the ref
		// exactly as the caller sent it. Bare paths are accepted on input and
		// stored canonical, which is why an author never needs a repo id.
		for i, f := range facts {
			canon, changed := gate.Canonicalize(f.Refs)
			if !changed {
				continue
			}
			facts[i].Refs = canon
			if err := reserialize(files, paths[i], facts[i]); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
		}

		// 4. BatchWrite all facts — and any subsumed hypotheses' retractions —
		// in one commit, so a learn call is all-or-nothing.
		commitMsg := fmt.Sprintf("learn: %s", momentName)
		hash, _, err := s.facts.BatchWriteFacts(ctx, agentBranch, files, retract, commitMsg, "learn")
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("write error: %v", err)), nil
		}

		// 5. Build response.
		type commitEntry struct {
			File string `json:"file"`
			Hash string `json:"hash"`
		}
		commits := make([]commitEntry, len(facts))
		for i, f := range facts {
			commits[i] = commitEntry{File: f.Path(), Hash: hash}
		}

		dest := describeWriteDestination(b)
		result := map[string]interface{}{
			"commits":    commits,
			"written_to": dest,
			"summary":    dest.summary(pluralFacts(len(facts))),
		}
		out, err := json.Marshal(result)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal result: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}
