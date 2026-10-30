package web

import (
	"sort"
	"strconv"
	"strings"

	udiff "github.com/aymanbagabas/go-udiff"

	"github.com/trevorswanson/composecd/internal/controller"
)

// FileDiff is one compose resource's deployed and desired YAML side by side.
type FileDiff struct {
	Resource string
	// Op is the resource's change, or "" when both sides are the same.
	Op   string
	Stat Stat
	Rows []DiffRow
}

// DiffRow is a line of each side, where Old or New is "removed", "added" or
// "" for an unchanged line, and "none" where that side has no line; or, with
// Fold, that many unchanged lines left out.
type DiffRow struct {
	OldNo, NewNo     int
	OldText, NewText string
	Old, New         string
	Fold             int
}

// diffContext is how many unchanged lines a compact diff keeps around a
// change; minFold is the fewest it folds away, short of a whole file.
const (
	diffContext = 3
	minFold     = 4
)

// fileDiffs lays out the app's changes side by side; with full, every
// resource in either manifest too, each in whole.
func fileDiffs(a controller.AppStatus, full bool) []FileDiff {
	objects := map[string]controller.Object{}
	for _, o := range a.Objects {
		if o.Kind != controller.KindContainer && (o.Desired != "" || o.Deployed != "") {
			objects[o.ID] = o
		}
	}
	var out []FileDiff
	seen := map[string]bool{}
	for _, ch := range a.Sync.Changes {
		seen[ch.Resource] = true
		f := FileDiff{Resource: ch.Resource, Op: ch.Op}
		o, ok := objects[ch.Resource]
		switch {
		case !ok:
			// A top-level key that isn't an object: only its diff is kept.
			f.Rows = pairRows(unifiedLines(ch.Diff))
		case ch.Op == controller.OpMissing:
			// As its diff: its whole desired state, against nothing.
			f.Rows = sideBySide("", o.Desired, full)
		default:
			f.Rows = sideBySide(o.Deployed, o.Desired, full)
		}
		f.Stat = rowStat(f.Rows)
		out = append(out, f)
	}
	if full {
		for id, o := range objects {
			if !seen[id] {
				out = append(out, FileDiff{Resource: id, Rows: sideBySide(o.Deployed, o.Desired, true)})
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	}
	return out
}

// diffLine is a line of a diff: one of udiff's Delete, Insert or Equal, with
// its numbers on each side (0 for the side it isn't on).
type diffLine struct {
	kind         udiff.OpKind
	text         string
	oldNo, newNo int
}

// sideBySide compares old and new by line; unless full, unchanged lines
// further than diffContext from a change are folded away.
func sideBySide(old, new string, full bool) []DiffRow {
	oldLines := splitLines(old)
	var lines []diffLine
	o, n := 0, 0
	add := func(kind udiff.OpKind, text string) {
		l := diffLine{kind: kind, text: strings.TrimSuffix(text, "\n")}
		if kind != udiff.Insert {
			o++
			l.oldNo = o
		}
		if kind != udiff.Delete {
			n++
			l.newNo = n
		}
		lines = append(lines, l)
	}
	if old != new {
		// Context enough for a single hunk over the whole of both.
		u, err := udiff.ToUnifiedDiff("old", "new", old, udiff.Lines(old, new), len(oldLines)+len(splitLines(new))+1)
		if err != nil {
			return pairRows([]diffLine{{kind: udiff.Equal, text: err.Error()}})
		}
		for _, h := range u.Hunks {
			for o < h.FromLine-1 && o < len(oldLines) {
				add(udiff.Equal, oldLines[o])
			}
			for _, l := range h.Lines {
				add(l.Kind, l.Content)
			}
		}
	}
	for o < len(oldLines) {
		add(udiff.Equal, oldLines[o])
	}
	if !full {
		lines = fold(lines)
	}
	return pairRows(lines)
}

// fold replaces each run of unchanged lines further than diffContext from a
// change with a single Equal line whose oldNo is minus the count it hides.
func fold(lines []diffLine) []diffLine {
	near := make([]bool, len(lines))
	for i, l := range lines {
		if l.kind == udiff.Equal {
			continue
		}
		for j := max(0, i-diffContext); j <= min(len(lines)-1, i+diffContext); j++ {
			near[j] = true
		}
	}
	var out []diffLine
	for i := 0; i < len(lines); {
		if near[i] {
			out = append(out, lines[i])
			i++
			continue
		}
		j := i
		for j < len(lines) && !near[j] {
			j++
		}
		if j-i < minFold && (i > 0 || j < len(lines)) {
			// A fold row takes about as much room as these lines.
			out = append(out, lines[i:j]...)
		} else {
			out = append(out, diffLine{kind: udiff.Equal, oldNo: -(j - i)})
		}
		i = j
	}
	return out
}

// unifiedLines reads a unified diff (as Change.Diff has it) back into lines.
func unifiedLines(diff string) []diffLine {
	var out []diffLine
	o, n := 0, 0
	for _, s := range splitLines(diff) {
		s = strings.TrimSuffix(s, "\n")
		switch {
		case strings.HasPrefix(s, "@@"):
			// "@@ -o,len +n,len @@"
			f := strings.Fields(s)
			if len(f) < 3 {
				continue
			}
			start := hunkStart(f[1]) - 1
			if start > o {
				out = append(out, diffLine{kind: udiff.Equal, oldNo: -(start - o)})
			}
			o, n = start, hunkStart(f[2])-1
		case strings.HasPrefix(s, "-"):
			o++
			out = append(out, diffLine{kind: udiff.Delete, text: s[1:], oldNo: o})
		case strings.HasPrefix(s, "+"):
			n++
			out = append(out, diffLine{kind: udiff.Insert, text: s[1:], newNo: n})
		case strings.HasPrefix(s, " "):
			o, n = o+1, n+1
			out = append(out, diffLine{kind: udiff.Equal, text: s[1:], oldNo: o, newNo: n})
		}
	}
	return out
}

// hunkStart is the first line number of a hunk header's "-12,5" or "+12,5";
// an empty side ("-0,0") starts at 1.
func hunkStart(s string) int {
	s, _, _ = strings.Cut(strings.TrimLeft(s, "-+"), ",")
	v, _ := strconv.Atoi(s)
	return max(v, 1)
}

// pairRows sets removed lines beside the added lines that replace them.
// Folded runs (a negative oldNo) become fold rows.
func pairRows(lines []diffLine) []DiffRow {
	var rows []DiffRow
	for i := 0; i < len(lines); {
		l := lines[i]
		if l.kind == udiff.Equal {
			if l.oldNo < 0 {
				rows = append(rows, DiffRow{Fold: -l.oldNo})
			} else {
				rows = append(rows, DiffRow{OldNo: l.oldNo, NewNo: l.newNo, OldText: l.text, NewText: l.text})
			}
			i++
			continue
		}
		var del, ins []diffLine
		for ; i < len(lines) && lines[i].kind != udiff.Equal; i++ {
			if lines[i].kind == udiff.Delete {
				del = append(del, lines[i])
			} else {
				ins = append(ins, lines[i])
			}
		}
		for k := 0; k < max(len(del), len(ins)); k++ {
			r := DiffRow{Old: "none", New: "none"}
			if k < len(del) {
				r.OldNo, r.OldText, r.Old = del[k].oldNo, del[k].text, "removed"
			}
			if k < len(ins) {
				r.NewNo, r.NewText, r.New = ins[k].newNo, ins[k].text, "added"
			}
			rows = append(rows, r)
		}
	}
	return rows
}

func rowStat(rows []DiffRow) Stat {
	var s Stat
	for _, r := range rows {
		if r.Old == "removed" {
			s.Del++
		}
		if r.New == "added" {
			s.Add++
		}
	}
	return s
}

// splitLines splits s after each newline, without a trailing empty line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.SplitAfter(strings.TrimSuffix(s, "\n"), "\n")
}
