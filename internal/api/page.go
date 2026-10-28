package api

import (
	"crypto/subtle"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/windows"
)

type pageData struct {
	Version  string
	LoggedIn bool
	Auth     bool
	Failed   bool
	Root     controller.RootStatus
	Hosts    []controller.HostStatus
	Apps     []controller.AppStatus
	Windows  []windows.Status
	Now      time.Time
}

// page renders the status page, or the login form when not authorized.
func (s *server) page(render func(http.ResponseWriter, *pageData)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := &pageData{Version: s.Version, Auth: !s.AuthDisabled, Now: time.Now(), Failed: r.URL.Query().Has("failed")}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		if !s.authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			if err := pageTemplate.Execute(w, d); err != nil {
				s.Log.Error("rendering the login page", "error", err)
			}
			return
		}
		d.LoggedIn = true
		render(w, d)
	}
}

func (s *server) statusPage(w http.ResponseWriter, d *pageData) {
	d.Root, d.Hosts, d.Apps, d.Windows = s.c.Root(), s.c.HostStatuses(), s.c.Apps(), s.c.Windows()
	if err := pageTemplate.Execute(w, d); err != nil {
		s.Log.Error("rendering the status page", "error", err)
	}
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("token")
	if s.AuthDisabled || s.Token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) != 1 {
		http.Redirect(w, r, "/?failed", http.StatusSeeOther)
		return
	}
	// Secure only over HTTPS: on a plain-HTTP LAN the browser would never send it back.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see above; HttpOnly and SameSite=Strict are set
		Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", MaxAge: 7 * 24 * 3600,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode}) //nolint:gosec // clears the cookie
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

var pageTemplate = template.Must(template.New("page").Funcs(template.FuncMap{
	"short": func(s string) string {
		if len(s) > 7 {
			return s[:7]
		}
		return s
	},
	"ago": func(now, t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return now.Sub(t).Truncate(time.Second).String() + " ago"
	},
	// lower turns a status (a string type) into its CSS class.
	"lower": func(v interface{}) string { return strings.ToLower(fmt.Sprint(v)) },
}).Parse(pageHTML))

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{- if .LoggedIn }}<meta http-equiv="refresh" content="30">{{ end }}
<title>composecd</title>
<style>
:root { --bg: #f7f7f8; --fg: #1d1d1f; --muted: #6e6e73; --card: #fff; --line: #e3e3e6;
  --ok: #1a7f37; --warn: #9a6700; --bad: #cf222e; --info: #0969da; }
@media (prefers-color-scheme: dark) {
  :root { --bg: #111214; --fg: #e8e8ea; --muted: #9a9aa0; --card: #1b1c1f; --line: #2c2d31;
    --ok: #3fb950; --warn: #d29922; --bad: #f85149; --info: #58a6ff; }
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--fg);
  font: 14px/1.45 system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 1200px; margin: 0 auto; padding: 24px 16px; }
header { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
h1 { font-size: 20px; margin: 0; }
h2 { font-size: 15px; margin: 28px 0 8px; }
.muted { color: var(--muted); }
.card { background: var(--card); border: 1px solid var(--line); border-radius: 8px; overflow-x: auto; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 8px 12px; border-bottom: 1px solid var(--line); vertical-align: top; }
th { font-weight: 600; color: var(--muted); font-size: 12px; }
tr:last-child td { border-bottom: 0; }
code { font: 12px ui-monospace, SFMono-Regular, Menlo, monospace; }
.badge { display: inline-block; padding: 1px 8px; border-radius: 999px; font-size: 12px; font-weight: 600;
  border: 1px solid currentColor; }
.synced, .healthy, .up, .succeeded { color: var(--ok); }
.outofsync, .progressing, .running, .suspended { color: var(--warn); }
.degraded, .missing, .down, .failed { color: var(--bad); }
.unknown { color: var(--muted); }
.error { color: var(--bad); font-size: 12px; margin-top: 4px; }
form.login { max-width: 360px; margin: 80px auto; display: grid; gap: 10px; }
input, button { font: inherit; padding: 8px 10px; border-radius: 6px; border: 1px solid var(--line);
  background: var(--card); color: var(--fg); }
button { cursor: pointer; }
</style>
</head>
<body><main>
{{- if not .LoggedIn }}
<form class="login card" method="post" action="/login" style="padding:20px">
  <h1>composecd</h1>
  {{- if .Auth }}
  <label for="token" class="muted">Admin token</label>
  <input id="token" name="token" type="password" autocomplete="current-password" required autofocus>
  {{- if .Failed }}<div class="error">That token is not valid.</div>{{ end }}
  <button type="submit">Sign in</button>
  {{- end }}
</form>
{{- else }}
<header>
  <h1>composecd <span class="muted" style="font-weight:400;font-size:13px">{{ .Version }}</span></h1>
  <div class="muted">
    Root <code>{{ .Root.RepoURL }}</code>{{ with .Root.Path }} <code>{{ . }}</code>{{ end }}
    @ <code>{{ .Root.TargetRevision }}</code> = <code>{{ short .Root.Revision }}</code>,
    refreshed {{ ago .Now .Root.RefreshedAt }}
    {{- if .Auth }} <form method="post" action="/logout" style="display:inline"><button type="submit" style="padding:2px 8px">Sign out</button></form>{{ end }}
  </div>
</header>
{{- with .Root.Error }}<div class="error">{{ . }}</div>{{ end }}

<h2>Applications <span class="muted">({{ len .Apps }})</span></h2>
<div class="card"><table>
<tr><th>Name</th><th>Host</th><th>Sync</th><th>Health</th><th>Revision</th><th>Last operation</th></tr>
{{- range .Apps }}
<tr>
  <td><strong>{{ .Name }}</strong>{{ if .Automated }} <span class="muted">auto</span>{{ end }}{{ if .Paused }} <span class="badge suspended" title="{{ .PausedReason }}">paused</span>{{ end }}
    {{- with .Error }}<div class="error">{{ . }}</div>{{ end }}</td>
  <td>{{ .Host }}</td>
  <td><span class="badge {{ lower .Sync.Status }}">{{ .Sync.Status }}</span>
    {{- range .Sync.Reasons }}<div class="muted" style="font-size:12px">{{ . }}</div>{{ end }}
    {{- with .Blocked }}<div class="muted" style="font-size:12px">waiting: {{ . }}</div>{{ end }}</td>
  <td><span class="badge {{ lower .Health.Status }}">{{ .Health.Status }}</span>
    {{- with .Health.Message }}<div class="muted" style="font-size:12px">{{ . }}</div>{{ end }}</td>
  <td>{{ if .Source.Chart }}<code>{{ .Source.Chart }}</code>{{ else }}<code>{{ short .Sync.Revision }}</code>{{ end }}
    {{- with .Release }}<div class="muted" style="font-size:12px">release rev {{ .Revision }} · {{ .Chart }}</div>{{ end }}</td>
  <td>{{ with .Operation }}<span class="badge {{ lower .Phase }}">{{ .Phase }}</span> {{ .Kind }} ({{ .Trigger }})
    <div class="muted" style="font-size:12px">{{ ago $.Now .StartedAt }}</div>{{ else }}<span class="muted">none</span>{{ end }}</td>
</tr>
{{- else }}
<tr><td colspan="6" class="muted">No applications defined yet.</td></tr>
{{- end }}
</table></div>

<h2>Hosts <span class="muted">({{ len .Hosts }})</span></h2>
<div class="card"><table>
<tr><th>Name</th><th>Engine</th><th>State</th><th>Versions</th><th>Apps</th></tr>
{{- range .Hosts }}
<tr>
  <td><strong>{{ .Name }}</strong></td>
  <td><code>{{ if .URL }}{{ .URL }}{{ else if .Context }}context {{ .Context }}{{ else }}default{{ end }}</code></td>
  <td><span class="badge {{ lower .State }}">{{ .State }}</span>
    {{- if .Maintenance }} <span class="badge suspended" title="{{ .MaintenanceReason }}">maintenance</span>{{ end }}
    {{- with .Error }}<div class="error">{{ . }}</div>{{ end }}
    {{- with .Orphans }}<div class="muted" style="font-size:12px">orphaned releases: {{ range $i, $o := . }}{{ if $i }}, {{ end }}{{ $o }}{{ end }}</div>{{ end }}</td>
  <td class="muted">{{ with .DockerVersion }}Docker {{ . }}{{ end }}{{ with .ComposeVersion }} · Compose {{ . }}{{ end }}</td>
  <td>{{ .Apps }}</td>
</tr>
{{- else }}
<tr><td colspan="5" class="muted">No hosts defined yet.</td></tr>
{{- end }}
</table></div>
{{- if .Windows }}

<h2>Sync windows <span class="muted">({{ len .Windows }})</span></h2>
<div class="card"><table>
<tr><th>Name</th><th>Kind</th><th>Schedule</th><th>State</th><th>Applications</th></tr>
{{- range .Windows }}
<tr>
  <td><strong>{{ .Name }}</strong>{{ if .ManualSync }} <span class="muted">manual sync allowed</span>{{ end }}</td>
  <td>{{ .Kind }}</td>
  <td><code>{{ .Schedule }}</code> for {{ .Duration }}{{ with .TimeZone }} <span class="muted">{{ . }}</span>{{ end }}</td>
  <td>{{ if .Open }}<span class="badge outofsync">open</span>{{ else }}<span class="muted">closed</span>{{ end }}</td>
  <td>{{ range $i, $a := .Applications }}{{ if $i }}, {{ end }}{{ $a }}{{ else }}<span class="muted">none</span>{{ end }}</td>
</tr>
{{- end }}
</table></div>
{{- end }}
{{- end }}
</main></body>
</html>
`
