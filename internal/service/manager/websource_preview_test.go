package manager

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rakunlabs/bw"

	"github.com/rytsh/krabby/internal/service/websource"
)

type testPreviewFetcher struct {
	token string
}

func (f *testPreviewFetcher) Validate(json.RawMessage) error { return nil }

func (f *testPreviewFetcher) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	var cur, next map[string]string
	_ = json.Unmarshal(current, &cur)
	_ = json.Unmarshal(update, &next)
	if next["api_token"] == "" {
		next["api_token"] = cur["api_token"]
	}

	return json.Marshal(next)
}

func (f *testPreviewFetcher) ConfigView(json.RawMessage) any { return nil }

func (f *testPreviewFetcher) Fetch(context.Context, *websource.Collection, []*websource.Page, json.RawMessage, websource.Emit) (*websource.FetchResult, error) {
	return nil, nil
}

func (f *testPreviewFetcher) Preview(_ context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	var cfg map[string]string
	_ = json.Unmarshal(raw, &cfg)
	f.token = cfg["api_token"]

	return websource.PreviewResult{ItemCount: 7, Scanned: 9}, nil
}

func (f *testPreviewFetcher) FullResyncSpec(json.RawMessage) (string, error) {
	return "0 2 * * *", nil
}

func TestWebSourcePreviewUsesStoredSecretWithoutPersisting(t *testing.T) {
	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store, err := websource.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCollection(context.Background(), &websource.Collection{
		Name: "tickets", Type: "fake", Config: json.RawMessage(`{"api_token":"stored","query":"old"}`),
	}); err != nil {
		t.Fatal(err)
	}

	fetcher := &testPreviewFetcher{}
	m := &Manager{webStore: store, webFetchers: map[string]websource.Fetcher{"fake": fetcher}}
	got := m.TestWebSource(context.Background(), "fake", "tickets", json.RawMessage(`{"api_token":"","query":"new"}`))
	if !got.OK || got.ItemCount != 7 || fetcher.token != "stored" {
		t.Fatalf("TestWebSource() = %+v, token = %q", got, fetcher.token)
	}

	col, err := store.GetCollection(context.Background(), "tickets")
	if err != nil {
		t.Fatal(err)
	}
	if string(col.Config) != `{"api_token":"stored","query":"old"}` {
		t.Fatalf("preview persisted config: %s", col.Config)
	}
}

func TestWebSourceSchedulesIncludeFullResyncCron(t *testing.T) {
	db, err := bw.Open("", bw.WithInMemory(true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store, err := websource.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCollection(context.Background(), &websource.Collection{
		Name: "tickets", Type: "fake", Specs: []string{"*/15 * * * *"}, Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	m := &Manager{webStore: store, webFetchers: map[string]websource.Fetcher{"fake": &testPreviewFetcher{}}}
	got := m.WebSourceSchedules(context.Background())
	if len(got) != 1 || len(got[0].Specs) != 2 || got[0].Specs[1] != "0 2 * * *" {
		t.Fatalf("WebSourceSchedules() = %#v", got)
	}
}
