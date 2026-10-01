package manager

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/rytsh/krabby/internal/service/coderag"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/graphbuilder"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/repofs"
)

// pictureInsight is derived repository evidence: Krabby's own generated
// documentation, code-index communication signals and dependency-graph facts.
// They let a bounded research window cover whole repositories instead of the
// handful of raw files that fit in it.
type pictureInsight struct {
	locator   string
	text      string
	truncated bool
}

const (
	pictureDocsBytes    = 24 << 10
	pictureSignalBytes  = 12 << 10
	pictureSignalLines  = 120
	pictureSignalLine   = 200
	pictureGraphImports = 40
	pictureGraphHubs    = 15
)

// pictureSignalPattern finds where services talk to each other: messaging
// producers/consumers, RPC and HTTP clients/routes, and endpoint/address
// configuration. It is a broad, case-insensitive net; the model interprets it.
var pictureSignalPattern = `(kafka|topic|subscri|publish|produc|consum|rabbit|amqp|nats|sqs|sns|pubsub|eventbridge|grpc|protobuf|http\.(get|post|newrequest)|httpclient|resttemplate|webclient|feignclient|axios|fetch\(|@(get|post|put|delete|request)mapping|handlefunc|\.(get|post|put|delete|patch)\(\s*["'/]|_url\b|_host\b|_endpoint\b|https?://)`

var (
	pictureSensitiveLine = regexp.MustCompile(`(?i)(passw|secret|token|api[_-]?key|private[_-]?key|credential|authorization|bearer)`)
	pictureURLUserInfo   = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`)
)

// pictureRepoInsights collects derived evidence for one repository. Missing
// artifacts are reported in notes, never treated as an error: a repository
// whose docs or index have not been generated still contributes raw files.
func (m *Manager) pictureRepoInsights(ctx context.Context, repo *registry.Repo) ([]pictureInsight, []string) {
	var out []pictureInsight
	var notes []string
	prefix := "repo:" + repo.ID + ": "

	if docs, note := m.pictureRepoDocs(repo); len(docs) > 0 {
		out = append(out, docs...)
		if note != "" {
			notes = append(notes, prefix+note)
		}
	} else {
		notes = append(notes, prefix+note)
	}
	if signals, note := m.pictureRepoSignals(ctx, repo); signals != nil {
		out = append(out, *signals)
		notes = append(notes, prefix+note)
	} else if note != "" {
		notes = append(notes, prefix+note)
	}
	if graph, note := m.pictureRepoGraph(repo); graph != nil {
		out = append(out, *graph)
	} else if note != "" {
		notes = append(notes, prefix+note)
	}
	return out, notes
}

func (m *Manager) pictureRepoDocs(repo *registry.Repo) ([]pictureInsight, string) {
	if m.docsRootDir == "" {
		return nil, "generated repository documentation unavailable (docs disabled)."
	}
	dir, _, err := m.repoDocsPath(repo.ID)
	if err != nil {
		return nil, "generated repository documentation unavailable."
	}
	man, err := docgen.LoadManifest(dir)
	if err != nil || man == nil || len(man.Docs) == 0 {
		return nil, "no generated repository documentation yet; only raw files were used."
	}
	budget := pictureDocsBytes
	var out []pictureInsight
	for _, doc := range man.Docs {
		if budget <= 0 {
			break
		}
		content, err := repofs.ReadFile(dir, doc.Path, 0, budget)
		if err != nil {
			continue
		}
		out = append(out, pictureInsight{locator: "krabby-docs/" + path.Clean(doc.Path), text: content.Content, truncated: content.Truncated})
		budget -= content.Bytes
	}
	note := ""
	if stage := repo.Stages.Docs; stage.Commit != "" && repo.LastCommit != "" && stage.Commit != repo.LastCommit {
		note = "generated documentation predates the latest commit; it may be stale."
	}
	return out, note
}

func (m *Manager) pictureRepoSignals(ctx context.Context, repo *registry.Repo) (*pictureInsight, string) {
	if m.codeText == nil {
		return nil, "code index unavailable; communication signals not collected."
	}
	page, err := m.SearchCodeRegex(ctx, repo.ID, "", pictureSignalPattern, coderag.RegexOptions{MaxMatches: 6, PerPage: 100, MaxFiles: 400})
	if err != nil {
		return nil, "communication signal search failed; signals not collected."
	}
	var b strings.Builder
	lines, files, truncated := 0, 0, !page.Exhaustive || page.Total > uint64(len(page.Results))
	// Output is deliberately stable across unrelated commits: no line numbers,
	// files in path order and each file's distinct lines sorted. Otherwise any
	// edit above a call site would change the research fingerprint and force a
	// model run.
	slices.SortFunc(page.Results, func(a, b coderag.RegexHit) int { return strings.Compare(a.Path, b.Path) })
	for _, hit := range page.Results {
		if isPictureTestPath(hit.Path) || isPictureSecretPath(hit.Path) {
			continue
		}
		texts := []string{}
		for _, match := range hit.Matches {
			text := strings.TrimSpace(match.Text)
			if text == "" || pictureSensitiveLine.MatchString(text) {
				continue
			}
			text = pictureURLUserInfo.ReplaceAllString(text, "${1}[redacted]@")
			if len(text) > pictureSignalLine {
				text = strings.ToValidUTF8(text[:pictureSignalLine], "") + "…"
			}
			texts = append(texts, text)
		}
		slices.Sort(texts)
		texts = slices.Compact(texts)
		if len(texts) == 0 {
			continue
		}
		if lines >= pictureSignalLines || b.Len() >= pictureSignalBytes {
			truncated = true
			break
		}
		fmt.Fprintf(&b, "%s:\n", hit.Path)
		for _, text := range texts {
			fmt.Fprintf(&b, "  %s\n", text)
			lines++
		}
		files++
		if hit.Truncated {
			truncated = true
		}
	}
	if lines == 0 {
		return nil, "no communication signals matched in the code index."
	}
	header := "Communication signal lines (messaging, RPC/HTTP clients and routes, endpoint configuration) matched in the repository code index. Lines are excerpts, not complete call sites; credential-looking lines are omitted.\n"
	return &pictureInsight{locator: "krabby:communication-signals", text: header + b.String(), truncated: truncated},
		fmt.Sprintf("communication signals: %d lines from %d files (code index, tests excluded).", lines, files)
}

func (m *Manager) pictureRepoGraph(repo *registry.Repo) (*pictureInsight, string) {
	if m.engine == nil {
		return nil, ""
	}
	graphPath := graphbuilder.GraphPath(repo.Path)
	if !fileExists(graphPath) {
		return nil, "code graph not built; dependency summary not collected."
	}
	g, err := m.engine.Graph(graphPath)
	if err != nil {
		return nil, "code graph unreadable; dependency summary not collected."
	}
	var b strings.Builder
	// Names only, alphabetically: exact import counts and hub degrees shift
	// with almost every commit and would defeat the unchanged-evidence check.
	b.WriteString("Most-used external dependencies (code graph; network, database and third-party packages imported by non-test files):\n")
	imports := g.ExternalImports(pictureGraphImports)
	names := make([]string, 0, len(imports))
	for _, dep := range imports {
		names = append(names, dep.Label)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(&b, "- %s\n", name)
	}
	if len(names) == 0 {
		b.WriteString("- none detected\n")
	}
	b.WriteString("\nCentral code entities (most connected, alphabetical):\n")
	hubs := g.TopEntities(pictureGraphHubs)
	slices.Sort(hubs)
	for _, hub := range hubs {
		fmt.Fprintf(&b, "- %s\n", hub)
	}
	return &pictureInsight{locator: "krabby:dependency-graph", text: b.String()}, ""
}

// isPictureSecretPath excludes common secret/key files from every collection
// path. This is not a DLP guarantee: ordinary source/config files can also
// contain secrets.
func isPictureSecretPath(file string) bool {
	lower := strings.ToLower(path.Clean(strings.ReplaceAll(file, "\\", "/")))
	base := path.Base(lower)
	return strings.HasPrefix(base, ".env") || strings.Contains(lower, "secret") || strings.Contains(lower, "credential") || strings.HasSuffix(lower, ".pem") || strings.HasSuffix(lower, ".key")
}

func isPictureTestPath(file string) bool {
	lower := strings.ToLower(file)
	base := path.Base(lower)
	return strings.HasSuffix(base, "_test.go") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		strings.Contains(lower, "/test/") || strings.Contains(lower, "/tests/") || strings.HasPrefix(lower, "test/") || strings.HasPrefix(lower, "tests/") ||
		strings.Contains(lower, "testdata/") || strings.Contains(lower, "/mocks/")
}
