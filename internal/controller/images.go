package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/compose"
	"github.com/trevorswanson/composetpl/pkg/release"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/images"
)

// Image pins.
const (
	// PinComposecd is a moving tag composecd pinned to the digest its registry
	// last served.
	PinComposecd = "composecd"
	// PinGit is a digest written in Git, which composecd never changes.
	PinGit = "git"
)

// gitPinNote explains why an update to a digest pinned in Git can't be applied.
const gitPinNote = "the digest is pinned in Git; change it there to update"

var (
	// imageTimeout bounds one registry check.
	imageTimeout = 30 * time.Second
	// imageConcurrency bounds the registry checks run at once.
	imageConcurrency = 4
)

// ImageStatus is one service's image and what its registry serves for its tag.
type ImageStatus struct {
	Service string `json:"service"`
	// Image is the image as Git renders it.
	Image string `json:"image"`
	// Ref is the tag checked, as registry/repository:tag.
	Ref string `json:"ref,omitempty"`
	// Pin is who pinned the desired digest: composecd (a moving tag) or git.
	Pin string `json:"pin,omitempty"`
	// Digest is the digest the desired manifest pins.
	Digest string `json:"digest,omitempty"`
	// Deployed is the digest the deployed release pins, if any.
	Deployed string `json:"deployed,omitempty"`
	// Latest is the digest the registry last served for the tag.
	Latest    string    `json:"latest,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	// UpdateAvailable is true when the deployed image is behind its tag.
	UpdateAvailable bool `json:"updateAvailable,omitempty"`
	// Applicable is true when a sync deploys the update; it is false for a
	// digest pinned in Git, which only Git can change.
	Applicable bool   `json:"applicable,omitempty"`
	Note       string `json:"note,omitempty"`
	// Error is the last failed registry check.
	Error string `json:"error,omitempty"`
}

// pinResult is what pinImages learned about an app's images.
type pinResult struct {
	manifest string
	pins     map[string]images.Pin
	statuses []ImageStatus
	reasons  []string
	warnings []string
}

// pinImages pins the moving tags of rendered, app's manifest as Git renders
// it, to the digests registries last served for them. A tag never seen before
// is resolved now; otherwise only CheckImages asks registries. A tag that
// can't be resolved keeps the digest the deployed release pins, so an
// unreachable registry never shows as a change.
func (c *Controller) pinImages(ctx context.Context, app *config.Application, rendered string, deployed *release.Release) pinResult {
	res := pinResult{manifest: rendered}
	name := app.Metadata.Name
	svcs, err := images.Services(rendered, config.ImageUpdatesFor(c.Boot.ImageUpdates.Enabled, app), config.LabelImageUpdates)
	if err != nil {
		res.warnings = append(res.warnings, "reading images: "+err.Error())
		return res
	}
	c.rememberImages(name, svcs)
	if !slices.ContainsFunc(svcs, func(s images.Service) bool { return s.Enabled }) {
		return res
	}
	cache, err := c.Store.ImageDigests()
	if err != nil {
		res.warnings = append(res.warnings, "reading image digests: "+err.Error())
		return res
	}
	var unseen []images.Ref
	for _, s := range svcs {
		if _, ok := cache[s.Ref.Name()]; s.Checked() && !ok {
			unseen = append(unseen, s.Ref)
		}
	}
	if len(unseen) > 0 {
		c.resolveImages(ctx, unseen)
		if cache, err = c.Store.ImageDigests(); err != nil {
			res.warnings = append(res.warnings, "reading image digests: "+err.Error())
			return res
		}
	}

	// What the deployed release pins, by service, for the image Git renders.
	var deployedPins map[string]string
	if deployed != nil {
		deployedPins = map[string]string{}
		if ds, err := images.Services(deployed.Manifest, true, ""); err == nil {
			for _, d := range ds {
				if d.Pinned {
					deployedPins[d.Name] = d.Image
				}
			}
		}
	}
	deployedDigest := func(s images.Service) string {
		img := deployedPins[s.Name]
		if after, ok := strings.CutPrefix(img, s.Image+"@"); ok && !s.Pinned {
			return after
		}
		if s.Pinned && img == s.Image {
			return s.Ref.Digest
		}
		return ""
	}

	res.pins = map[string]images.Pin{}
	for _, s := range svcs {
		if !s.Enabled {
			continue
		}
		st := ImageStatus{Service: s.Name, Image: s.Image, Note: s.Skip}
		if s.Skip != "" {
			res.statuses = append(res.statuses, st)
			continue
		}
		entry := cache[s.Ref.Name()]
		st.Deployed, st.Error = deployedDigest(s), entry.Error
		if s.Ref.Tag != "" {
			st.Ref, st.Latest, st.CheckedAt = s.Ref.Name(), entry.Digest, entry.CheckedAt
		}
		if s.Pinned {
			st.Pin, st.Digest = PinGit, s.Ref.Digest
			if st.Latest != "" && st.Latest != st.Digest {
				st.UpdateAvailable, st.Note = true, gitPinNote
			}
			res.statuses = append(res.statuses, st)
			continue
		}
		st.Digest = entry.Digest
		if st.Digest == "" {
			st.Digest = st.Deployed
		}
		if st.Digest == "" {
			st.Note = "not resolved yet"
			res.statuses = append(res.statuses, st)
			continue
		}
		st.Pin, st.Applicable = PinComposecd, true
		res.pins[s.Name] = images.Pin{Image: s.Image, Digest: st.Digest}
		if deployed != nil && st.Deployed != st.Digest {
			st.UpdateAvailable = st.Deployed != ""
			from := "unpinned"
			if st.Deployed != "" {
				from = images.Short(st.Deployed)
			}
			res.reasons = append(res.reasons, fmt.Sprintf("image %s (%s): %s → %s", s.Image, s.Name, from, images.Short(st.Digest)))
		}
		res.statuses = append(res.statuses, st)
	}
	if res.manifest, err = images.PinManifest(rendered, res.pins); err != nil {
		res.warnings = append(res.warnings, "pinning images: "+err.Error())
		res.manifest, res.pins, res.reasons = rendered, nil, nil
	}
	return res
}

// pinRelease pins rel, which Upgrade.Prepare saved as pending, as assess
// pinned the manifest it compared, and saves it again.
func pinRelease(ctx context.Context, cfg *action.Configuration, rel *release.Release, pins map[string]images.Pin) error {
	m, err := images.PinManifest(rel.Manifest, pins)
	if err != nil || m == rel.Manifest {
		return err
	}
	rel.Manifest = m
	if err := cfg.Compose.Config(ctx, compose.Project{Name: rel.Name, Manifest: []byte(m), Dir: rel.ProjectDir}); err != nil {
		return err
	}
	return cfg.Store.Save(rel)
}

// rememberImages records app's images, which CheckImages checks.
func (c *Controller) rememberImages(name string, svcs []images.Service) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.apps[name]
	if !ok {
		s = &appState{}
		c.apps[name] = s
	}
	s.images = svcs
}

// resolveImages asks registries for refs' digests and records them, or the
// error, keeping the last digest. It returns the refs whose digest, or whether
// their check failed, changed.
func (c *Controller) resolveImages(ctx context.Context, refs []images.Ref) map[string]bool {
	before, _ := c.Store.ImageDigests()
	changed := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, imageConcurrency)
	for _, r := range refs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v, err, _ := c.imageFlight.Do(r.Name(), func() (interface{}, error) {
				ctx, cancel := context.WithTimeout(ctx, imageTimeout)
				defer cancel()
				return c.ImageResolver.Digest(ctx, r)
			})
			now, prev := time.Now(), before[r.Name()]
			if err != nil {
				c.Log.Warn("checking an image", "image", r.Name(), "error", err)
				if serr := c.Store.SetImageError(r.Name(), err.Error(), now); serr != nil {
					c.Log.Error("recording an image check", "image", r.Name(), "error", serr)
				}
				if prev.Error == "" {
					mu.Lock()
					changed[r.Name()] = true
					mu.Unlock()
				}
				return
			}
			d := v.(string)
			if serr := c.Store.SetImageDigest(r.Name(), d, now); serr != nil {
				c.Log.Error("recording an image digest", "image", r.Name(), "error", serr)
				return
			}
			if prev.Digest != d || prev.Error != "" {
				if prev.Digest != d {
					c.Log.Info("image digest changed", "image", r.Name(), "from", prev.Digest, "to", d)
				}
				mu.Lock()
				changed[r.Name()] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return changed
}

// CheckImages asks registries for the digests of the tags app's services use
// (every app's, when app is ""), in the background, then reconciles the apps
// whose digests changed, and app itself.
func (c *Controller) CheckImages(app string) error {
	if app != "" {
		if _, err := c.application(app); err != nil {
			return err
		}
	}
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		c.checkImages(c.baseContext(), app)
	}()
	return nil
}

func (c *Controller) checkImages(ctx context.Context, only string) {
	c.mu.RLock()
	uses := map[string][]string{}
	var refs []images.Ref
	seen := map[string]bool{}
	for name, s := range c.apps {
		for _, svc := range s.images {
			if !svc.Checked() {
				continue
			}
			uses[svc.Ref.Name()] = append(uses[svc.Ref.Name()], name)
			if (only == "" || only == name) && !seen[svc.Ref.Name()] {
				seen[svc.Ref.Name()] = true
				refs = append(refs, svc.Ref)
			}
		}
	}
	c.mu.RUnlock()
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name() < refs[j].Name() })
	apps := map[string]bool{}
	if only != "" {
		apps[only] = true
	}
	for ref := range c.resolveImages(ctx, refs) {
		for _, a := range uses[ref] {
			apps[a] = true
		}
	}
	for name := range apps {
		if a, err := c.application(name); err == nil {
			c.reconcileAsync(a, false)
		}
	}
}

// runImageSchedule calls CheckImages for every app on composecd.yaml's
// imageUpdates.schedule, until ctx is done.
func (c *Controller) runImageSchedule(ctx context.Context) {
	sched, loc, err := imageSchedule(c.Boot.ImageUpdates)
	if err != nil {
		c.Log.Error("image updates are not scheduled", "error", err)
		return
	}
	for {
		next := sched.Next(time.Now().In(loc))
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			c.checkImages(ctx, "")
		}
	}
}

func imageSchedule(u config.ImageUpdates) (cron.Schedule, *time.Location, error) {
	s := u.Schedule
	if s == "" {
		s = config.DefaultImageSchedule
	}
	sched, err := config.ParseSchedule(s)
	if err != nil {
		return nil, nil, err
	}
	loc, err := time.LoadLocation(u.TimeZone)
	if err != nil {
		return nil, nil, err
	}
	return sched, loc, nil
}

// notifyImages sends an image-update event for each update st shows that prev
// didn't.
func (c *Controller) notifyImages(prev, st *AppStatus) {
	known := map[string]bool{}
	for _, im := range prev.Images {
		if im.UpdateAvailable {
			known[im.Service+"@"+im.Latest] = true
		}
	}
	for _, im := range st.Images {
		if !im.UpdateAvailable || known[im.Service+"@"+im.Latest] || im.Latest == "" {
			continue
		}
		msg := fmt.Sprintf("%s: a new image is available for %s (%s): %s", st.Name, im.Service, im.Image, images.Short(im.Latest))
		if !im.Applicable {
			msg += " (" + im.Note + ")"
		}
		c.notify(Event{Type: config.EventImageUpdate, App: st.Name, Host: st.Host, Message: msg})
	}
}
