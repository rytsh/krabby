package strutil

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateShortStringIsUnchanged(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "abc", "exactly-10"} {
		if got := Truncate(s, 10); got != s {
			t.Errorf("Truncate(%q, 10) = %q, want it unchanged", s, got)
		}
	}
}

func TestTruncateAppendsEllipsis(t *testing.T) {
	t.Parallel()

	if got, want := Truncate("abcdefghij", 5), "abcde"+Ellipsis; got != want {
		t.Errorf("Truncate = %q, want %q", got, want)
	}
}

// The four copies this replaced all cut with s[:n], which splits a multi-byte
// rune whenever the limit lands inside one. These limits are applied to
// upstream error bodies and issue summaries, which are routinely not ASCII.
func TestTruncateNeverSplitsARune(t *testing.T) {
	t.Parallel()

	// Every rune here is multi-byte, so most byte limits land mid-rune.
	const s = "çğıöşüÇĞİÖŞÜ日本語テキスト"

	for n := 1; n <= len(s); n++ {
		got := Truncate(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("Truncate(s, %d) = %q, which is not valid UTF-8", n, got)
		}
		if len(got) > n+len(Ellipsis) {
			t.Fatalf("Truncate(s, %d) produced %d bytes, want at most %d", n, len(got), n+len(Ellipsis))
		}
	}
}

func TestTruncateNonPositiveLimit(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -1} {
		if got := Truncate("anything", n); got != "" {
			t.Errorf("Truncate(_, %d) = %q, want empty", n, got)
		}
	}
}

func TestTruncateSpaceTrimsFirst(t *testing.T) {
	t.Parallel()

	if got := TruncateSpace("   hello   ", 20); got != "hello" {
		t.Errorf("TruncateSpace = %q, want %q", got, "hello")
	}
	if got, want := TruncateSpace("  abcdefghij  ", 4), "abcd"+Ellipsis; got != want {
		t.Errorf("TruncateSpace = %q, want %q", got, want)
	}
}
