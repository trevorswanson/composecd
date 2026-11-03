package hosts

import (
	"strings"
	"testing"
	"time"
)

func TestParseSystemDF(t *testing.T) {
	// docker system df --format '{{json .}}' from Docker 29.
	out := `{"Active":"0","Reclaimable":"532.5MB (23%)","Size":"2.301GB","TotalCount":"5","Type":"Images"}
{"Active":"0","Reclaimable":"0B","Size":"0B","TotalCount":"0","Type":"Containers"}
{"Active":"0","Reclaimable":"0B","Size":"0B","TotalCount":"3","Type":"Local Volumes"}
{"Active":"0","Reclaimable":"2.023GB","Size":"2.341GB","TotalCount":"33","Type":"Build Cache"}
`
	kinds, err := ParseSystemDF(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []DiskKind{
		{Kind: "images", Count: 5, Size: 2301000000, Reclaimable: 532500000},
		{Kind: "containers"},
		{Kind: "volumes", Count: 3},
		{Kind: "build-cache", Count: 33, Size: 2341000000, Reclaimable: 2023000000},
	}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %+v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("kind %d = %+v, want %+v", i, kinds[i], want[i])
		}
	}
	if u := (&DiskUsage{Kinds: kinds}); u.Reclaimable() != 2555500000 {
		t.Errorf("reclaimable = %d", u.Reclaimable())
	}
	if _, err := ParseSystemDF(`{"Type":"Images","Size":"lots"}`); err == nil {
		t.Error("bad size accepted")
	}
	if kinds, err := ParseSystemDF("\n"); err != nil || len(kinds) != 0 {
		t.Errorf("empty output: %v %v", kinds, err)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"0B": 0, "1.2kB": 1200, "532.5MB (23%)": 532500000, "2.301GB": 2301000000, "1TB": 1e12, "": 0, "N/A": 0,
	} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"12", "MB", "1.2 parsecs"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) accepted", in)
		}
	}
}

func TestParseDF(t *testing.T) {
	// busybox df -Pk on a Docker Desktop data root.
	out := `Filesystem           1024-blocks    Used Available Capacity Mounted on
/dev/vdb1            102624184   2566876  94798148   3% /data
`
	size, used, avail, err := ParseDF(out)
	if err != nil {
		t.Fatal(err)
	}
	if size != 102624184*1024 || used != 2566876*1024 || avail != 94798148*1024 {
		t.Errorf("df = %d %d %d", size, used, avail)
	}
	u := &DiskUsage{Size: size, Used: used, Available: avail}
	if r, ok := u.UsedRatio(); !ok || r < 0.026 || r > 0.027 {
		t.Errorf("ratio = %v %v", r, ok)
	}
	// POSIX 512-byte blocks, and a filesystem name with a space.
	size, used, avail, err = ParseDF("Filesystem 512-blocks Used Available Capacity Mounted on\nmy disk 1000 600 400 60% /data\n")
	if err != nil || size != 512000 || used != 307200 || avail != 204800 {
		t.Errorf("512-blocks: %d %d %d %v", size, used, avail, err)
	}
	for _, bad := range []string{"", "Filesystem 1024-blocks Used Available Capacity Mounted on", "header\n/dev/sda1 x y z 1% /data"} {
		if _, _, _, err := ParseDF(bad); err == nil {
			t.Errorf("ParseDF(%q) accepted", bad)
		}
	}
	if _, ok := (&DiskUsage{}).UsedRatio(); ok {
		t.Error("ratio of an unmeasured disk")
	}
}

func TestPruneArgs(t *testing.T) {
	if got := strings.Join(ImagePruneArgs(PruneOptions{All: true, Until: 168 * time.Hour}), " "); got != "image prune --force --all --filter until=168h0m0s" {
		t.Errorf("image prune = %q", got)
	}
	if got := strings.Join(ImagePruneArgs(PruneOptions{}), " "); got != "image prune --force" {
		t.Errorf("image prune = %q", got)
	}
	if got := strings.Join(BuilderPruneArgs(PruneOptions{KeepStorage: "10GB"}), " "); got != "builder prune --force --keep-storage 10GB" {
		t.Errorf("builder prune = %q", got)
	}
}

func TestHelperArgs(t *testing.T) {
	got := strings.Join(helperArgs(HelperSpec{Image: "busybox:1.37", Network: "none", Volumes: []string{"/var/lib/docker:/data:ro"},
		Labels: map[string]string{"b": "2", "a": "1"}, Args: []string{"df", "-Pk", "/data"}}), " ")
	if got != "--volume /var/lib/docker:/data:ro --network none --label a=1 --label b=2 -- busybox:1.37 df -Pk /data" {
		t.Errorf("args = %q", got)
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0B", 999: "999B", 1200: "1.2kB", 2301000000: "2.3GB", 5e12: "5.0TB"} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
