package manager

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/worldline-go/types"

	"github.com/rytsh/krabby/internal/service/websource"
)

// TestUpdateWebCollectionKeepsUnmentionedFields is the regression test for a
// partial update that destroyed state it was never asked to touch: the envelope
// (description, schedule, interval) was replaced wholesale while the provider
// config merged per field, so changing one of them silently cleared the others
// — and with them the collection's automatic sync.
func TestUpdateWebCollectionKeepsUnmentionedFields(t *testing.T) {
	ctx := context.Background()

	m, webStore := newReconcileManager(t, &fakeReconcileFetcher{})

	col := &websource.Collection{
		Name:            "wiki",
		Type:            "fake",
		Description:     "team wiki",
		RefreshInterval: 6 * time.Hour,
		Specs:           []string{"0 2 * * *"},
		Status:          websource.StatusReady,
		Config:          json.RawMessage(`{"base_url":"https://wiki"}`),
		State:           json.RawMessage(`{"w":"1"}`),
		CreatedAt:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := webStore.UpsertCollection(ctx, col); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Change only the description.
	update := websource.CollectionUpdate{Description: types.NewNull("renamed")}
	if err := m.UpdateWebCollection(ctx, "wiki", update, nil); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := m.WebCollection(ctx, "wiki")
	if err != nil {
		t.Fatal(err)
	}

	if got.Description != "renamed" {
		t.Fatalf("description = %q", got.Description)
	}
	if !slices.Equal(got.Specs, []string{"0 2 * * *"}) {
		t.Fatalf("schedule was wiped: %#v", got.Specs)
	}
	if got.RefreshInterval != 6*time.Hour {
		t.Fatalf("refresh interval was wiped: %v", got.RefreshInterval)
	}

	// Everything the client cannot set must survive too.
	if got.Type != "fake" {
		t.Fatalf("type changed to %q", got.Type)
	}
	if got.Status != websource.StatusReady {
		t.Fatalf("status changed to %q", got.Status)
	}
	if string(got.State) != `{"w":"1"}` {
		t.Fatalf("sync watermark lost: %s", got.State)
	}
	if !got.CreatedAt.Equal(col.CreatedAt) {
		t.Fatalf("created_at changed to %v", got.CreatedAt)
	}

	// An explicit null clears, and only what it names.
	var clear websource.CollectionUpdate
	if err := json.Unmarshal([]byte(`{"specs":null}`), &clear); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateWebCollection(ctx, "wiki", clear, nil); err != nil {
		t.Fatalf("clear specs: %v", err)
	}

	if got, err = m.WebCollection(ctx, "wiki"); err != nil {
		t.Fatal(err)
	}
	if len(got.Specs) != 0 {
		t.Fatalf("explicit null did not clear the schedule: %#v", got.Specs)
	}
	if got.Description != "renamed" || got.RefreshInterval != 6*time.Hour {
		t.Fatalf("clearing the schedule touched other fields: %#v", got)
	}
}

// TestUpdateWebCollectionRejectsUnknownName keeps the update path from creating
// collections by accident.
func TestUpdateWebCollectionRejectsUnknownName(t *testing.T) {
	ctx := context.Background()
	m, _ := newReconcileManager(t, &fakeReconcileFetcher{})

	if err := m.UpdateWebCollection(ctx, "absent", websource.CollectionUpdate{}, nil); err == nil {
		t.Fatal("updating a collection that does not exist succeeded")
	}
}
