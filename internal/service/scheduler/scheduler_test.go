package scheduler

import (
	"testing"

	"github.com/rytsh/krabby/internal/service/settings"
)

func TestScheduleSignatureChangesWithContent(t *testing.T) {
	t.Parallel()

	a := []settings.RepoSchedule{{Namespace: "*", Specs: []string{"0 * * * *"}}}
	b := []settings.RepoSchedule{{Namespace: "*", Specs: []string{"0 */6 * * *"}}}
	c := []settings.RepoSchedule{{Namespace: "team-a", Specs: []string{"0 * * * *"}}}

	if scheduleSignature(a) == scheduleSignature(b) {
		t.Fatal("different specs produced the same signature")
	}
	if scheduleSignature(a) == scheduleSignature(c) {
		t.Fatal("different namespaces produced the same signature")
	}
	if scheduleSignature(a) != scheduleSignature(a) {
		t.Fatal("signature is not stable for identical input")
	}

	// The disabled flag must change the signature so reconcile reloads.
	d := []settings.RepoSchedule{{Namespace: "*", Specs: []string{"0 * * * *"}, Disabled: true}}
	if scheduleSignature(a) == scheduleSignature(d) {
		t.Fatal("disabled flag did not affect the signature")
	}
}

func TestBuildCronsSkipsDisabledAndEmpty(t *testing.T) {
	t.Parallel()

	s := &scheduler{}
	crons := s.buildCrons([]settings.RepoSchedule{
		{Namespace: "*", Specs: []string{"0 * * * *"}},
		{Namespace: "team-a", Specs: []string{"*/15 * * * *"}, Disabled: true},
		{Namespace: "team-b", Specs: nil},
	})

	if len(crons) != 1 {
		t.Fatalf("expected 1 cron (disabled and empty skipped), got %d", len(crons))
	}
	if crons[0].Name != "repo-poll:all" {
		t.Fatalf("unexpected cron name %q", crons[0].Name)
	}
}

func TestNamespaceLabel(t *testing.T) {
	t.Parallel()

	cases := map[string]string{"": "default", "  ": "default", "*": "all", "team-a": "team-a"}
	for in, want := range cases {
		if got := namespaceLabel(in); got != want {
			t.Errorf("namespaceLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
