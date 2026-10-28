package events

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const stream = `{"Type":"container","Action":"die","Actor":{"Attributes":{"com.docker.compose.project":"media","name":"media-web-1"}}}
{"Type":"container","Action":"exec_start: sh","Actor":{"Attributes":{"com.docker.compose.project":"media"}}}
{"Type":"container","Action":"health_status: unhealthy","Actor":{"Attributes":{"com.docker.compose.project":"photos"}}}
{"Type":"container","Action":"start","Actor":{"Attributes":{"com.docker.compose.project":"media","com.docker.compose.oneoff":"True"}}}
{"Type":"network","Action":"connect","Actor":{"Attributes":{"com.docker.compose.project":"media"}}}
{"Type":"container","Action":"destroy","Actor":{"Attributes":{"name":"not-compose"}}}
not json
{"Type":"container","Action":"start","Actor":{"Attributes":{"com.docker.compose.project":"tools"}}}
`

func TestScan(t *testing.T) {
	var got []string
	if err := Scan(strings.NewReader(stream), func(p string) { got = append(got, p) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "media,photos,tools" {
		t.Errorf("projects = %v", got)
	}
}

func TestWatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as a fake docker")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "events.json"), []byte(stream), 0o644); err != nil {
		t.Fatal(err)
	}
	// The fake docker checks its arguments and environment, then prints the stream.
	script := `#!/bin/sh
[ "$1 $2 $3 $4" = "--host ssh://pi events --format" ] || { echo "bad args: $*" >&2; exit 2; }
[ "$COMPOSECD_TEST" = yes ] || { echo "env not passed" >&2; exit 3; }
cat "` + filepath.Join(dir, "events.json") + `"
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var got []string
	err := Watch(context.Background(), []string{"--host", "ssh://pi"}, []string{"COMPOSECD_TEST=yes"}, func(p string) { got = append(got, p) })
	if err == nil || !strings.Contains(err.Error(), "stream ended") {
		t.Errorf("err = %v", err)
	}
	if strings.Join(got, ",") != "media,photos,tools" {
		t.Errorf("projects = %v", got)
	}

	err = Watch(context.Background(), []string{"--context", "x"}, nil, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "bad args") {
		t.Errorf("failing docker: %v", err)
	}
}
