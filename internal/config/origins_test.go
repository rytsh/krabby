package config

import (
	"slices"
	"testing"
)

func TestNormalizeOrigins(t *testing.T) {
	got := NormalizeOrigins([]string{
		" https://a.example.com ", "", "https://a.example.com/", "https://*.example.org", "  ",
	})
	want := []string{"https://a.example.com", "https://*.example.org"}

	if !slices.Equal(got, want) {
		t.Fatalf("NormalizeOrigins() = %q, want %q", got, want)
	}

	if got := NormalizeOrigins(nil); got != nil {
		t.Fatalf("NormalizeOrigins(nil) = %q, want nil", got)
	}
}
