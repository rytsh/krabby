package manager

import (
	"reflect"
	"testing"

	"github.com/rytsh/krabby/internal/service/vectorstore"
)

func TestDocsFilter(t *testing.T) {
	tests := []struct {
		name, scope, key string
		want             vectorstore.Filter
		wantErr          bool
	}{
		{name: "all default", want: vectorstore.Filter{}},
		{name: "all explicit", scope: ScopeAll, want: vectorstore.Filter{}},
		{name: "repos", scope: ScopeRepos, want: vectorstore.Filter{ExcludePrefix: "web:"}},
		{name: "sources", scope: ScopeSources, want: vectorstore.Filter{Prefix: "web:"}},
		{name: "single source wins", scope: ScopeRepos, key: "web:wine", want: vectorstore.Filter{Keys: []string{"web:wine"}}},
		{name: "single repo", key: "git.example.com/a/repo", want: vectorstore.Filter{Keys: []string{"git.example.com/a/repo"}}},
		{name: "invalid", scope: "other", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := docsFilter(tt.scope, tt.key)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("filter=%#v want=%#v", got, tt.want)
			}
		})
	}
}
