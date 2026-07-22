package gitops

import "testing"

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
