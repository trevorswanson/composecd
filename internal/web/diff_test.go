package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/controller"
)

func numbered(prefix string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s%d: x\n", prefix, i)
	}
	return b.String()
}

func TestSideBySidePairsChanges(t *testing.T) {
	old := "image: a\nports:\n- 80\n" + numbered("k", 10)
	new := "image: b\nports:\n- 80\n- 443\n" + numbered("k", 10)
	rows := sideBySide(old, new, false)
	r := rows[0]
	if r.OldText != "image: a" || r.NewText != "image: b" || r.Old != "removed" || r.New != "added" || r.OldNo != 1 || r.NewNo != 1 {
		t.Errorf("first row = %+v", r)
	}
	if r := rows[3]; r.Old != "none" || r.NewText != "- 443" || r.NewNo != 4 || r.OldNo != 0 {
		t.Errorf("insert row = %+v", r)
	}
	if r := rows[4]; r.OldNo != 4 || r.NewNo != 5 || r.OldText != "k1: x" {
		t.Errorf("context row = %+v", r)
	}
	// Three lines of context after the insert, then the rest folded.
	if last := rows[len(rows)-1]; last.Fold != 7 {
		t.Errorf("last row = %+v, want 7 folded", last)
	}
	if s := rowStat(rows); s.Add != 2 || s.Del != 1 {
		t.Errorf("stat = %+v", s)
	}
	full := sideBySide(old, new, true)
	if len(full) != 14 {
		t.Errorf("full rows = %d, want 14", len(full))
	}
	for _, r := range full {
		if r.Fold != 0 {
			t.Errorf("full diff folded: %+v", r)
		}
	}
}

func TestSideBySideSameAndEmpty(t *testing.T) {
	if rows := sideBySide("a: 1\nb: 2\n", "a: 1\nb: 2\n", false); len(rows) != 1 || rows[0].Fold != 2 {
		t.Errorf("compact unchanged = %+v", rows)
	}
	if rows := sideBySide("a: 1\nb: 2\n", "a: 1\nb: 2\n", true); len(rows) != 2 || rows[1].OldText != "b: 2" || rows[1].NewNo != 2 {
		t.Errorf("full unchanged = %+v", rows)
	}
	rows := sideBySide("", "a: 1\nb: 2\n", false)
	if len(rows) != 2 || rows[0].Old != "none" || rows[1].NewText != "b: 2" || rows[1].NewNo != 2 {
		t.Errorf("added = %+v", rows)
	}
}

func TestUnifiedLinesFoldsBetweenHunks(t *testing.T) {
	rows := pairRows(unifiedLines("@@ -1,2 +1,2 @@\n-a\n+b\n c\n@@ -10,2 +10,2 @@\n d\n-e\n+f\n"))
	if len(rows) != 5 || rows[0].OldText != "a" || rows[0].NewText != "b" || rows[2].Fold != 7 || rows[4].OldNo != 11 || rows[4].NewNo != 11 {
		t.Errorf("rows = %+v", rows)
	}
}

func TestFileDiffs(t *testing.T) {
	a := controller.AppStatus{Objects: []controller.Object{
		{ID: "services/web", Kind: controller.KindService, Deployed: "image: a\n", Desired: "image: b\n"},
		{ID: "services/db", Kind: controller.KindService, Deployed: "image: pg\n", Desired: "image: pg\n"},
		{ID: "containers/web-1", Kind: controller.KindContainer, Parent: "services/web"},
	}}
	a.Sync.Changes = []controller.Change{
		{Resource: "name", Op: "changed", Diff: "@@ -1 +1 @@\n-x\n+y\n"},
		{Resource: "services/web", Op: "changed"},
	}
	compact := fileDiffs(a, false)
	if len(compact) != 2 || compact[0].Rows[0].NewText != "y" || compact[1].Stat.Add != 1 {
		t.Errorf("compact = %+v", compact)
	}
	full := fileDiffs(a, true)
	var ids []string
	for _, f := range full {
		ids = append(ids, f.Resource+":"+f.Op)
	}
	if strings.Join(ids, ",") != "name:changed,services/db:,services/web:changed" {
		t.Errorf("full = %v", ids)
	}
}
