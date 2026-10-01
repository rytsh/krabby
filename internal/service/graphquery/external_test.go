package graphquery

import (
	"slices"
	"testing"
)

func TestExternalImportsAndTopEntities(t *testing.T) {
	file := writeGraph(t, `{"nodes":[
    {"id":"main.go","label":"main.go","source_file":"cmd/main.go"},
    {"id":"handler.go","label":"api/handler.go","source_file":"api/handler.go"},
    {"id":"svc","label":"OrderService","source_file":"orders/service.go"},
    {"id":"fake","label":"FakeBroker","source_file":"orders/orders_test.go"},
    {"id":"test.go","label":"orders_test.go","source_file":"orders/orders_test.go"},
    {"id":"kafka","label":"go_pkg_github_com_segmentio_kafka_go","source_file":""},
    {"id":"http","label":"go_pkg_net_http","source_file":""},
    {"id":"str","label":"go_pkg_strings","source_file":""},
    {"id":"testify","label":"go_pkg_github_com_stretchr_testify","source_file":""},
    {"id":"axios","label":"axios","source_file":""},
    {"id":"ref","label":"ref_node_assert","source_file":""}
  ],"links":[
    {"source":"main.go","target":"kafka","relation":"imports_from"},
    {"source":"handler.go","target":"kafka","relation":"imports_from"},
    {"source":"handler.go","target":"http","relation":"imports_from"},
    {"source":"main.go","target":"str","relation":"imports_from"},
    {"source":"test.go","target":"testify","relation":"imports_from"},
    {"source":"handler.go","target":"axios","relation":"imports"},
    {"source":"main.go","target":"ref","relation":"imports_from"},
    {"source":"main.go","target":"svc","relation":"calls"},
    {"source":"handler.go","target":"svc","relation":"calls"},
    {"source":"fake","target":"svc","relation":"references"},
    {"source":"fake","target":"handler.go","relation":"references"},
    {"source":"fake","target":"main.go","relation":"references"}
  ]}`)
	g, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	imports := g.ExternalImports(10)
	got := make([]string, 0, len(imports))
	for _, dep := range imports {
		got = append(got, dep.Label)
	}
	want := []string{"go:github_com_segmentio_kafka_go", "axios", "go:net_http"}
	if !slices.Equal(got, want) || imports[0].Files != 2 {
		t.Fatalf("imports = %+v, want %v", imports, want)
	}
	if top := g.TopEntities(5); !slices.Equal(top, []string{"OrderService"}) {
		t.Fatalf("top entities = %v", top)
	}
}
