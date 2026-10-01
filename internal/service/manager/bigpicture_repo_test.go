package manager

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/docgen"
	"github.com/rytsh/krabby/internal/service/graphbuilder"
	"github.com/rytsh/krabby/internal/service/graphquery"
	"github.com/rytsh/krabby/internal/service/registry"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

const pictureGraph = `{
  "directed": false, "multigraph": false, "nodes": [
    {"id":"main.go","label":"main.go","source_file":"cmd/main.go","source_location":"L1","file_type":"code"},
    {"id":"orders.Publish","label":"Publish()","source_file":"orders/publish.go","source_location":"L4","file_type":"code"},
    {"id":"orders.Service","label":"OrderService","source_file":"orders/service.go","source_location":"L9","file_type":"code"},
    {"id":"orders_test.go","label":"orders_test.go","source_file":"orders/orders_test.go","source_location":"L1","file_type":"code"},
    {"id":"go_pkg_github_com_segmentio_kafka_go","label":"go_pkg_github_com_segmentio_kafka_go","source_file":"","file_type":"code"},
    {"id":"go_pkg_net_http","label":"go_pkg_net_http","source_file":"","file_type":"code"},
    {"id":"go_pkg_strings","label":"go_pkg_strings","source_file":"","file_type":"code"},
    {"id":"go_pkg_github_com_stretchr_testify","label":"go_pkg_github_com_stretchr_testify","source_file":"","file_type":"code"}
  ], "links": [
    {"source":"orders.Publish","target":"go_pkg_github_com_segmentio_kafka_go","relation":"imports_from"},
    {"source":"main.go","target":"go_pkg_net_http","relation":"imports_from"},
    {"source":"main.go","target":"go_pkg_strings","relation":"imports_from"},
    {"source":"orders_test.go","target":"go_pkg_github_com_stretchr_testify","relation":"imports_from"},
    {"source":"main.go","target":"orders.Service","relation":"calls"},
    {"source":"orders.Publish","target":"orders.Service","relation":"references"}
  ]
}`

func TestPictureRepoInsightsUseDocsSignalsAndGraph(t *testing.T) {
	m, ctx := codeSearchFixture(t, "acme/orders", []vectorstore.Item{
		codeChunk("acme/orders", "orders/publish.go", "Publish", 1, "func Publish() {\n\twriter := kafka.NewWriter(kafka.WriterConfig{Topic: \"order-created\"})\n\tpassword := cfg.KafkaPassword // topic auth\n}"),
		codeChunk("acme/orders", "orders/client.go", "Client", 1, "const paymentURL = \"https://svc:hunter2@payments.internal/charge\"\nresp, err := http.Post(paymentURL, body)"),
		codeChunk("acme/orders", "orders/orders_test.go", "Test", 1, "kafka.NewWriter(testTopic)"),
		codeChunk("acme/orders", "config/secrets.yaml", "", 1, "kafka_topic: hidden"),
	})
	repoPath := filepath.Join(t.TempDir(), "repo")
	mustWriteManagerTest(t, graphbuilder.GraphPath(repoPath), pictureGraph)
	m.engine = graphquery.NewEngine(0)
	m.docsRootDir = t.TempDir()
	docsDir := filepath.Join(m.docsRootDir, "acme", "orders")
	mustWriteManagerTest(t, filepath.Join(docsDir, "documentation.md"), "# Orders\nOrders publishes order-created events.")
	if err := os.WriteFile(filepath.Join(docsDir, docgen.ManifestName), []byte(`{"repo":"acme/orders","docs":[{"path":"documentation.md","title":"Orders"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := &registry.Repo{ID: "acme/orders", Path: repoPath, LastCommit: "c2", Stages: registry.Stages{Docs: registry.StageState{Commit: "c1"}}}

	insights, notes := m.pictureRepoInsights(ctx, repo)
	byLocator := map[string]string{}
	for _, insight := range insights {
		byLocator[insight.locator] = insight.text
	}
	if !strings.Contains(byLocator["krabby-docs/documentation.md"], "order-created") {
		t.Fatalf("generated docs missing: %v", byLocator)
	}
	signals := byLocator["krabby:communication-signals"]
	if !strings.Contains(signals, "orders/publish.go:") || !strings.Contains(signals, `Topic: "order-created"`) || !strings.Contains(signals, "http.Post(paymentURL") {
		t.Fatalf("communication signals missing:\n%s", signals)
	}
	for _, leaked := range []string{"password", "hunter2", "orders_test.go", "secrets.yaml"} {
		if strings.Contains(signals, leaked) {
			t.Fatalf("signals leaked %q:\n%s", leaked, signals)
		}
	}
	graph := byLocator["krabby:dependency-graph"]
	if !strings.Contains(graph, "go:github_com_segmentio_kafka_go") || !strings.Contains(graph, "go:net_http") || !strings.Contains(graph, "OrderService") {
		t.Fatalf("dependency graph summary missing:\n%s", graph)
	}
	if strings.Contains(graph, "strings") || strings.Contains(graph, "testify") {
		t.Fatalf("graph summary included stdlib/test-only imports:\n%s", graph)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "predates the latest commit") {
		t.Fatalf("stale docs not noted: %v", notes)
	}

	// Moving a call site without changing it keeps the evidence identical, so
	// the research fingerprint does not force a model run.
	if err := m.codeText.ReplaceRepo(ctx, "acme/orders", []vectorstore.Item{
		codeChunk("acme/orders", "orders/client.go", "Client", 40, "// moved\n\nresp, err := http.Post(paymentURL, body)\nconst paymentURL = \"https://svc:hunter2@payments.internal/charge\""),
		codeChunk("acme/orders", "orders/publish.go", "Publish", 90, "func Publish() {\n\n\n\twriter := kafka.NewWriter(kafka.WriterConfig{Topic: \"order-created\"})\n}"),
	}); err != nil {
		t.Fatal(err)
	}
	moved, _ := m.pictureRepoSignals(ctx, repo)
	if moved == nil || moved.text != signals {
		t.Fatalf("signals changed on a move:\n%s\n---\n%v", signals, moved)
	}

	// Collection puts derived evidence first and still adds raw files.
	mustWriteManagerTest(t, filepath.Join(repoPath, "deploy", "orders.yaml"), "replicas: 2")
	if err := m.reg.Upsert(ctx, &registry.Repo{ID: "acme/orders", URL: "https://example.com/acme/orders", Path: repoPath, LastCommit: "c2"}); err != nil {
		t.Fatal(err)
	}
	research, err := m.collectPictureResearch(ctx, &bigpicture.Picture{Name: "commerce", Sources: []bigpicture.Source{{Kind: "repo", Ref: "acme/orders"}}})
	if err != nil {
		t.Fatal(err)
	}
	var locators []string
	for _, item := range research.Items {
		locators = append(locators, item.Evidence.Locator)
	}
	if len(locators) < 4 || locators[0] != "krabby-docs/documentation.md" || !slices.Contains(locators, "krabby:communication-signals") || !slices.Contains(locators, "deploy/orders.yaml") {
		t.Fatalf("collected locators = %v", locators)
	}
}

func TestPictureRepoInsightsDegradeWithoutArtifacts(t *testing.T) {
	m := &Manager{}
	repo := &registry.Repo{ID: "acme/bare", Path: t.TempDir()}
	insights, notes := m.pictureRepoInsights(context.Background(), repo)
	if len(insights) != 0 {
		t.Fatalf("unexpected insights: %+v", insights)
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"documentation unavailable", "code index unavailable"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing note %q in %v", want, notes)
		}
	}
}

func TestPictureSecretPathsExcluded(t *testing.T) {
	if pictureFileRank("deploy/secrets.yaml", "repo") != 0 || !isPictureSecretPath("A/Credentials.json") || isPictureSecretPath("orders/publish.go") {
		t.Fatal("secret path filtering regressed")
	}
}
