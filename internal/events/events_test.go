package events

import (
	"context"
	"encoding/json"
	"fmt"
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
	if err := Scan(strings.NewReader(stream), Handlers{Project: func(p string) { got = append(got, p) }}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "media,photos,tools" {
		t.Errorf("projects = %v", got)
	}
}

// storageStream is what an engine reports for, in order: a read through the
// accessor (acc), a write through it, a read-write helper (rw), a read-only
// helper (ro), and a read-write helper on another volume (other).
const storageStream = `{"Type":"volume","Action":"mount","Actor":{"ID":"composetpl-releases","Attributes":{"container":"acc","destination":"/data","read/write":"true"}}}
{"Type":"container","Action":"archive-path","Actor":{"ID":"acc","Attributes":{"io.composetpl.storage":"v1","name":"composetpl-releases-accessor"}}}
{"Type":"volume","Action":"unmount","Actor":{"ID":"composetpl-releases","Attributes":{"container":"acc"}}}
{"Type":"volume","Action":"mount","Actor":{"ID":"composetpl-releases","Attributes":{"container":"acc","destination":"/data","read/write":"true"}}}
{"Type":"container","Action":"extract-to-dir","Actor":{"ID":"acc","Attributes":{"io.composetpl.storage":"v1","name":"composetpl-releases-accessor"}}}
{"Type":"volume","Action":"unmount","Actor":{"ID":"composetpl-releases","Attributes":{"container":"acc"}}}
{"Type":"container","Action":"create","Actor":{"ID":"rw","Attributes":{"name":"kind_leakey"}}}
{"Type":"volume","Action":"mount","Actor":{"ID":"composetpl-releases","Attributes":{"container":"rw","destination":"/data","read/write":"true"}}}
{"Type":"container","Action":"start","Actor":{"ID":"rw","Attributes":{"name":"kind_leakey"}}}
{"Type":"volume","Action":"unmount","Actor":{"ID":"composetpl-releases","Attributes":{"container":"rw"}}}
{"Type":"container","Action":"destroy","Actor":{"ID":"rw","Attributes":{"name":"kind_leakey"}}}
{"Type":"volume","Action":"mount","Actor":{"ID":"composetpl-releases","Attributes":{"container":"ro","destination":"/data","read/write":"false"}}}
{"Type":"container","Action":"start","Actor":{"ID":"ro","Attributes":{"name":"tender_maxwell"}}}
{"Type":"volume","Action":"mount","Actor":{"ID":"media_data","Attributes":{"container":"other","destination":"/data","read/write":"true"}}}
{"Type":"container","Action":"start","Actor":{"ID":"other","Attributes":{"name":"media-web-1"}}}
`

func TestScanStorage(t *testing.T) {
	var w storageWrites
	var at []int
	for i, l := range strings.Split(strings.TrimSpace(storageStream), "\n") {
		var e event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		if w.written(&e) {
			at = append(at, i+1)
		}
	}
	// The accessor's write and the read-write helper's start.
	if fmt.Sprint(at) != "[5 9]" {
		t.Errorf("storage writes at lines %v, want [5 9]", at)
	}

	calls := 0
	if err := Scan(strings.NewReader(storageStream), Handlers{Storage: func() { calls++ }}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("Scan reported %d storage writes, want 2", calls)
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
	err := Watch(context.Background(), []string{"--host", "ssh://pi"}, []string{"COMPOSECD_TEST=yes"}, Handlers{Project: func(p string) { got = append(got, p) }})
	if err == nil || !strings.Contains(err.Error(), "stream ended") {
		t.Errorf("err = %v", err)
	}
	if strings.Join(got, ",") != "media,photos,tools" {
		t.Errorf("projects = %v", got)
	}

	err = Watch(context.Background(), []string{"--context", "x"}, nil, Handlers{})
	if err == nil || !strings.Contains(err.Error(), "bad args") {
		t.Errorf("failing docker: %v", err)
	}
}
