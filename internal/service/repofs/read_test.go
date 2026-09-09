package repofs

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadFileBytesSizeHint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		hint    int64
		limit   int
		want    string
	}{
		{"stable", "abcdef", 6, 10, "abcdef"},
		{"shrunk", "abc", 6, 10, "abc"},
		{"grown", "abcdef", 2, 10, "abcdef"},
		{"grown_capped", "abcdefgh", 2, 5, "abcde"},
		{"grown_one_byte", "abcd", 3, 4, "abcd"},
		{"zero_size_hint", "abcdef", 0, 10, "abcdef"},
		{"empty", "", 0, 10, ""},
		{"offset_past_end", "", -100, 10, ""},
		{"large", "abcdefgh", 8, 3, "abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readFileBytes(strings.NewReader(tc.content), tc.limit, tc.hint)
			if err != nil || string(got) != tc.want {
				t.Fatalf("got=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
}

type failedFileRead struct{ err error }

func (r failedFileRead) Read([]byte) (int, error) { return 0, r.err }

func TestReadFileBytesPropagatesErrors(t *testing.T) {
	failure := errors.New("read failed")
	// Fail during the initial read, the growth probe, and the remaining read.
	for _, hint := range []int64{10, 3, 1} {
		r := io.MultiReader(strings.NewReader("abc"), failedFileRead{failure})
		if _, err := readFileBytes(r, 20, hint); !errors.Is(err, failure) {
			t.Fatalf("hint=%d err=%v", hint, err)
		}
	}
}
