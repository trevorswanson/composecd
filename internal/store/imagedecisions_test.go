package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestImageDecisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "composecd.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ImageDecisions("web"); err != nil || len(got) != 0 {
		t.Fatalf("empty = %+v, %v", got, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	d := ImageDecision{App: "web", Service: "web", Candidate: "1.27.3", Choice: DecisionMemory, User: "trevor", Fingerprint: "fp1",
		Repo: "https://git.example.lan/me/gitops.git", File: "charts/web/values.yaml", Key: "tag", DecidedAt: now}
	for _, x := range []ImageDecision{d, {App: "other", Service: "web", Candidate: "2", Choice: DecisionDismiss}} {
		if err := s.SetImageDecision(x); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.ImageDecisions("web"); err != nil || len(got) != 1 || got["web"] != d {
		t.Errorf("decisions = %+v, %v", got, err)
	}
	d.Choice, d.Candidate = DecisionDismiss, "1.27.4"
	if err := s.SetImageDecision(d); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ImageDecisions("web"); got["web"] != d {
		t.Errorf("replaced = %+v", got["web"])
	}
	s.Close()

	// Decisions outlive a restart, and deleting one leaves the others.
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.DeleteImageDecision("web", "web"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ImageDecisions("web"); len(got) != 0 {
		t.Errorf("deleted = %+v", got)
	}
	if got, _ := s.ImageDecisions("other"); got["web"].Candidate != "2" || got["web"].DecidedAt.IsZero() {
		t.Errorf("other = %+v", got)
	}
}
