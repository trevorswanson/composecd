// Package selfapply deploys composecd's own release from a helper container.
// composecd can't run `compose up` on its own container: compose stops that
// container, and the process doing the up with it, before the new one exists.
// So composecd prepares the release and starts a helper from its own image to
// apply it (docs/SELF.md). The helper only talks to its engine and the release
// storage; it needs no Git access or secrets.
package selfapply

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/compose"
	"github.com/trevorswanson/composetpl/pkg/release"
	"github.com/trevorswanson/composetpl/pkg/storage"
)

// Options say what a helper does: apply a prepared Revision, roll back to
// RollbackTo, or Repair the deployed revision.
type Options struct {
	Release string
	// Revision is a revision saved by Install.Prepare or Upgrade.Prepare.
	Revision int
	// RollbackTo is the revision to roll back to (0: the one before the
	// latest); -1 means no rollback.
	RollbackTo int
	Repair     bool
	// Description is recorded on an applied revision.
	Description string
	// Labels are merged over a rollback's labels.
	Labels     map[string]string
	Deploy     action.DeployOptions
	HistoryMax int

	// Storage is the host's release storage: "volume" or "local".
	Storage string
	// LocalDir holds local release storage.
	LocalDir string
	// VolumeHelperImage overrides the volume storage driver's helper image.
	VolumeHelperImage string
	// Env is added to every docker call, for compose's ${VAR} interpolation.
	Env []string
}

// Args returns the self-apply command-line arguments for o; Parse reads them.
func (o Options) Args() []string {
	args := []string{"--release", o.Release, "--storage", o.Storage,
		"--wait=" + strconv.FormatBool(o.Deploy.Wait), "--timeout", o.Deploy.Timeout.String(),
		"--history-max", strconv.Itoa(o.HistoryMax)}
	switch {
	case o.Repair:
		args = append(args, "--repair")
	case o.RollbackTo >= 0:
		args = append(args, "--rollback-to", strconv.Itoa(o.RollbackTo))
	default:
		args = append(args, "--revision", strconv.Itoa(o.Revision))
	}
	add := func(flag, v string) {
		if v != "" {
			args = append(args, flag, v)
		}
	}
	add("--description", o.Description)
	add("--pull", o.Deploy.Pull)
	add("--local-dir", o.LocalDir)
	add("--volume-helper-image", o.VolumeHelperImage)
	if o.Deploy.ForceRecreate {
		args = append(args, "--force-recreate")
	}
	keys := make([]string, 0, len(o.Labels))
	for k := range o.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+o.Labels[k])
	}
	for _, e := range o.Env {
		args = append(args, "--env", e)
	}
	return args
}

// Parse reads arguments made by Args.
func Parse(args []string) (Options, error) {
	o := Options{Labels: map[string]string{}}
	fs := flag.NewFlagSet("self-apply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.Release, "release", "", "")
	fs.IntVar(&o.Revision, "revision", 0, "")
	fs.IntVar(&o.RollbackTo, "rollback-to", -1, "")
	fs.BoolVar(&o.Repair, "repair", false, "")
	fs.StringVar(&o.Description, "description", "", "")
	fs.BoolVar(&o.Deploy.Wait, "wait", true, "")
	fs.DurationVar(&o.Deploy.Timeout, "timeout", 5*time.Minute, "")
	fs.StringVar(&o.Deploy.Pull, "pull", "", "")
	fs.BoolVar(&o.Deploy.ForceRecreate, "force-recreate", false, "")
	fs.IntVar(&o.HistoryMax, "history-max", 10, "")
	fs.StringVar(&o.Storage, "storage", "volume", "")
	fs.StringVar(&o.LocalDir, "local-dir", "", "")
	fs.StringVar(&o.VolumeHelperImage, "volume-helper-image", "", "")
	fs.Func("label", "", func(s string) error {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("label %q is not key=value", s)
		}
		o.Labels[k] = v
		return nil
	})
	fs.Func("env", "", func(s string) error {
		o.Env = append(o.Env, s)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if o.Release == "" {
		return o, fmt.Errorf("--release is required")
	}
	if !o.Repair && o.RollbackTo < 0 && o.Revision < 1 {
		return o, fmt.Errorf("one of --revision, --rollback-to or --repair is required")
	}
	return o, nil
}

// Configuration reaches the engine the helper runs on (DOCKER_HOST, or the
// socket mounted from composecd's container) and the host's release storage.
func (o Options) Configuration(log io.Writer) (*action.Configuration, error) {
	var store storage.Driver
	switch o.Storage {
	case "local":
		if o.LocalDir == "" {
			return nil, fmt.Errorf("--local-dir is required for local storage")
		}
		store = storage.NewLocal(o.LocalDir)
	case "volume", "":
		img := o.VolumeHelperImage
		if img == "" {
			img = os.Getenv("COMPOSECD_HELPER_IMAGE")
		}
		v := storage.NewVolume(nil, img)
		v.Env = o.Env
		store = v
	default:
		return nil, fmt.Errorf("unknown storage %q", o.Storage)
	}
	cli := compose.NewCLI(nil, log)
	cli.Env = o.Env
	return &action.Configuration{Store: store, Compose: cli, Log: log}, nil
}

// Run carries out o with cfg and returns the release revision it left.
func Run(ctx context.Context, cfg *action.Configuration, o Options) (*release.Release, error) {
	// Compose may need the project directory to exist where it runs.
	if h, err := cfg.Store.History(o.Release); err == nil && len(h) > 0 {
		if dir := h[len(h)-1].ProjectDir; dir != "" && filepath.IsAbs(dir) {
			_ = os.MkdirAll(dir, 0o755)
		}
	}
	switch {
	case o.Repair:
		r := action.NewRepair(cfg)
		r.DeployOptions = o.Deploy
		return r.Run(ctx, o.Release)
	case o.RollbackTo >= 0:
		rb := action.NewRollback(cfg)
		rb.Version, rb.DeployOptions, rb.HistoryMax = o.RollbackTo, o.Deploy, o.HistoryMax
		rb.Labels = o.Labels
		return rb.Run(ctx, o.Release)
	}
	a := action.NewApply(cfg)
	a.DeployOptions, a.HistoryMax, a.Description = o.Deploy, o.HistoryMax, o.Description
	// A broken new version is rolled back by the helper: nothing else is left
	// running to do it.
	a.Atomic = true
	return a.Run(ctx, o.Release, o.Revision)
}
