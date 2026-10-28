package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "composecd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUsers(t *testing.T) {
	s := openTest(t)
	if err := s.AddUser(&User{Name: "trevor", Role: "admin", PasswordHash: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser(&User{Name: "trevor", Role: "readonly", PasswordHash: "h"}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate user: %v", err)
	}
	_ = s.AddUser(&User{Name: "guest", Role: "readonly", PasswordHash: "h2"})
	u, err := s.User("trevor")
	if err != nil || u.Role != "admin" || u.PasswordHash != "h1" || u.CreatedAt.IsZero() {
		t.Errorf("user = %+v, %v", u, err)
	}
	if us, _ := s.Users(); len(us) != 2 || us[0].Name != "guest" || us[0].PasswordHash != "" {
		t.Errorf("users = %+v", us)
	}

	now := time.Now()
	se := &Session{IDHash: "sess", User: "trevor", Role: "admin", Via: "password", CSRF: "c", CreatedAt: now, LastSeenAt: now}
	if err := s.AddSession(se); err != nil {
		t.Fatal(err)
	}
	// A new password signs the user out.
	if err := s.SetPassword("trevor", "h3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session("sess"); !errors.Is(err, ErrNotFound) {
		t.Errorf("session survived a password change: %v", err)
	}
	if u, _ := s.User("trevor"); u.PasswordHash != "h3" {
		t.Errorf("hash = %q", u.PasswordHash)
	}
	if err := s.SetPassword("nobody", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user: %v", err)
	}
	if err := s.DeleteUser("guest"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.User("guest"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted user: %v", err)
	}
	if err := s.DeleteUser("guest"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
}

func TestTokensAndSessions(t *testing.T) {
	s := openTest(t)
	if err := s.AddToken(&Token{ID: "abc", Name: "ci", Role: "readonly", SecretHash: "hash", CreatedBy: "trevor"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddToken(&Token{ID: "def", Name: "ci", Role: "admin", SecretHash: "h"}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate name: %v", err)
	}
	tok, err := s.Token("abc")
	if err != nil || tok.Name != "ci" || tok.SecretHash != "hash" || tok.LastUsedAt != nil {
		t.Errorf("token = %+v, %v", tok, err)
	}
	used := time.Now()
	if err := s.TouchToken("abc", used); err != nil {
		t.Fatal(err)
	}
	if ts, _ := s.Tokens(); len(ts) != 1 || ts[0].LastUsedAt == nil || !ts[0].LastUsedAt.Equal(used) {
		t.Errorf("tokens = %+v", ts)
	}
	if err := s.DeleteToken("ci"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token("abc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token: %v", err)
	}

	old := time.Now().Add(-48 * time.Hour)
	_ = s.AddSession(&Session{IDHash: "old", User: "a", Role: "admin", Via: "oidc", CSRF: "c", CreatedAt: old, LastSeenAt: old})
	_ = s.AddSession(&Session{IDHash: "new", User: "a", Role: "admin", Via: "oidc", CSRF: "c", CreatedAt: time.Now(), LastSeenAt: time.Now()})
	if err := s.DeleteSessionsBefore(time.Now().Add(-24*time.Hour), time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session("old"); !errors.Is(err, ErrNotFound) {
		t.Error("idle session kept")
	}
	if se, err := s.Session("new"); err != nil || se.Via != "oidc" {
		t.Errorf("fresh session = %+v, %v", se, err)
	}
	if err := s.DeleteSession("new"); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateAddsOperationUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "composecd.db")
	// The operations table as Phase 2 created it, with one row.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE operations (id INTEGER PRIMARY KEY AUTOINCREMENT, app TEXT NOT NULL, kind TEXT NOT NULL,
		trigger TEXT NOT NULL, source_revision TEXT NOT NULL DEFAULT '', release_revision INTEGER NOT NULL DEFAULT 0,
		phase TEXT NOT NULL, message TEXT NOT NULL DEFAULT '', log TEXT NOT NULL DEFAULT '', started_at TEXT NOT NULL, finished_at TEXT);
		INSERT INTO operations (app, kind, trigger, phase, started_at) VALUES ('media', 'sync', 'auto', 'Succeeded', '2026-10-28T12:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ops, err := s.Operations("media", 5)
	if err != nil || len(ops) != 1 || ops[0].User != "" {
		t.Fatalf("old operation = %+v, %v", ops, err)
	}
	op := &Operation{App: "media", Kind: KindSync, Trigger: TriggerManual, User: "trevor"}
	if err := s.StartOperation(op); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Operation(op.ID); got.User != "trevor" {
		t.Errorf("user = %q", got.User)
	}
}
