package manager

import (
	"context"
	"fmt"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/graphify"
	"github.com/rytsh/krabby/internal/service/registry"
)

// RepoSettings is what one repository is actually built with, and where each
// value came from.
//
// It exists because the answer is not readable from either half alone: the
// install-wide settings and the repository's overrides combine by rules that
// differ per field (Include replaces, IncludeExtra and Exclude add), so a UI
// that showed the two side by side would be asking the user to run the merge in
// their head. Effective is the merge the pipeline will actually perform.
type RepoSettings struct {
	Repo string `json:"repo"`

	// Global is the install-wide configuration these overrides apply to.
	Global RepoSettingsGlobal `json:"global"`
	// Overrides is what this repository sets, verbatim.
	Overrides registry.Overrides `json:"overrides"`
	// Effective is the result the next build will use.
	Effective RepoSettingsEffective `json:"effective"`
}

// RepoSettingsGlobal is the install-wide half, for showing what an override
// departs from.
type RepoSettingsGlobal struct {
	CodeInclude      []string `json:"code_include,omitempty"`
	CodeIncludeExtra []string `json:"code_include_extra,omitempty"`
	CodeExclude      []string `json:"code_exclude,omitempty"`

	DocsInclude      []string `json:"docs_include,omitempty"`
	DocsIncludeExtra []string `json:"docs_include_extra,omitempty"`
	DocsExclude      []string `json:"docs_exclude,omitempty"`

	GraphExclude []string `json:"graph_exclude,omitempty"`

	// DocsPromptSet reports whether an install-wide prompt replaces the
	// built-in default. The prompt bodies themselves are not repeated here;
	// they are available from the settings endpoint.
	DocsPromptSet      bool `json:"docs_prompt_set"`
	DocsPromptExtraSet bool `json:"docs_prompt_extra_set"`

	// Install-wide documentation input budgets, already resolved to the
	// built-in defaults where unset, so the UI can show a number rather than a
	// zero meaning "something else decides".
	DocsLimits config.DocsLimits `json:"docs_limits"`
}

// RepoSettingsEffective is the merged result, plus a label naming which level
// decided the documentation prompt so the UI never has to guess.
type RepoSettingsEffective struct {
	CodeInclude      []string `json:"code_include,omitempty"`
	CodeIncludeExtra []string `json:"code_include_extra,omitempty"`
	CodeExclude      []string `json:"code_exclude,omitempty"`

	DocsInclude      []string `json:"docs_include,omitempty"`
	DocsIncludeExtra []string `json:"docs_include_extra,omitempty"`
	DocsExclude      []string `json:"docs_exclude,omitempty"`

	GraphExclude []string `json:"graph_exclude,omitempty"`

	// CodeIncludeIsDefault reports that no Include is set at either level, so
	// the built-in allowlist (source extensions, build manifests, deploy
	// config) decides. It is the common case and worth saying out loud rather
	// than rendering an empty list.
	CodeIncludeIsDefault bool `json:"code_include_is_default"`
	DocsIncludeIsDefault bool `json:"docs_include_is_default"`

	// DocsPromptSource is "default", "global" or "repo".
	DocsPromptSource string `json:"docs_prompt_source"`
	// DocsPromptExtras names the levels contributing appended instructions, in
	// the order they are appended.
	DocsPromptExtras []string `json:"docs_prompt_extras,omitempty"`

	// DocsLimits is the budget the next docs build will actually apply.
	DocsLimits config.DocsLimits `json:"docs_limits"`

	// SkipStages are the pipeline stages this repository will not run.
	SkipStages []string `json:"skip_stages,omitempty"`
}

// RepoSettings resolves the effective build configuration of one repository.
func (m *Manager) RepoSettings(ctx context.Context, ref string) (*RepoSettings, error) {
	if m.settings == nil {
		return nil, ErrNoSettingsStore
	}

	repo, err := m.reg.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, fmt.Errorf("repo %s not found", ref)
	}

	s, err := m.settings.Get(ctx)
	if err != nil {
		return nil, err
	}

	code := codeRagConfig(s).Filters
	docs := docsConfig(s)

	var graphGlobal []string
	if m.gfy != nil {
		graphGlobal = m.gfy.Exclude()
	}

	over := repo.Overrides
	repoFilters := repoFilters(repo)

	effCode := code.Merge(repoFilters)
	effDocs := docs.Filters.Merge(repoFilters)

	out := &RepoSettings{
		Repo:      repo.ID,
		Overrides: over,
		Global: RepoSettingsGlobal{
			CodeInclude:        code.Include,
			CodeIncludeExtra:   code.IncludeExtra,
			CodeExclude:        code.Exclude,
			DocsInclude:        docs.Include,
			DocsIncludeExtra:   docs.IncludeExtra,
			DocsExclude:        docs.Exclude,
			GraphExclude:       graphGlobal,
			DocsPromptSet:      docs.Prompt != "",
			DocsPromptExtraSet: docs.PromptExtra != "",
			DocsLimits:         docs.Limits.Resolve(),
		},
		Effective: RepoSettingsEffective{
			CodeInclude:          effCode.Include,
			CodeIncludeExtra:     effCode.IncludeExtra,
			CodeExclude:          effCode.Exclude,
			DocsInclude:          effDocs.Include,
			DocsIncludeExtra:     effDocs.IncludeExtra,
			DocsExclude:          effDocs.Exclude,
			GraphExclude:         graphify.MergeExclude(graphGlobal, over.GraphExclude),
			CodeIncludeIsDefault: len(effCode.Include) == 0,
			DocsIncludeIsDefault: len(effDocs.Include) == 0,
			DocsPromptSource:     promptSource(docs.Prompt, over.DocsPrompt),
			DocsPromptExtras:     promptExtras(docs.PromptExtra, over.DocsPromptExtra),
			DocsLimits:           docs.Limits.Merge(repoDocsOverride(repo).Limits).Resolve(),
			SkipStages:           over.SkipStages,
		},
	}

	return out, nil
}

// promptSource names the level whose prompt replaces the others; the most
// specific one wins, mirroring docgen's own resolution.
func promptSource(global, repo string) string {
	switch {
	case repo != "":
		return "repo"
	case global != "":
		return "global"
	default:
		return "default"
	}
}

// promptExtras lists the levels appending instructions, in append order.
func promptExtras(global, repo string) []string {
	var out []string
	if global != "" {
		out = append(out, "global")
	}
	if repo != "" {
		out = append(out, "repo")
	}

	return out
}

// DefaultDocsPrompt is the built-in synthesis prompt, exposed so the UI can
// show what "default" means.
func DefaultDocsPrompt() string { return docgen.DefaultPrompt }
