package config

import (
	"slices"
	"testing"
)

func TestFiltersMerge(t *testing.T) {
	global := Filters{
		Include:      []string{"*.go"},
		IncludeExtra: []string{"**/*.sql"},
		Exclude:      []string{"**/*.pb.go"},
	}

	t.Run("repo include replaces", func(t *testing.T) {
		got := global.Merge(Filters{Include: []string{"**/*.yaml"}})
		if !slices.Equal(got.Include, []string{"**/*.yaml"}) {
			t.Fatalf("Include = %v, want the repo's own list", got.Include)
		}
	})

	t.Run("empty repo include inherits", func(t *testing.T) {
		got := global.Merge(Filters{})
		if !slices.Equal(got.Include, []string{"*.go"}) {
			t.Fatalf("Include = %v, want the install-wide list", got.Include)
		}
	})

	t.Run("extras and excludes accumulate", func(t *testing.T) {
		got := global.Merge(Filters{
			IncludeExtra: []string{"**/*.yaml"},
			Exclude:      []string{"vendor/"},
		})
		if !slices.Equal(got.IncludeExtra, []string{"**/*.sql", "**/*.yaml"}) {
			t.Fatalf("IncludeExtra = %v, want both lists", got.IncludeExtra)
		}
		// An install-wide exclude is a policy, so a repository cannot drop it
		// by setting its own.
		if !slices.Equal(got.Exclude, []string{"**/*.pb.go", "vendor/"}) {
			t.Fatalf("Exclude = %v, want both lists", got.Exclude)
		}
	})

	// Merging must not write into the install-wide slices, which are shared by
	// every repository indexed in the same process.
	t.Run("does not alias the global lists", func(t *testing.T) {
		base := Filters{IncludeExtra: make([]string, 1, 4), Exclude: make([]string, 1, 4)}
		base.IncludeExtra[0] = "a"
		base.Exclude[0] = "x"

		_ = base.Merge(Filters{IncludeExtra: []string{"b"}, Exclude: []string{"y"}})

		if base.IncludeExtra[0] != "a" || len(base.IncludeExtra) != 1 {
			t.Fatalf("global IncludeExtra mutated: %v", base.IncludeExtra)
		}
		if base.Exclude[0] != "x" || len(base.Exclude) != 1 {
			t.Fatalf("global Exclude mutated: %v", base.Exclude)
		}
	})
}
