package hosts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/trevorswanson/composetpl/pkg/storage"
)

// DiskUsage is what an engine keeps on disk, by kind, and how full the
// filesystem holding its data root is.
type DiskUsage struct {
	// Kinds are images, containers, volumes and build cache, as `docker
	// system df` counts them.
	Kinds []DiskKind `json:"kinds,omitempty"`
	// Root is the engine's data root (DockerRootDir).
	Root string `json:"root,omitempty"`
	// Size, Used and Available are the root filesystem's, in bytes; all zero
	// when it couldn't be measured.
	Size      int64     `json:"size,omitempty"`
	Used      int64     `json:"used,omitempty"`
	Available int64     `json:"available,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	// Error is why the disk couldn't be measured, in part or at all.
	Error string `json:"error,omitempty"`
}

// UsedRatio is the share of the root filesystem in use, from 0 to 1, as df's
// Capacity counts it (blocks reserved for root count as neither); ok is false
// when it is unknown.
func (d *DiskUsage) UsedRatio() (ratio float64, ok bool) {
	if d == nil || d.Used+d.Available <= 0 {
		return 0, false
	}
	return float64(d.Used) / float64(d.Used+d.Available), true
}

// Reclaimable is the space prune could free, over every kind.
func (d *DiskUsage) Reclaimable() int64 {
	var n int64
	for _, k := range d.Kinds {
		n += k.Reclaimable
	}
	return n
}

// DiskKind is one line of `docker system df`.
type DiskKind struct {
	// Kind is images, containers, volumes or build-cache.
	Kind        string `json:"kind"`
	Count       int    `json:"count"`
	Active      int    `json:"active"`
	Size        int64  `json:"size"`
	Reclaimable int64  `json:"reclaimable"`
}

// PruneOptions select what a prune removes.
type PruneOptions struct {
	// All removes unused images (or build cache) that are tagged or still
	// referenced, not just dangling ones.
	All bool
	// Until spares images created more recently (image prune only).
	Until time.Duration
	// KeepStorage is how much build cache to keep (builder prune only).
	KeepStorage string
}

// DiskUsage measures h: `docker system df`, then df on its data root from a
// short-lived helper container that mounts it read-only. The helper uses the
// image of composetpl's volume accessor, which volume storage already pulled.
// A failed df still returns the system df figures, with Error set.
func (c *CLI) DiskUsage(ctx context.Context, h *Host) (*DiskUsage, error) {
	out, err := c.docker(ctx, h, append(h.Global(), "system", "df", "--format", "{{json .}}")...)
	if err != nil {
		return nil, fmt.Errorf("docker system df: %w", err)
	}
	kinds, err := ParseSystemDF(out)
	if err != nil {
		return nil, err
	}
	u := &DiskUsage{Kinds: kinds}
	if err := c.measureRoot(ctx, h, u); err != nil {
		u.Error = err.Error()
	}
	return u, nil
}

func (c *CLI) measureRoot(ctx context.Context, h *Host, u *DiskUsage) error {
	out, err := c.docker(ctx, h, append(h.Global(), "info", "--format", "{{json .DockerRootDir}}")...)
	if err != nil {
		return fmt.Errorf("docker info: %w", err)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &u.Root); err != nil {
		return fmt.Errorf("docker info: data root %q: %w", strings.TrimSpace(out), err)
	}
	if !strings.HasPrefix(u.Root, "/") {
		// A Windows engine's C:\ProgramData\docker: the helper is a Linux image.
		return fmt.Errorf("can't measure free space on data root %q", u.Root)
	}
	image := c.HelperImage
	if image == "" {
		image = storage.DefaultHelperImage
	}
	out, err = c.RunHelper(ctx, h, HelperSpec{
		Image: image, Network: "none", Volumes: []string{u.Root + ":/data:ro"},
		Labels: map[string]string{"composecd.housekeeping": "df"},
		Args:   []string{"df", "-Pk", "/data"},
	})
	if err != nil {
		return err
	}
	u.Size, u.Used, u.Available, err = ParseDF(out)
	return err
}

// PruneImages runs `docker image prune` on h, writing its output to w.
func (c *CLI) PruneImages(ctx context.Context, h *Host, o PruneOptions, w io.Writer) error {
	return c.prune(ctx, h, w, ImagePruneArgs(o)...)
}

// PruneBuildCache runs `docker builder prune` on h, writing its output to w.
func (c *CLI) PruneBuildCache(ctx context.Context, h *Host, o PruneOptions, w io.Writer) error {
	return c.prune(ctx, h, w, BuilderPruneArgs(o)...)
}

// ImagePruneArgs are the docker arguments that prune images as o says.
func ImagePruneArgs(o PruneOptions) []string {
	args := []string{"image", "prune", "--force"}
	if o.All {
		args = append(args, "--all")
	}
	if o.Until > 0 {
		args = append(args, "--filter", "until="+o.Until.String())
	}
	return args
}

// BuilderPruneArgs are the docker arguments that prune build cache as o says.
func BuilderPruneArgs(o PruneOptions) []string {
	args := []string{"builder", "prune", "--force"}
	if o.All {
		args = append(args, "--all")
	}
	if o.KeepStorage != "" {
		args = append(args, "--keep-storage", o.KeepStorage)
	}
	return args
}

func (c *CLI) prune(ctx context.Context, h *Host, w io.Writer, args ...string) error {
	fmt.Fprintf(w, "$ docker %s\n", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "docker", append(h.Global(), args...)...)
	cmd.Env = append(os.Environ(), h.Env()...)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(args[:2], " "), err)
	}
	return nil
}

// diskKinds names `docker system df` types the way metrics label them.
var diskKinds = map[string]string{
	"Images":        "images",
	"Containers":    "containers",
	"Local Volumes": "volumes",
	"Build Cache":   "build-cache",
}

// ParseSystemDF reads `docker system df --format '{{json .}}'` lines.
func ParseSystemDF(out string) ([]DiskKind, error) {
	var kinds []DiskKind
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var row struct{ Type, TotalCount, Active, Size, Reclaimable string }
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("docker system df: %w", err)
		}
		k := DiskKind{Kind: diskKinds[row.Type]}
		if k.Kind == "" {
			k.Kind = strings.ReplaceAll(strings.ToLower(row.Type), " ", "-")
		}
		k.Count, _ = strconv.Atoi(row.TotalCount)
		k.Active, _ = strconv.Atoi(row.Active)
		var err error
		if k.Size, err = ParseSize(row.Size); err != nil {
			return nil, fmt.Errorf("docker system df: %s: %w", row.Type, err)
		}
		if k.Reclaimable, err = ParseSize(row.Reclaimable); err != nil {
			return nil, fmt.Errorf("docker system df: %s: %w", row.Type, err)
		}
		kinds = append(kinds, k)
	}
	return kinds, nil
}

// humanSize is a size as docker prints it: decimal units, and for
// reclaimable space a percentage after it, "532.5MB (23%)".
var humanSize = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([kKMGTPEZY]?B)\b`)

var decimalUnits = map[string]float64{"B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15, "EB": 1e18}

// ParseSize reads a size docker printed for humans ("2.301GB", "0B").
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0, nil
	}
	m := humanSize.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("unexpected size %q", s)
	}
	unit, ok := decimalUnits[m[2]]
	if !ok {
		return 0, fmt.Errorf("unexpected size %q", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected size %q", s)
	}
	return int64(n * unit), nil
}

// blocksHeader is df -P's second column, which gives its block size.
var blocksHeader = regexp.MustCompile(`^([0-9]+)-blocks$`)

// ParseDF reads `df -P` output for one filesystem: its size, used and
// available bytes.
func ParseDF(out string) (size, used, avail int64, err error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, 0, 0, fmt.Errorf("df: unexpected output %q", out)
	}
	unit := int64(1024)
	if head := strings.Fields(lines[0]); len(head) > 1 {
		if m := blocksHeader.FindStringSubmatch(head[1]); m != nil {
			unit, _ = strconv.ParseInt(m[1], 10, 64)
		}
	}
	// A long filesystem name may contain spaces; the numbers are the last
	// columns but the mount point.
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 6 {
		return 0, 0, 0, fmt.Errorf("df: unexpected line %q", lines[len(lines)-1])
	}
	f = f[len(f)-5:]
	var n [3]int64
	for i := range n {
		if n[i], err = strconv.ParseInt(f[i], 10, 64); err != nil {
			return 0, 0, 0, errors.New("df: unexpected line " + strconv.Quote(lines[len(lines)-1]))
		}
	}
	return n[0] * unit, n[1] * unit, n[2] * unit, nil
}

// FormatBytes writes n with decimal units, as docker does: "2.3GB".
func FormatBytes(n int64) string {
	const units = "kMGTPE"
	if n < 1000 {
		return strconv.FormatInt(n, 10) + "B"
	}
	f, i := float64(n)/1000, 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + string(units[i]) + "B"
}
