package controller

import (
	"strings"
	"testing"

	"github.com/trevorswanson/composetpl/pkg/output"
)

const configTemplate = `services:
  web:
    image: nginx:{{ .Values.tag }}
    configs: [{source: app, target: /etc/app.conf}]
configs:
  app: {content: {{ .Values.conf | quote }}}
`

func TestConfigContentChangeResyncs(t *testing.T) {
	e := newEnv(t)
	e.commit(map[string]string{
		"composecd/apps.yaml":         appYAML("web", true, ""),
		"charts/web/values.yaml":      "tag: '1'\nconf: one\n",
		"charts/web/templates/a.yaml": configTemplate,
	})
	e.refresh()
	first := e.fake().Running["web"]
	if st := e.app("web"); st.Sync.Status != Synced || !strings.Contains(first, output.ContentHashLabel) {
		t.Fatalf("first sync: %+v\n%s", st.Sync, first)
	}
	// The desired render carries the same label the release does.
	e.refresh()
	if st := e.app("web"); st.Sync.Status != Synced || e.ups("web") != 1 {
		t.Fatalf("resynced without a change: %+v ups=%d", st.Sync, e.ups("web"))
	}

	e.commit(map[string]string{"charts/web/values.yaml": "tag: '1'\nconf: two\n"})
	e.refresh()
	second := e.fake().Running["web"]
	if e.ups("web") != 2 || !strings.Contains(second, "two") {
		t.Fatalf("content change didn't sync: ups=%d\n%s", e.ups("web"), second)
	}
	label := func(m string) string {
		for _, l := range strings.Split(m, "\n") {
			if strings.Contains(l, output.ContentHashLabel) {
				return l
			}
		}
		return ""
	}
	if label(first) == label(second) {
		t.Errorf("the content hash didn't change, so compose wouldn't recreate the container: %s", label(second))
	}
}
