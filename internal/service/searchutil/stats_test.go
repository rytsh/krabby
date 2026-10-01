package searchutil

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
)

func TestSamplerCountsDocumentsNotOccurrences(t *testing.T) {
	sampler := NewFrequentTermSampler(100)
	for i := range 100 {
		body := fmt.Sprintf("common unique%03d", i)
		if i < 20 {
			body += " rare rare rare rare rare"
		}
		sampler.ObserveSample(body, "COMMON")
	}
	terms, sampled := sampler.Result()
	if sampled != 100 || !reflect.DeepEqual(terms, []string{"common"}) {
		t.Fatalf("Result = %v, %d", terms, sampled)
	}
}

func TestSamplerStrideMatchesPreselectedRecords(t *testing.T) {
	const total = 6000
	walk := NewFrequentTermSampler(total)
	selected := NewFrequentTermSampler(total)
	if walk.Stride() != 3 {
		t.Fatalf("Stride = %d, want 3", walk.Stride())
	}
	for i := range total {
		body := fmt.Sprintf("common group%d", i%7)
		walk.Observe(body)
		if i%selected.Stride() == 0 {
			selected.ObserveSample(body)
		}
	}
	a, n := walk.Result()
	b, m := selected.Result()
	if n != statsSampleSize || m != n || !reflect.DeepEqual(a, b) {
		t.Fatalf("walk = %v/%d, selected = %v/%d", a, n, b, m)
	}
}

func TestSamplerEmptyAndCap(t *testing.T) {
	sampler := NewFrequentTermSampler(0)
	if terms, count := sampler.Result(); terms != nil || count != 0 || sampler.Stride() != 1 {
		t.Fatalf("empty sampler = %v/%d, stride %d", terms, count, sampler.Stride())
	}
	var fields []string
	for i := range maxFrequentTerms + 50 {
		fields = append(fields, fmt.Sprintf("term%04d", i))
	}
	sampler.ObserveSample(fields...)
	terms, count := sampler.Result()
	if count != 1 || len(terms) != maxFrequentTerms || !slices.IsSorted(terms) {
		t.Fatalf("capped sampler = %d terms/%d samples", len(terms), count)
	}
}

func TestStatsFreshBoundaries(t *testing.T) {
	for _, tc := range []struct {
		previous, current int
		want              bool
	}{
		{0, 100, false}, {100, 100, true},
		{100, 124, true}, {100, 125, false},
		{100, 76, true}, {100, 75, false}, {100, 0, false},
	} {
		if got := StatsFresh(tc.previous, tc.current); got != tc.want {
			t.Errorf("StatsFresh(%d, %d) = %t, want %t", tc.previous, tc.current, got, tc.want)
		}
	}
}
