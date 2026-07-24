package gitops

import (
	"reflect"
	"testing"
)

func TestParseBlamePorcelain(t *testing.T) {
	// Two lines from commit aaa, one from bbb. Header groups carry a size field
	// on the first line of a group; subsequent lines omit it.
	out := "" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 10 10 2\n" +
		"author Alice\n" +
		"author-mail <alice@example.com>\n" +
		"author-time 1700000000\n" +
		"author-tz +0200\n" +
		"summary first commit\n" +
		"filename foo.go\n" +
		"\tfirst line\n" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 11 11\n" +
		"author Alice\n" +
		"author-mail <alice@example.com>\n" +
		"author-time 1700000000\n" +
		"summary first commit\n" +
		"filename foo.go\n" +
		"\tsecond line\n" +
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb 12 12 1\n" +
		"author Bob\n" +
		"author-mail <bob@example.com>\n" +
		"author-time 1700000100\n" +
		"summary second commit\n" +
		"filename foo.go\n" +
		"\t\tindented content\n"

	got := parseBlamePorcelain(out)
	want := []BlameLine{
		{Line: 10, Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Author: "Alice", Email: "alice@example.com", Time: 1700000000, Summary: "first commit", Content: "first line"},
		{Line: 11, Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Author: "Alice", Email: "alice@example.com", Time: 1700000000, Summary: "first commit", Content: "second line"},
		{Line: 12, Commit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Author: "Bob", Email: "bob@example.com", Time: 1700000100, Summary: "second commit", Content: "\tindented content"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseBlamePorcelain mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestParseBlamePorcelainEmpty(t *testing.T) {
	if got := parseBlamePorcelain(""); len(got) != 0 {
		t.Errorf("expected no lines, got %#v", got)
	}
}

func TestParseRepoID(t *testing.T) {
	tests := []struct {
		url     string
		want    string
		wantErr bool
	}{
		{url: "git@github.com:rytsh/krabby.git", want: "github.com/rytsh/krabby"},
		{url: "git@github.com:rytsh/krabby", want: "github.com/rytsh/krabby"},
		{url: "https://github.com/rakunlabs/ada.git", want: "github.com/rakunlabs/ada"},
		{url: "https://github.com/rakunlabs/ada", want: "github.com/rakunlabs/ada"},
		{url: "https://github.com/rakunlabs/ada/", want: "github.com/rakunlabs/ada"},
		{url: "ssh://git@github.com/owner/name.git", want: "github.com/owner/name"},
		{url: "ssh://git@git.example.com:2222/owner/name.git", want: "git.example.com/owner/name"},
		// Host + every path segment (nested GitLab groups included) keep repos
		// on different git servers or groups from colliding.
		{url: "https://gitlab.example.com/group/sub/project.git", want: "gitlab.example.com/group/sub/project"},
		{url: "git@gitlab.com:group/sub/deeper/project.git", want: "gitlab.com/group/sub/deeper/project"},
		{url: "owner/name", want: "owner/name"},
		{url: "not-a-url", wantErr: true},
		{url: "", wantErr: true},
	}

	for _, tt := range tests {
		got, err := ParseRepoID(tt.url)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseRepoID(%q) = %q, want error", tt.url, got)
			}

			continue
		}

		if err != nil {
			t.Errorf("ParseRepoID(%q) error: %v", tt.url, err)

			continue
		}

		if got != tt.want {
			t.Errorf("ParseRepoID(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}
