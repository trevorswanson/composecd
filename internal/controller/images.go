package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/trevorswanson/composetpl/pkg/release"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/images"
	"github.com/trevorswanson/composecd/internal/source"
	"github.com/trevorswanson/composecd/internal/store"
)

// Image pins.
const (
	// PinComposecd is a moving tag composecd pinned to the digest its registry
	// last served.
	PinComposecd = "composecd"
	// PinGit is a digest written in Git, which composecd only changes by
	// committing to Git (writeBack: git).
	PinGit = "git"
)

// gitPinNote explains why an update to a digest pinned in Git can't be applied.
const gitPinNote = "the digest is pinned in Git; change it there to update, or set writeBack: git"

// gitPinHeldNote starts the note of an update to a digest pinned in Git that
// write-back can't write.
const gitPinHeldNote = "the digest is pinned in Git, and write-back can't change it: "

var (
	// imageTimeout bounds one registry check.
	imageTimeout = 30 * time.Second
	// tagListTimeout bounds listing a repository's tags, which takes a request
	// per page.
	tagListTimeout = 2 * time.Minute
	// imageConcurrency bounds the registry checks run at once.
	imageConcurrency = 4
)

// ImageStatus is one service's image and what its registry serves for its tag.
type ImageStatus struct {
	Service string `json:"service"`
	// Image is the image as Git renders it.
	Image string `json:"image"`
	// Ref is the tag checked, as registry/repository:tag: the Candidate's, if
	// the strategy picked one.
	Ref string `json:"ref,omitempty"`
	// Strategy is how the tag may move: digest, semver or newest-build.
	Strategy string `json:"strategy,omitempty"`
	// Constraint is the semver constraint the strategy applies.
	Constraint string `json:"constraint,omitempty"`
	// Tag is the tag deployed (Git's, before the first deployment).
	Tag string `json:"tag,omitempty"`
	// Candidate is the newer tag the strategy picked over Git's, which a sync
	// deploys.
	Candidate string `json:"candidate,omitempty"`
	// Pin is who pinned the desired digest: composecd (a moving tag) or git.
	Pin string `json:"pin,omitempty"`
	// Digest is the digest the desired manifest pins.
	Digest string `json:"digest,omitempty"`
	// Deployed is the digest the deployed release pins, if any.
	Deployed string `json:"deployed,omitempty"`
	// Latest is the digest the registry last served for the tag.
	Latest    string    `json:"latest,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	// TagsCheckedAt is when the strategy last listed the repository's tags.
	TagsCheckedAt time.Time `json:"tagsCheckedAt,omitzero"`
	// UpdateAvailable is true when the deployed image is behind its tag, or
	// the strategy picked a tag other than the deployed one.
	UpdateAvailable bool `json:"updateAvailable,omitempty"`
	// Applicable is true when a sync deploys the update; it is false for a
	// digest pinned in Git, which only a change in Git (by hand, or by
	// write-back) updates.
	Applicable bool   `json:"applicable,omitempty"`
	Note       string `json:"note,omitempty"`
	// Error is the last failed registry check.
	Error string `json:"error,omitempty"`
	// WriteBack is "git" when the strategy's picks are committed to Git
	// rather than applied in memory.
	WriteBack string `json:"writeBack,omitempty"`
	// Commit is the write-back commit that sets Candidate in Git; it deploys
	// once Git renders it.
	Commit string `json:"commit,omitempty"`
	// Held says why Candidate couldn't be written to Git. Nothing deploys it
	// until an admin applies it in memory or dismisses it.
	Held string `json:"held,omitempty"`
	// File and Key are where Git sets the tag, when write-back found them: a
	// path in the source (or root) repository and a dotted values key.
	File string `json:"file,omitempty"`
	Key  string `json:"key,omitempty"`
	// Decision is an admin's choice about a held candidate: memory (apply it
	// in memory) or dismiss, and DecidedBy who made it.
	Decision  string `json:"decision,omitempty"`
	DecidedBy string `json:"decidedBy,omitempty"`
	// Diverged is true while an update applied in memory makes the deployment
	// differ from what Git renders.
	Diverged bool `json:"diverged,omitempty"`
	// fingerprint and repo are what a decision about a held candidate
	// records: see imageFingerprint.
	fingerprint, repo string
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
// it, to the digests registries last served for them, and moves services
// whose strategy picked a newer tag to it. A tag never seen before, or a
// service whose strategy never picked, is checked now; otherwise only
// CheckImages asks registries. A tag that can't be resolved keeps the digest
// the deployed release pins, so an unreachable registry never shows as a
// change.
//
// With write-back to Git, a newer tag deploys once Git renders it, unless an
// admin applied it in memory: until then the service keeps Git's tag.
func (c *Controller) pinImages(ctx context.Context, app *config.Application, rendered string, deployed *release.Release, prep *source.Prepared) pinResult {
	res := pinResult{manifest: rendered}
	name := app.Metadata.Name
	svcs, err := images.Services(rendered, config.ImageUpdatesFor(c.Boot.ImageUpdates.Enabled, app),
		app.Spec.SyncPolicy.ImageUpdates.Policy(), config.LabelImageUpdates)
	if err != nil {
		res.warnings = append(res.warnings, "reading images: "+err.Error())
		return res
	}
	c.rememberImages(name, svcs)
	if !slices.ContainsFunc(svcs, func(s images.Service) bool { return s.Enabled }) {
		return res
	}

	// Strategies that never picked for the image Git renders now pick first.
	picks, err := c.Store.ImageTags(name)
	if err != nil {
		res.warnings = append(res.warnings, "reading image tags: "+err.Error())
		return res
	}
	var unpicked []tagJob
	for _, s := range svcs {
		if p, ok := picks[s.Name]; s.MovesTags() && (!ok || !pickedFor(p, s)) {
			unpicked = append(unpicked, tagJob{app: name, svc: s})
		}
	}
	if len(unpicked) > 0 {
		choices := c.chooseTags(ctx, unpicked)
		c.resolveImages(ctx, chosenRefs(choices))
		c.recordTags(choices)
		if picks, err = c.Store.ImageTags(name); err != nil {
			res.warnings = append(res.warnings, "reading image tags: "+err.Error())
			return res
		}
	}
	// With write-back, a candidate waits on Git, unless applied in memory.
	var wbs map[string]*wbView
	if app.Spec.SyncPolicy.ImageUpdates.WritesBack() {
		var warnings []string
		wbs, warnings = c.writeBackViews(ctx, app, prep, svcs, picks)
		res.warnings = append(res.warnings, warnings...)
	}
	waits := func(s images.Service) bool { v := wbs[s.Name]; return v != nil && !v.inMemory }
	// The images the deployed release runs, by service.
	var deployedImages map[string]string
	if deployed != nil {
		deployedImages = map[string]string{}
		if ds, err := images.Services(deployed.Manifest, true, images.Policy{}, ""); err == nil {
			for _, d := range ds {
				deployedImages[d.Name] = d.Image
			}
		}
	}
	// deployedDigest is the digest the deployed release pins for want, the
	// image s deploys.
	deployedDigest := func(s images.Service, want string) string {
		img := deployedImages[s.Name]
		if after, ok := strings.CutPrefix(img, want+"@"); ok && !s.Pinned {
			return after
		}
		if s.Pinned && img == s.Image {
			return s.Ref.Digest
		}
		return ""
	}
	// deployedTag is the tag the deployed release runs for s's repository.
	deployedTag := func(s images.Service) string {
		r, err := images.Parse(deployedImages[s.Name])
		if err != nil || r.Registry != s.Ref.Registry || r.Repository != s.Ref.Repository {
			return ""
		}
		return r.Tag
	}

	// keepsDeployed is true when s's strategy has no pick for the image and
	// policy Git has because the registry couldn't be asked, and the deployed
	// release runs a newer tag the policy still accepts: an outage must never
	// deploy an older image.
	keepsDeployed := func(s images.Service) bool {
		p, ok := picks[s.Name]
		if !s.MovesTags() || ok && pickedFor(p, s) && p.Tag != "" {
			return false
		}
		t := deployedTag(s)
		return t != "" && t != s.Ref.Tag && s.Policy.Accepts(s.Ref.Tag, t) &&
			deployedDigest(s, images.WithTag(s.Image, t)) != ""
	}
	// tagOf is the tag a service deploys: what its strategy picked, the
	// deployed one it keeps, or Git's.
	tagOf := func(s images.Service) string {
		if waits(s) {
			return s.Ref.Tag
		}
		if p, ok := picks[s.Name]; s.MovesTags() && ok && pickedFor(p, s) && p.Tag != "" {
			return p.Tag
		}
		if keepsDeployed(s) {
			return deployedTag(s)
		}
		return s.Ref.Tag
	}
	refOf := func(s images.Service) images.Ref {
		r := s.Ref
		r.Tag = tagOf(s)
		return r
	}

	cache, err := c.Store.ImageDigests()
	if err != nil {
		res.warnings = append(res.warnings, "reading image digests: "+err.Error())
		return res
	}
	var unseen []images.Ref
	for _, s := range svcs {
		if _, ok := cache[refOf(s).Name()]; s.Checked() && !ok && !keepsDeployed(s) {
			unseen = append(unseen, refOf(s))
		}
	}
	if len(unseen) > 0 {
		c.resolveImages(ctx, unseen)
		if cache, err = c.Store.ImageDigests(); err != nil {
			res.warnings = append(res.warnings, "reading image digests: "+err.Error())
			return res
		}
	}

	res.pins = map[string]images.Pin{}
	for _, s := range svcs {
		if !s.Enabled {
			continue
		}
		st := ImageStatus{Service: s.Name, Image: s.Image, Note: s.Skip, Strategy: s.Policy.Name()}
		if s.Skip != "" {
			res.statuses = append(res.statuses, st)
			continue
		}
		tag, pick := tagOf(s), picks[s.Name]
		want := s.Image
		if tag != s.Ref.Tag {
			want, st.Candidate = images.WithTag(s.Image, tag), tag
		}
		if s.Policy.Strategy == images.StrategySemver {
			st.Constraint, _ = s.Policy.ConstraintFor(s.Ref.Tag)
		}
		entry := cache[refOf(s).Name()]
		st.Deployed, st.Error = deployedDigest(s, want), entry.Error
		st.Tag = s.Ref.Tag
		if t := deployedTag(s); t != "" {
			st.Tag = t
		}
		if s.Ref.Tag != "" {
			st.Ref, st.Latest, st.CheckedAt = refOf(s).Name(), entry.Digest, entry.CheckedAt
		}
		if s.MovesTags() {
			st.TagsCheckedAt = pick.CheckedAt
			if pick.Error != "" {
				st.Error = pick.Error
			}
		}
		st.Note = s.Warning
		if s.MovesTags() && pick.Truncated {
			st.Note = "the registry has more tags than composecd reads; a newer one may be missed"
		}
		if s.Pinned {
			st.Pin, st.Digest = PinGit, s.Ref.Digest
			if st.Latest != "" && st.Latest != st.Digest {
				st.UpdateAvailable, st.Note = true, gitPinNote
				// With write-back, the new digest is committed where Git pins
				// it, and deploys from there.
				if app.Spec.SyncPolicy.ImageUpdates.WritesBack() {
					showPinnedWriteBack(&st, c.pinnedWriteBackView(ctx, app, prep, s, pinnedCandidate(s, st.Latest)))
				}
			}
			res.statuses = append(res.statuses, st)
			continue
		}
		st.Digest = entry.Digest
		if keepsDeployed(s) {
			st.Digest, st.Note = st.Deployed, "registry unreachable; keeping the deployed "+tag
		}
		if st.Digest == "" && st.Candidate != "" {
			st.Digest = pick.Digest
		}
		if st.Digest == "" {
			st.Digest = st.Deployed
		}
		if st.Digest == "" {
			st.Note = "not resolved yet"
			res.statuses = append(res.statuses, st)
			continue
		}
		st.Pin, st.Applicable = PinComposecd, true
		res.pins[s.Name] = images.Pin{Image: s.Image, Tag: st.Candidate, Digest: st.Digest}
		if deployed != nil {
			from := deployedTag(s)
			switch {
			case st.Candidate != "" && from != st.Candidate:
				// The strategy moves the tag.
				if from == "" {
					from = s.Ref.Tag
				}
				st.UpdateAvailable = true
				res.reasons = append(res.reasons, fmt.Sprintf("image %s (%s): %s → %s", images.Repo(s.Image), s.Name, from, st.Candidate))
			case st.Deployed != st.Digest:
				st.UpdateAvailable = st.Deployed != ""
				from := "unpinned"
				if st.Deployed != "" {
					from = images.Short(st.Deployed)
				}
				res.reasons = append(res.reasons, fmt.Sprintf("image %s (%s): %s → %s", want, s.Name, from, images.Short(st.Digest)))
			}
		}
		res.statuses = append(res.statuses, st)
	}
	for i := range res.statuses {
		if v := wbs[res.statuses[i].Service]; v != nil {
			showWriteBack(&res.statuses[i], v)
		}
	}
	if res.manifest, err = images.Override(rendered, res.pins); err != nil {
		res.warnings = append(res.warnings, "pinning images: "+err.Error())
		res.manifest, res.pins, res.reasons = rendered, nil, nil
	}
	return res
}

// showWriteBack shows on st how writing its candidate to Git stands.
func showWriteBack(st *ImageStatus, v *wbView) {
	st.WriteBack, st.fingerprint = config.WriteBackGit, v.fingerprint
	if d := v.decision; d != nil {
		st.Decision, st.DecidedBy, st.repo, st.File, st.Key = d.Choice, d.User, d.Repo, d.File, d.Key
	}
	gitTag := ""
	if r, err := images.Parse(st.Image); err == nil {
		gitTag = r.Tag
	}
	switch {
	case v.inMemory:
		st.Diverged = true
		st.Note = fmt.Sprintf("applied in memory by %s, diverging from Git, which says %s", v.decision.User, gitTag)
		return
	case v.decision != nil:
		st.Candidate, st.UpdateAvailable, st.Applicable = v.tag, false, false
		st.Note = fmt.Sprintf("%s dismissed by %s until a newer tag", v.tag, v.decision.User)
		if st.File != "" {
			st.Note += fmt.Sprintf("; to update, set %s in %s to %s", st.Key, st.File, v.tag)
		}
		return
	}
	st.Candidate, st.UpdateAvailable, st.Applicable = v.tag, true, false
	switch w := v.state; {
	case w == nil:
		st.Note = "writing " + v.tag + " to Git"
	case w.held != "":
		st.Held, st.repo, st.File, st.Key = w.held, w.repo, w.file, w.key
		st.Note = heldNote + w.held
	default:
		st.Commit, st.repo, st.File, st.Key = w.commit, w.repo, w.file, w.key
		st.Note = fmt.Sprintf("committed to Git as %s (%s in %s); it deploys once Git renders it", short(w.commit), w.key, w.file)
	}
}

// showPinnedWriteBack shows on st, an update to a digest pinned in Git, how
// writing it to Git stands. One that can't be written is refused, never held:
// composecd doesn't override a pin in memory.
func showPinnedWriteBack(st *ImageStatus, v *wbView) {
	st.WriteBack, st.fingerprint = config.WriteBackGit, v.fingerprint
	switch w := v.state; {
	case w == nil:
		st.Note = "writing the digest " + images.Short(st.Latest) + " to Git"
	case w.held != "":
		st.repo, st.File, st.Key = w.repo, w.file, w.key
		st.Note = gitPinHeldNote + w.held + "; change it there to update"
	default:
		st.Commit, st.repo, st.File, st.Key = w.commit, w.repo, w.file, w.key
		st.Note = fmt.Sprintf("committed to Git as %s (%s in %s); it deploys once Git renders it", short(w.commit), w.key, w.file)
	}
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

// ImageCheck is what a registry check found.
type ImageCheck struct {
	// Checked counts the tags asked about.
	Checked int `json:"checked"`
	// Updated are the tags whose digest moved since the last check.
	Updated []string `json:"updated,omitempty"`
	// Moved are the services a strategy moved to another tag, as
	// "app/service: tag".
	Moved []string `json:"moved,omitempty"`
	// Failed are why the tags a registry didn't answer for failed.
	Failed map[string]string `json:"failed,omitempty"`
	// Written are the tags committed to Git, as "app/service: tag", and Held
	// those that couldn't be, as "app/service: tag: why".
	Written []string `json:"written,omitempty"`
	Held    []string `json:"held,omitempty"`
}

// resolveImages asks registries for refs' digests and records them, or the
// error, keeping the last digest. It returns the refs whose digest, or whether
// their check failed, changed, and what the check found.
func (c *Controller) resolveImages(ctx context.Context, refs []images.Ref) (map[string]bool, ImageCheck) {
	before, _ := c.Store.ImageDigests()
	changed := map[string]bool{}
	check := ImageCheck{Checked: len(refs)}
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
				mu.Lock()
				if check.Failed == nil {
					check.Failed = map[string]string{}
				}
				check.Failed[r.Name()] = err.Error()
				mu.Unlock()
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
				mu.Lock()
				if prev.Digest != d {
					c.Log.Info("image digest changed", "image", r.Name(), "from", prev.Digest, "to", d)
				}
				// A first digest is news to no one.
				if prev.Digest != d && prev.Digest != "" {
					check.Updated = append(check.Updated, r.Name())
				}
				changed[r.Name()] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Strings(check.Updated)
	return changed, check
}

// CheckImages asks registries for the digests of the tags app's services use
// (every app's, when app is ""), in the background, then reconciles the apps
// whose digests changed, and app itself.
func (c *Controller) CheckImages(app string) error {
	_, err := c.startImageCheck(app)
	return err
}

// CheckImagesWait is CheckImages, returning what the registries said once they
// have all answered, or ctx's error if that comes first. The check carries on
// in the background either way.
func (c *Controller) CheckImagesWait(ctx context.Context, app string) (ImageCheck, error) {
	done, err := c.startImageCheck(app)
	if err != nil {
		return ImageCheck{}, err
	}
	select {
	case check := <-done:
		return check, nil
	case <-ctx.Done():
		return ImageCheck{}, ctx.Err()
	}
}

func (c *Controller) startImageCheck(app string) (<-chan ImageCheck, error) {
	if app != "" {
		if _, err := c.application(app); err != nil {
			return nil, err
		}
	}
	done := make(chan ImageCheck, 1)
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		done <- c.checkImages(c.baseContext(), app)
	}()
	return done, nil
}

func (c *Controller) checkImages(ctx context.Context, only string) ImageCheck {
	c.mu.RLock()
	uses := map[string][]string{}
	var refs []images.Ref
	var jobs []tagJob
	seen := map[string]bool{}
	for name, s := range c.apps {
		for _, svc := range s.images {
			if !svc.Checked() {
				continue
			}
			if svc.MovesTags() {
				if only == "" || only == name {
					jobs = append(jobs, tagJob{app: name, svc: svc})
				}
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
	choices := c.chooseTags(ctx, jobs)
	for _, ch := range choices {
		if ch.tag == "" {
			continue
		}
		r := ch.ref()
		uses[r.Name()] = append(uses[r.Name()], ch.app)
		if !seen[r.Name()] {
			seen[r.Name()] = true
			refs = append(refs, r)
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name() < refs[j].Name() })
	apps := map[string]bool{}
	if only != "" {
		apps[only] = true
	}
	changed, check := c.resolveImages(ctx, refs)
	for ref := range changed {
		for _, a := range uses[ref] {
			apps[a] = true
		}
	}
	moved, tagApps := c.recordTags(choices)
	check.Moved = moved
	for a := range tagApps {
		apps[a] = true
	}
	// Write the picks of apps with write-back to Git before they reconcile.
	var names []string
	if only != "" {
		names = []string{only}
	}
	wb := c.writeBack(ctx, names)
	check.Written, check.Held = wb.written, wb.held
	for a := range wb.apps {
		apps[a] = true
	}
	if wb.root {
		c.Refresh()
	}
	for _, ch := range choices {
		if ch.err != nil {
			if check.Failed == nil {
				check.Failed = map[string]string{}
			}
			check.Failed[ch.svc.Ref.Name()] = ch.err.Error()
		}
	}
	if len(apps) > 0 {
		c.bumpGeneration()
	}
	for name := range apps {
		if a, err := c.application(name); err == nil {
			c.reconcileAsync(a, false)
		}
	}
	return check
}

// tagJob is a service whose strategy picks its tag.
type tagJob struct {
	app string
	svc images.Service
}

// tagChoice is the tag a strategy picked for a service: Git's own when
// nothing is newer.
type tagChoice struct {
	tagJob
	tag       string
	truncated bool
	err       error
}

func (t tagChoice) ref() images.Ref {
	r := t.svc.Ref
	r.Tag = t.tag
	return r
}

// pickedFor is true when p was picked for s's image and policy as they are.
func pickedFor(p store.ImageTag, s images.Service) bool {
	return p.Image == s.Image && p.Policy == s.Policy.Fingerprint()
}

// chosenRefs are the tags choices picked.
func chosenRefs(choices []tagChoice) []images.Ref {
	var out []images.Ref
	seen := map[string]bool{}
	for _, ch := range choices {
		if r := ch.ref(); ch.tag != "" && !seen[r.Name()] {
			seen[r.Name()] = true
			out = append(out, r)
		}
	}
	return out
}

// chooseTags lists the tags of jobs' repositories, each once, and lets each
// service's strategy pick one.
func (c *Controller) chooseTags(ctx context.Context, jobs []tagJob) []tagChoice {
	type list struct {
		tags      []string
		truncated bool
		err       error
	}
	repo := func(r images.Ref) string { return r.Registry + "/" + r.Repository }
	repos := map[string]images.Ref{}
	for _, j := range jobs {
		repos[repo(j.svc.Ref)] = j.svc.Ref
	}
	lists := map[string]list{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, imageConcurrency)
	for key, r := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v, err, _ := c.imageFlight.Do("tags:"+key, func() (interface{}, error) {
				ctx, cancel := context.WithTimeout(ctx, tagListTimeout)
				defer cancel()
				tags, truncated, err := c.ImageResolver.Tags(ctx, r)
				return list{tags: tags, truncated: truncated}, err
			})
			l := list{err: err}
			if err != nil {
				c.Log.Warn("listing an image's tags", "repository", key, "error", err)
			} else {
				l = v.(list)
			}
			mu.Lock()
			lists[key] = l
			mu.Unlock()
		}()
	}
	wg.Wait()

	out := make([]tagChoice, len(jobs))
	for i, j := range jobs {
		l := lists[repo(j.svc.Ref)]
		ch := tagChoice{tagJob: j, truncated: l.truncated, err: l.err}
		if ch.err == nil {
			switch j.svc.Policy.Strategy {
			case images.StrategySemver:
				ch.tag, ch.err = j.svc.Policy.PickSemver(j.svc.Ref.Tag, l.tags)
			case images.StrategyNewestBuild:
				ch.tag, ch.err = c.newestBuild(ctx, j.svc, l.tags)
			}
		}
		out[i] = ch
	}
	return out
}

// newestBuild picks, among s's candidate tags, the one whose image was built
// last; ties go to the newest by name. Build times are cached by digest.
func (c *Controller) newestBuild(ctx context.Context, s images.Service, tags []string) (string, error) {
	cands := s.Policy.NewestCandidates(s.Ref.Tag, tags)
	built := make([]time.Time, len(cands))
	errs := make([]error, len(cands))
	var wg sync.WaitGroup
	sem := make(chan struct{}, imageConcurrency)
	for i, t := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(ctx, imageTimeout)
			defer cancel()
			r := s.Ref
			r.Tag = t
			d, err := c.ImageResolver.Digest(ctx, r)
			if err != nil {
				errs[i] = err
				return
			}
			if v, ok := c.buildTimes.Load(d); ok {
				built[i] = v.(time.Time)
				return
			}
			r.Digest = d
			at, err := c.ImageResolver.Created(ctx, r)
			if err != nil {
				errs[i] = err
				return
			}
			c.buildTimes.Store(d, at)
			built[i] = at
		}()
	}
	wg.Wait()
	best := -1
	for i := range cands {
		if errs[i] != nil {
			c.Log.Warn("reading when an image was built", "image", s.Ref.Registry+"/"+s.Ref.Repository+":"+cands[i], "error", errs[i])
			continue
		}
		if best < 0 || built[i].After(built[best]) {
			best = i
		}
	}
	if best < 0 {
		return "", fmt.Errorf("no candidate tag could be read: %w", errors.Join(errs...))
	}
	return cands[best], nil
}

// recordTags stores choices, once resolveImages has resolved their tags. A
// failed choice keeps the last pick for the same image and policy. It returns
// the services moved to a new tag, as "app/service: tag", and the apps whose
// pick changed.
func (c *Controller) recordTags(choices []tagChoice) ([]string, map[string]bool) {
	apps := map[string]bool{}
	var moved []string
	if len(choices) == 0 {
		return nil, apps
	}
	cache, err := c.Store.ImageDigests()
	if err != nil {
		c.Log.Error("reading image digests", "error", err)
		return nil, apps
	}
	prevs := map[string]map[string]store.ImageTag{}
	now := time.Now()
	for _, ch := range choices {
		if prevs[ch.app] == nil {
			if prevs[ch.app], err = c.Store.ImageTags(ch.app); err != nil {
				c.Log.Error("reading image tags", "app", ch.app, "error", err)
				continue
			}
		}
		prev, ok := prevs[ch.app][ch.svc.Name]
		same := ok && pickedFor(prev, ch.svc)
		t := store.ImageTag{App: ch.app, Service: ch.svc.Name, Image: ch.svc.Image, Policy: ch.svc.Policy.Fingerprint(),
			CheckedAt: now, Truncated: ch.truncated}
		if same {
			t.Tag, t.Digest, t.PickedAt = prev.Tag, prev.Digest, prev.PickedAt
		}
		switch entry := cache[ch.ref().Name()]; {
		case ch.err != nil:
			t.Error = ch.err.Error()
		case entry.Digest == "":
			t.Error = entry.Error
			if t.Error == "" {
				t.Error = "resolving " + ch.ref().Name() + " failed"
			}
		default:
			if t.Tag != ch.tag {
				t.PickedAt = now
				if ch.tag != ch.svc.Ref.Tag {
					moved = append(moved, fmt.Sprintf("%s/%s: %s", ch.app, ch.svc.Name, ch.tag))
					c.Log.Info("image tag picked", "app", ch.app, "service", ch.svc.Name, "from", t.Tag, "to", ch.tag)
				}
			}
			t.Tag, t.Digest = ch.tag, entry.Digest
		}
		if err := c.Store.SetImageTag(t); err != nil {
			c.Log.Error("recording an image tag", "app", ch.app, "service", ch.svc.Name, "error", err)
			continue
		}
		if !same || t.Tag != prev.Tag || t.Digest != prev.Digest || (t.Error == "") != (prev.Error == "") || t.Truncated != prev.Truncated {
			apps[ch.app] = true
		}
	}
	sort.Strings(moved)
	return moved, apps
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
	key := func(im ImageStatus) string { return im.Service + "@" + im.Candidate + "@" + im.Latest }
	known := map[string]bool{}
	for _, im := range prev.Images {
		if im.UpdateAvailable {
			known[key(im)] = true
		}
	}
	held := map[string]bool{}
	for _, im := range prev.Images {
		if im.Held != "" {
			held[im.Service+"@"+im.Candidate] = true
		}
	}
	for _, im := range st.Images {
		if im.Held != "" && !held[im.Service+"@"+im.Candidate] {
			msg := fmt.Sprintf("%s: %s (%s): %s can't be written to Git: %s. Nothing deploys it until an admin applies it in memory or dismisses it",
				st.Name, images.Repo(im.Image), im.Service, im.Candidate, im.Held)
			c.notify(Event{Type: config.EventWriteBackBlocked, App: st.Name, Host: st.Host, Message: msg})
		}
		if !im.UpdateAvailable || known[key(im)] || im.Latest == "" || im.Held != "" {
			continue
		}
		var msg string
		if im.Candidate != "" && im.Candidate != im.Tag {
			policy := im.Strategy
			if im.Constraint != "" {
				policy += " " + im.Constraint
			}
			msg = fmt.Sprintf("%s: %s (%s): %s available (%s)", st.Name, images.Repo(im.Image), im.Service, im.Candidate, policy)
		} else {
			msg = fmt.Sprintf("%s: a new image is available for %s (%s): %s", st.Name, im.Service, im.Image, images.Short(im.Latest))
		}
		if !im.Applicable {
			msg += " (" + im.Note + ")"
		}
		c.notify(Event{Type: config.EventImageUpdate, App: st.Name, Host: st.Host, Message: msg})
	}
}
