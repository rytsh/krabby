package gitops

import "testing"

func TestParseRepoID(t *testing.T) {
	tests := []struct {
		url     string
		want    string
		wantErr bool
	}{
		{url: "git@github.com:rytsh/krabby.git", want: "rytsh/krabby"},
		{url: "git@github.com:rytsh/krabby", want: "rytsh/krabby"},
		{url: "https://github.com/rakunlabs/ada.git", want: "rakunlabs/ada"},
		{url: "https://github.com/rakunlabs/ada", want: "rakunlabs/ada"},
		{url: "https://github.com/rakunlabs/ada/", want: "rakunlabs/ada"},
		{url: "ssh://git@github.com/owner/name.git", want: "owner/name"},
		{url: "https://gitlab.example.com/group/sub/project.git", want: "sub/project"},
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
