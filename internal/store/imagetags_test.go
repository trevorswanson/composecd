package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestImageTags(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "composecd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.ImageTags("web"); err != nil || len(got) != 0 {
		t.Fatalf("empty = %+v, %v", got, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	failed := ImageTag{App: "web", Service: "web", Image: "nginx:1.27.1", Policy: "semver", CheckedAt: now, Error: "registry down"}
	if err := s.SetImageTag(failed); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ImageTags("web"); got["web"].Error != "registry down" || got["web"].Tag != "" || !got["web"].PickedAt.IsZero() {
		t.Errorf("failed = %+v", got["web"])
	}
	picked := ImageTag{App: "web", Service: "web", Image: "nginx:1.27.1", Policy: "semver", Tag: "1.27.3", Digest: "sha256:aa",
		PickedAt: now, CheckedAt: now.Add(time.Minute), Truncated: true}
	other := ImageTag{App: "other", Service: "web", Image: "nginx:1", CheckedAt: now}
	for _, tag := range []ImageTag{picked, other} {
		if err := s.SetImageTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ImageTags("web")
	if err != nil || len(got) != 1 {
		t.Fatalf("tags = %+v, %v", got, err)
	}
	if g := got["web"]; g.Tag != "1.27.3" || g.Digest != "sha256:aa" || !g.PickedAt.Equal(now) || !g.CheckedAt.Equal(now.Add(time.Minute)) ||
		!g.Truncated || g.Error != "" {
		t.Errorf("picked = %+v", g)
	}
}

func TestMigrateAddsImageTags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "composecd.db")
	// A database from before image_tags, with its operations table as Phase 3 left it.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE operations ADD COLUMN user TEXT NOT NULL DEFAULT '';
		ALTER TABLE operations ADD COLUMN helper TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetImageTag(ImageTag{App: "web", Service: "web", Image: "nginx:1", Tag: "1.2", CheckedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ImageTags("web"); err != nil || got["web"].Tag != "1.2" {
		t.Errorf("tags = %+v, %v", got, err)
	}
	s.Close()
	// Opening it again is a no-op.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.ImageTags("web"); err != nil || got["web"].Tag != "1.2" {
		t.Errorf("tags after reopening = %+v, %v", got, err)
	}
}
