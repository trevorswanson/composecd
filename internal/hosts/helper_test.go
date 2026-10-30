package hosts

import (
	"testing"
	"time"
)

func TestParseHelpers(t *testing.T) {
	out := `{"Command":"\"/sbin/tini -- composecd self-apply\"","CreatedAt":"2026-10-30 12:26:36 +0000 UTC","Labels":"composecd.self-apply=12,composecd.self-apply.app=composecd","Names":"composecd-self-apply-12","State":"exited"}
{"CreatedAt":"2026-10-29 08:00:00 -0500 CDT","Labels":"composecd.self-apply=13","Names":"composecd-self-apply-13","State":"running"}
`
	list, err := parseHelpers(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d helpers", len(list))
	}
	if h := list[0]; h.Name != "composecd-self-apply-12" || h.Running || h.Labels["composecd.self-apply"] != "12" ||
		h.Labels["composecd.self-apply.app"] != "composecd" || !h.Created.Equal(time.Date(2026, 10, 30, 12, 26, 36, 0, time.UTC)) {
		t.Errorf("first = %+v", h)
	}
	if h := list[1]; !h.Running || !h.Created.Equal(time.Date(2026, 10, 29, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("second = %+v", h)
	}
	if list, err := parseHelpers("\n"); err != nil || len(list) != 0 {
		t.Errorf("empty output: %v %v", list, err)
	}
}
