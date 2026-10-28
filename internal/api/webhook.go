package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// webhook handles push events from GitHub, Gitea, Forgejo and GitLab. GitHub,
// Gitea and Forgejo sign the body with the shared secret (HMAC-SHA256); GitLab
// sends the secret itself as a token. A push to the root or any Application's
// repository triggers a refresh.
func (s *server) webhook(w http.ResponseWriter, r *http.Request) {
	if s.WebhookSecret == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "webhooks are not configured (webhook.secretFile)"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 25<<20))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload too large"})
		return
	}
	if !s.verify(r, body) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid webhook signature"})
		return
	}
	event := firstHeader(r, "X-GitHub-Event", "X-Gitea-Event", "X-Forgejo-Event", "X-Gitlab-Event")
	switch strings.ToLower(event) {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	case "push", "push hook", "tag push hook", "create":
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored event " + event})
		return
	}
	var payload struct {
		Repository struct {
			CloneURL   string `json:"clone_url"`
			SSHURL     string `json:"ssh_url"`
			HTMLURL    string `json:"html_url"`
			GitHTTPURL string `json:"git_http_url"`
			GitSSHURL  string `json:"git_ssh_url"`
		} `json:"repository"`
		Project struct {
			GitHTTPURL string `json:"git_http_url"`
			GitSSHURL  string `json:"git_ssh_url"`
		} `json:"project"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return
	}
	rp, pp := payload.Repository, payload.Project
	if s.c.RefreshRepos([]string{rp.CloneURL, rp.SSHURL, rp.HTMLURL, rp.GitHTTPURL, rp.GitSSHURL, pp.GitHTTPURL, pp.GitSSHURL}) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "refresh requested"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "no application uses this repository"})
}

func (s *server) verify(r *http.Request, body []byte) bool {
	if token := r.Header.Get("X-Gitlab-Token"); token != "" {
		return subtle.ConstantTimeCompare([]byte(token), []byte(s.WebhookSecret)) == 1
	}
	sig := strings.TrimPrefix(firstHeader(r, "X-Hub-Signature-256", "X-Gitea-Signature", "X-Forgejo-Signature"), "sha256=")
	got, err := hex.DecodeString(sig)
	if err != nil || len(got) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.WebhookSecret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := r.Header.Get(n); v != "" {
			return v
		}
	}
	return ""
}
