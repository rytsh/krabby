package searchutil

import "testing"

// A query that read the stored set before a refresh published a newer one
// must not write its older read over it.
func TestStatsCacheFillLosesToAPublish(t *testing.T) {
	var c StatsCache

	_, ok, gen := c.Get()
	if ok {
		t.Fatal("an empty cache reported a loaded set")
	}

	fresh := NewStopWords([]string{"fresh"})
	c.Publish(fresh)
	c.Fill(gen, NewStopWords([]string{"stale"}))

	terms, ok, _ := c.Get()
	if !ok || !terms["fresh"] || terms["stale"] {
		t.Fatalf("cache = %v (loaded %v), want the published set", terms, ok)
	}

	c.Invalidate()
	_, ok, gen = c.Get()
	if ok {
		t.Fatal("an invalidated cache reported a loaded set")
	}
	c.Fill(gen, NewStopWords([]string{"read"}))
	if terms, ok, _ := c.Get(); !ok || !terms["read"] {
		t.Fatalf("a fill with no publish in between was dropped: %v", terms)
	}
}
