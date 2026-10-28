package controller

import "testing"

func TestNormalizeRepoURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://git.lan/Me/homelab.git":       "git.lan/me/homelab",
		"https://bot:token@git.lan/me/homelab": "git.lan/me/homelab",
		"git@git.lan:me/homelab.git":           "git.lan/me/homelab",
		"ssh://git@git.lan:2222/me/homelab":    "git.lan/me/homelab",
		"http://git.lan:3000/me/homelab/":      "git.lan/me/homelab",
		"/srv/git/homelab.git":                 "/srv/git/homelab",
	} {
		if got := NormalizeRepoURL(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

func TestRefreshRepos(t *testing.T) {
	e := newEnv(t)
	e.refresh()
	if e.c.RefreshRepos([]string{"https://elsewhere/x.git"}) {
		t.Error("an unrelated repository triggered a refresh")
	}
	if !e.c.RefreshRepos([]string{"", e.repo + ".git"}) {
		t.Error("the root repository didn't trigger a refresh")
	}
	select {
	case <-e.c.refreshCh:
	default:
		t.Error("no refresh was queued")
	}
}
