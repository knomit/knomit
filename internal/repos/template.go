package repos

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// F24: create a repo from a template held by another MOUNTED repo.
//
// The source is named by repo NAME (what every route, lens and screen uses);
// the provenance trailer records its ID. The template is read at the tip of
// the source's CONSENSUS branch (UpstreamTip(UpstreamBranch()) — the followed
// branch of a subscription), the same commit skills and recipes are read
// from, never its agent branch or an experiment. Trust is the chain of trust:
// the operator chose to mount that repo; nothing checks a key.

// TemplateRef names a template: the mounted repo that holds it, and its name.
type TemplateRef struct {
	Repo string
	Name string
}

// Template refusals. Each names what is wrong; none leaves anything behind
// (the create resolves the template before any repo file exists, and Create's
// cleanup removes the registry row and the .db on any later failure).
var (
	// ErrTemplateSourceNotFound: template.repo is not an ACTIVE repo on this
	// instance (unknown, archived, or a lens name).
	ErrTemplateSourceNotFound = errors.New("template source repo is not mounted on this instance")
	// ErrTemplateSourceUnavailable: the source repo is mounted but its store
	// is not open (populating, closing, failed to open).
	ErrTemplateSourceUnavailable = errors.New("template source repo is not open")
	// ErrTemplateName: the template name is not kebab-case.
	ErrTemplateName = errors.New("invalid template name: lowercase kebab-case only")
	// ErrTemplateNotFound: no such template folder at the source's consensus
	// tip (or no consensus branch yet), or no folder behind a listed fact.
	ErrTemplateNotFound = store.ErrTemplateNotFound
	// ErrTemplateNotRegular, ErrTemplateTooLarge, ErrTemplateLayout: see the
	// store sentinels.
	ErrTemplateNotRegular = store.ErrTemplateNotRegular
	ErrTemplateTooLarge   = store.ErrTemplateTooLarge
	ErrTemplateLayout     = store.ErrTemplateLayout
	// ErrTemplateDescribedTwice: two `part: template` facts describe the same
	// template name. Neither is listed, and the name is not creatable until
	// one is removed: which one is right is not knomit's call.
	ErrTemplateDescribedTwice = errors.New("two template facts describe the same template")
	// ErrTemplateNoOntology: the template carries no .knomit/ontology.yaml.
	ErrTemplateNoOntology = errors.New("template has no .knomit/ontology.yaml")
	// ErrTemplateOntology: its ontology does not parse as a NEW ontology.
	ErrTemplateOntology = errors.New("template ontology is invalid")
	// ErrTemplatePresetID: its ontology uses an embedded preset's id but is
	// not that preset (the boot refresh would treat it as the preset).
	ErrTemplatePresetID = errors.New("template ontology uses a built-in preset's id but is not that preset")
	// ErrTemplateFleetLocal: a fleet-id template may not become a LOCAL repo
	// (it would become this instance's fleet, and `knomit fleet register`
	// would then answer already_registered); use mode "initialize" on the
	// fleet's git URL. Every fleet-template refusal matches it, including
	// ErrTemplateFleetPresent.
	ErrTemplateFleetLocal = errors.New("a fleet template creates the fleet repository only with mode \"initialize\" on the fleet's git URL, and only when this instance has no fleet")
	// ErrTemplateFleetPresent: the request IS an initialize, but this
	// instance already has a fleet repository, or a fleet registration or
	// unregistration is in flight. It wraps ErrTemplateFleetLocal (so
	// errors.Is(err, ErrTemplateFleetLocal) still holds) and exists so the
	// answer does not tell an initialize to use initialize.
	ErrTemplateFleetPresent = fmt.Errorf("%w: this instance already has a fleet", ErrTemplateFleetLocal)
	// ErrTemplateFleetStateUnavailable: the fleet state could not be READ,
	// so whether a registration is in flight is unknown. The create is
	// refused (fail closed); it is not a verdict on the template.
	ErrTemplateFleetStateUnavailable = errors.New("fleet state unavailable: cannot tell whether a fleet registration is in flight")
)

// templatePresetByID is fact.EmbeddedPresetByID, as a variable only so a test
// can stand in a preset WITH root attributes: no embedded preset declares one
// today, and without one refreshDivergence and SubsetDivergence give the same
// answer for every template, so nothing else could pin which one is called.
var templatePresetByID = fact.EmbeddedPresetByID

// ResolvedTemplate is a template read and checked, ready to write.
type ResolvedTemplate struct {
	Ref      TemplateRef
	SourceID string        // the source repo's ID (root commit), for the trailer
	Commit   plumbing.Hash // the source's consensus tip it was read at
	Files    map[string]string
	Ontology *fact.Ontology
}

// Trailers is the provenance stamped on the commit that writes the template.
func (rt *ResolvedTemplate) Trailers() store.Trailers {
	return store.Trailers{
		Template:       rt.Ref.Name,
		TemplateSource: "kb://" + rt.SourceID + "@" + rt.Commit.String(),
	}
}

// templateSource resolves ref.Repo to an open source and runs fn on its store
// with the consensus tip. The store is held through Acquire for the whole
// read, never kept: a rename or archive of the source while a create reads it
// either waits for the read or makes it fail with a named error.
func (m *Manager) templateSource(ctx context.Context, ref TemplateRef, fn func(svc *store.Service, id string, tip plumbing.Hash) error) error {
	ri := m.Get(ref.Repo)
	if ri == nil {
		return fmt.Errorf("%w: %q", ErrTemplateSourceNotFound, ref.Repo)
	}
	id := ri.ID()
	svc, release, err := ri.Acquire()
	if err != nil {
		return fmt.Errorf("%w: %q: %v", ErrTemplateSourceUnavailable, ref.Repo, err)
	}
	defer release()
	if id == "" {
		return fmt.Errorf("%w: %q has no identity yet", ErrTemplateSourceUnavailable, ref.Repo)
	}
	branch := svc.UpstreamBranch()
	tip, err := svc.Triggers().UpstreamTip(ctx, branch)
	if err != nil {
		return fmt.Errorf("template source %q: read %s: %w", ref.Repo, branch, err)
	}
	return fn(svc, id, tip)
}

// ResolveTemplate reads template ref at its source's consensus tip and checks
// it: name, folder, plain files only, layout, limits, an ontology that parses
// as NEW, not a preset id unless it IS that preset, and at most one describing
// fact. It writes nothing. The fleet rule depends on the mode and is checked
// by the caller (checkTemplateMode).
func (m *Manager) ResolveTemplate(ctx context.Context, ref TemplateRef) (*ResolvedTemplate, error) {
	if !fact.ValidTemplateName(ref.Name) {
		return nil, fmt.Errorf("%w: %q", ErrTemplateName, ref.Name)
	}
	rt := &ResolvedTemplate{Ref: ref}
	err := m.templateSource(ctx, ref, func(svc *store.Service, id string, tip plumbing.Hash) error {
		if tip.IsZero() {
			return fmt.Errorf("%w: %q in %q (the repo has no consensus branch yet)", ErrTemplateNotFound, ref.Name, ref.Repo)
		}
		files, err := svc.Templates().TemplateFilesAt(ctx, tip, ref.Name)
		if err != nil {
			return fmt.Errorf("template %s/%s: %w", ref.Repo, ref.Name, err)
		}
		facts, err := m.templateFactsAt(ctx, svc, tip)
		if err != nil {
			return err
		}
		if d, ok := facts[ref.Name]; ok && d.twice {
			return fmt.Errorf("%w: %q in %q: %v", ErrTemplateDescribedTwice, ref.Name, ref.Repo, d.paths)
		}
		rt.SourceID, rt.Commit = id, tip
		rt.Files = make(map[string]string, len(files))
		for _, f := range files {
			rt.Files[f.Path] = string(f.Data)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	y, ok := rt.Files[OntologyPath]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrTemplateNoOntology, ref.Repo, ref.Name)
	}
	o, err := fact.ParseNewOntology([]byte(y))
	if err != nil {
		return nil, fmt.Errorf("%w: %s/%s: %v", ErrTemplateOntology, ref.Repo, ref.Name, err)
	}
	// "Is that preset" is exactly what the boot refresh asks (stages.go
	// refreshDivergence): a template this accepts with a preset's id is one
	// the refresh upgrades in place, never one it would rewrite into a
	// different taxonomy.
	if preset := templatePresetByID(o.ID); preset != nil {
		if d := refreshDivergence(o, preset); d != "" {
			return nil, fmt.Errorf("%w: id %q (%s)", ErrTemplatePresetID, o.ID, d)
		}
	}
	rt.Ontology = o
	return rt, nil
}

// checkTemplateMode applies the rules that depend on the create mode: a
// fleet-id template only with "initialize" (ErrTemplateFleetLocal), and only
// while this instance has no fleet repository and no fleet registration in
// flight (ErrTemplateFleetPresent). A fleet state that cannot be read
// refuses too (ErrTemplateFleetStateUnavailable): fail closed.
func (m *Manager) checkTemplateMode(rt *ResolvedTemplate, mode string) error {
	if !fact.IsFleetOntology(rt.Ontology) {
		return nil
	}
	if mode != "initialize" {
		return ErrTemplateFleetLocal
	}
	if ri := m.fleetRepo(); ri != nil {
		return fmt.Errorf("%w: its fleet repository is %q", ErrTemplateFleetPresent, ri.Name())
	}
	// Any read failure refuses, a missing identity row included: app.New and
	// Manager.Start both write that row before a create can run, so a row
	// that is not there was lost, and "nothing in flight" cannot be known.
	row, err := m.fleetDB()
	switch {
	case err != nil:
		return fmt.Errorf("%w: %v", ErrTemplateFleetStateUnavailable, err)
	case row.State == FleetRegistering || row.State == FleetUnregistering:
		return fmt.Errorf("%w: a fleet registration is %s", ErrTemplateFleetPresent, row.State)
	}
	return nil
}

// listingSkipLevel is the level the all-repos listing logs a skipped repo at.
// A repo that is not open (populating, closing) or was renamed away since
// Names() is an ordinary state, not a fault, and the listing runs on every
// GET /templates: Debug. Anything else failed on an open repo: Warn.
func listingSkipLevel(err error) zerolog.Level {
	if errors.Is(err, ErrTemplateSourceUnavailable) || errors.Is(err, ErrTemplateSourceNotFound) {
		return zerolog.DebugLevel
	}
	return zerolog.WarnLevel
}

// TemplateInfo is one listed template.
type TemplateInfo struct {
	Repo        string `json:"repo"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Commit      string `json:"commit"`
	Fact        string `json:"fact"`
}

type describedTemplate struct {
	fact  fact.TemplateFact
	twice bool
	paths []string
}

// templateFactsAt reads the `part: template` facts at tip, by template name.
// A malformed one is logged and skipped, as a malformed skill is.
func (m *Manager) templateFactsAt(ctx context.Context, svc *store.Service, tip plumbing.Hash) (map[string]*describedTemplate, error) {
	files, err := svc.Templates().TemplateFactsAt(ctx, tip, m.deps.Cfg.OntologyRoot)
	if err != nil {
		return nil, err
	}
	out := map[string]*describedTemplate{}
	for _, f := range files {
		tf, ok, perr := fact.ParseTemplateFact(f.Path, f.Folder, string(f.Data))
		if perr != nil {
			log.Warn().Err(perr).Str("fact", f.Path).Msg("templates: malformed template fact skipped")
			continue
		}
		if !ok {
			continue
		}
		if d, seen := out[tf.Name]; seen {
			d.twice = true
			d.paths = append(d.paths, tf.Path)
			continue
		}
		out[tf.Name] = &describedTemplate{fact: tf, paths: []string{tf.Path}}
	}
	return out, nil
}

// ListTemplates lists the templates of one mounted repo (repo != "") or of
// every mounted, open repo: each `part: template` fact at the repo's
// consensus tip whose folder exists at that same commit. A folder without a
// fact is not listed (it is still creatable by name); a fact without a folder
// is not listed; a name with two facts is not listed (and is refused at
// create). A repo that is not open is skipped in the all-repos listing (at
// Debug: it is a state, not a fault) and is an error
// (ErrTemplateSourceUnavailable) for the one-repo listing.
func (m *Manager) ListTemplates(ctx context.Context, repo string) ([]TemplateInfo, error) {
	names := m.Names()
	if repo != "" {
		names = []string{repo}
	}
	out := []TemplateInfo{}
	for _, name := range names {
		err := m.templateSource(ctx, TemplateRef{Repo: name}, func(svc *store.Service, _ string, tip plumbing.Hash) error {
			if tip.IsZero() {
				return nil
			}
			facts, err := m.templateFactsAt(ctx, svc, tip)
			if err != nil {
				return err
			}
			for tname, d := range facts {
				if d.twice {
					log.Warn().Str("repo", name).Str("template", tname).Strs("facts", d.paths).
						Msg("templates: two template facts describe one template; neither is listed")
					continue
				}
				ok, err := svc.Templates().TemplateExistsAt(ctx, tip, tname)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				out = append(out, TemplateInfo{Repo: name, Name: tname, Description: d.fact.Title, Commit: tip.String(), Fact: d.fact.Path})
			}
			return nil
		})
		if err != nil {
			if repo != "" {
				return nil, err
			}
			log.WithLevel(listingSkipLevel(err)).Err(err).Str("repo", name).Msg("templates: repo skipped in the listing")
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
