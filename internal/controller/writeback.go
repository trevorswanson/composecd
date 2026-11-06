package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/trevorswanson/composetpl/pkg/values"
	"gopkg.in/yaml.v3"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/gitcache"
	"github.com/trevorswanson/composecd/internal/images"
	"github.com/trevorswanson/composecd/internal/sops"
	"github.com/trevorswanson/composecd/internal/source"
	"github.com/trevorswanson/composecd/internal/store"
	"github.com/trevorswanson/composecd/internal/yamledit"
)

// TrailerImageUpdate ends the message of the commits image write-back makes,
// so webhooks for them can be told apart.
const TrailerImageUpdate = "Composecd-Image-Update: true"

// writeBackRetries is how often a push the remote rejected because the branch
// moved on is retried, each time against the new tip.
var writeBackRetries = 3

// ErrNotHeld is a decision about a service whose image update isn't held.
var ErrNotHeld = errors.New("no image update is held for this service")

// wbState is how the last write-back of one service's candidate went.
type wbState struct {
	// candidate is the tag written or held, and image Git's image then.
	candidate string
	image     string
	// fingerprint is the service's source fingerprint when pinImages first
	// saw the state; a changed source makes the state stale.
	fingerprint string
	// held is why the candidate couldn't be written ("" once committed).
	held string
	// repo, file and key are where Git sets the tag, when tracing found it.
	repo, file, key string
	// commit is the commit written.
	commit string
}

// wbJob is one service's candidate to write to Git, and what planning found.
type wbJob struct {
	app *config.Application
	svc images.Service
	// tag is the tag to write, or tag@digest for an image pinned in Git: the
	// image becomes images.WithTag(svc.Image, tag) either way.
	tag string

	held            string
	repo, file, key string
	// feeds are the services the key sets, as tracing found them.
	feeds  []string
	edit   *wbEdit
	commit string
	// deployed is true when the commit went to the branch the app deploys.
	deployed bool
}

// wbEdit is the change one job makes to one file.
type wbEdit struct {
	repo, branch, parent string
	// dir is the checkout of parent.
	dir  string
	path string
	// app names the Application document of a root file to edit, for inline
	// values; "" edits a values file.
	app   string
	key   []string
	value string
	// readers are other Applications reading path, which must make the same
	// edit for it to be made; fallback is where this app's key goes otherwise.
	readers  []string
	fallback *wbEdit
}

func (e *wbEdit) same(o *wbEdit) bool {
	return e.repo == o.repo && e.path == o.path && e.app == o.app && slices.Equal(e.key, o.key)
}

// repoTip is the checkout of the tip of the branch write-back pushes to.
type repoTip struct {
	url, branch, sha, dir string
	deployed              bool
}

// heldNote is how a held update shows on its image.
const heldNote = "Held: "

// wbResult is what a write-back did.
type wbResult struct {
	// apps are the apps tried; root is set when a commit changed the root's
	// Applications.
	apps map[string]bool
	root bool
	// written and held are as in ImageCheck.
	written, held []string
}

// writeBack writes the candidates of names' services (every app's when names
// is nil) with writeBack: git to Git: one commit per repository, retried
// against the new tip when someone pushed meanwhile. Each candidate ends up
// committed or held.
func (c *Controller) writeBack(ctx context.Context, names []string) wbResult {
	c.wbRun.Lock()
	defer c.wbRun.Unlock()
	jobs := c.writeBackJobs(names)
	res := wbResult{apps: map[string]bool{}}
	if len(jobs) == 0 {
		return res
	}
	c.mu.RLock()
	objs := c.objects
	c.mu.RUnlock()
	pending := jobs
	for attempt := 0; len(pending) > 0; attempt++ {
		var releases []func()
		c.planWriteBack(ctx, objs, pending, &releases)
		rejected := c.commitWriteBack(ctx, pending, attempt < writeBackRetries)
		for _, r := range releases {
			r()
		}
		fetched := map[string]bool{}
		for _, j := range rejected {
			if !fetched[j.edit.repo] {
				fetched[j.edit.repo] = true
				if err := c.Git.Fetch(ctx, j.edit.repo); err != nil {
					c.Log.Warn("fetching after a rejected push", "repo", j.edit.repo, "error", err)
				}
			}
			j.held, j.repo, j.file, j.key, j.edit = "", "", "", "", nil
		}
		pending = rejected
	}
	for _, j := range jobs {
		name := j.app.Metadata.Name
		res.apps[name] = true
		st := &wbState{candidate: j.tag, image: j.svc.Image, held: j.held, repo: j.repo, file: j.file, key: j.key, commit: j.commit}
		c.setWBState(name, j.svc.Name, st)
		if j.held != "" {
			res.held = append(res.held, fmt.Sprintf("%s/%s: %s: %s", name, j.svc.Name, j.tag, j.held))
			c.Log.Warn("image update held", "app", name, "service", j.svc.Name, "tag", j.tag, "reason", j.held)
			continue
		}
		res.written = append(res.written, fmt.Sprintf("%s/%s: %s", name, j.svc.Name, j.tag))
		c.Log.Info("image update written to Git", "app", name, "service", j.svc.Name, "tag", j.tag, "commit", j.commit)
		if j.edit != nil && j.edit.app != "" {
			res.root = true
		}
		// The commit renders the new image, which the pick then belongs to,
		// so its arrival doesn't list the registry's tags again.
		if j.deployed {
			if picks, err := c.Store.ImageTags(name); err == nil {
				if p, ok := picks[j.svc.Name]; ok && p.Tag == j.tag {
					p.Image = images.WithTag(j.svc.Image, j.tag)
					if err := c.Store.SetImageTag(p); err != nil {
						c.Log.Error("recording an image tag", "app", name, "service", j.svc.Name, "error", err)
					}
				}
			}
		}
	}
	return res
}

// writeBackJobs lists the services of names with a candidate to write.
func (c *Controller) writeBackJobs(names []string) []*wbJob {
	c.mu.RLock()
	objs := c.objects
	svcs := map[string][]images.Service{}
	for name, s := range c.apps {
		svcs[name] = s.images
	}
	c.mu.RUnlock()
	if objs == nil {
		return nil
	}
	if names == nil {
		for _, a := range objs.Applications {
			names = append(names, a.Metadata.Name)
		}
	}
	sort.Strings(names)
	var jobs []*wbJob
	for _, name := range names {
		app := objs.Application(name)
		if app == nil || !app.Spec.SyncPolicy.ImageUpdates.WritesBack() {
			continue
		}
		picks, err := c.Store.ImageTags(name)
		if err != nil {
			c.Log.Error("reading image tags", "app", name, "error", err)
			continue
		}
		decisions, err := c.Store.ImageDecisions(name)
		if err != nil {
			c.Log.Error("reading image decisions", "app", name, "error", err)
			continue
		}
		digests, err := c.Store.ImageDigests()
		if err != nil {
			c.Log.Error("reading image digests", "error", err)
			continue
		}
		for _, s := range svcs[name] {
			if s.Pinned {
				if cand := pinnedCandidate(s, digests[s.Ref.Name()].Digest); cand != "" {
					jobs = append(jobs, &wbJob{app: app, svc: s, tag: cand})
				}
				continue
			}
			p, ok := picks[s.Name]
			if !s.MovesTags() || !ok || !pickedFor(p, s) || p.Tag == "" || p.Tag == s.Ref.Tag {
				continue
			}
			if d, ok := decisions[s.Name]; ok && (d.Choice == store.DecisionMemory || d.Candidate == p.Tag) {
				continue
			}
			jobs = append(jobs, &wbJob{app: app, svc: s, tag: p.Tag})
		}
	}
	return jobs
}

// pinnedCandidate is what write-back writes for s, an image pinned in Git, when
// its tag now resolves to latest: tag@latest, or "" when there is nothing to
// write. A digest without a tag (redis@sha256:…) is never checked.
func pinnedCandidate(s images.Service, latest string) string {
	if !s.Pinned || !s.Checked() || latest == "" || latest == s.Ref.Digest {
		return ""
	}
	return s.Ref.Tag + "@" + latest
}

// planWriteBack traces each job's tag to the file and key that set it, and
// plans the edit, or holds the job.
func (c *Controller) planWriteBack(ctx context.Context, objs *config.Objects, jobs []*wbJob, releases *[]func()) {
	tips := map[string]*repoTip{}
	tipErrs := map[string]error{}
	tip := func(url, rev string) (*repoTip, error) {
		k := url + "\x00" + rev
		if t, ok := tips[k]; ok || tipErrs[k] != nil {
			return t, tipErrs[k]
		}
		t, release, err := c.writeTip(ctx, url, rev)
		if err != nil {
			tipErrs[k] = err
			return nil, err
		}
		*releases = append(*releases, release)
		tips[k] = t
		return t, nil
	}
	var byApp [][]*wbJob
	for _, j := range jobs {
		if n := len(byApp); n > 0 && byApp[n-1][0].app == j.app {
			byApp[n-1] = append(byApp[n-1], j)
		} else {
			byApp = append(byApp, []*wbJob{j})
		}
	}
	for _, appJobs := range byApp {
		c.planApp(ctx, objs, appJobs, tip)
	}

	// A key feeding several services is edited only if they all move to the
	// same tag; a file other apps read only if they all make the same edit.
	for changed := true; changed; {
		changed = false
		for _, j := range jobs {
			if j.held != "" || j.edit == nil {
				continue
			}
			if msg := sharedHold(j, jobs); msg != "" {
				j.held, changed = msg, true
				continue
			}
			if len(j.edit.readers) == 0 {
				continue
			}
			var missing []string
			for _, r := range j.edit.readers {
				if !slices.ContainsFunc(jobs, func(o *wbJob) bool {
					return o.app.Metadata.Name == r && o.held == "" && o.edit != nil && o.edit.same(j.edit) && o.edit.value == j.edit.value
				}) {
					missing = append(missing, r)
				}
			}
			if len(missing) == 0 {
				continue
			}
			changed = true
			if j.edit.fallback != nil {
				j.edit = j.edit.fallback
				j.repo, j.file, j.key = j.edit.repo, j.edit.path, strings.Join(j.edit.key, ".")
				continue
			}
			j.held = fmt.Sprintf("the tag is in %s, which %s also read, and %s has no values file of its own after it to override it in; add one to valueFiles",
				j.edit.path, strings.Join(missing, ", "), j.app.Metadata.Name)
		}
	}
}

// sharedHold says why j's key can't be edited because it sets other services'
// images that don't move with it, or "".
func sharedHold(j *wbJob, jobs []*wbJob) string {
	for _, f := range j.feeds {
		if f == j.svc.Name {
			continue
		}
		ok := slices.ContainsFunc(jobs, func(o *wbJob) bool {
			return o.app == j.app && o.svc.Name == f && o.held == "" && o.edit != nil && o.edit.same(j.edit) && o.tag == j.tag
		})
		if !ok {
			return fmt.Sprintf("%s also sets the image of %s, which doesn't move to %s with it", j.key, f, j.tag)
		}
	}
	return ""
}

// writeTip checks out the tip of the branch write-back pushes to for a source
// at url and rev: imageUpdates.writeBack.branch, or rev, which must then be a
// branch.
func (c *Controller) writeTip(ctx context.Context, url, rev string) (*repoTip, func(), error) {
	branch := c.Boot.ImageUpdates.WriteBack.Branch
	var deployed bool
	if b, err := c.Git.Branch(ctx, url, rev); err == nil {
		deployed = branch == "" || branch == b
		if branch == "" {
			branch = b
		}
	} else if branch == "" {
		return nil, nil, fmt.Errorf("%v: write-back pushes to the targetRevision, so it must be a branch, or set imageUpdates.writeBack.branch in composecd.yaml", err)
	}
	sha, err := c.Git.Resolve(ctx, url, branch)
	if err != nil && !deployed {
		// A branch for review that doesn't exist yet starts at the deployed
		// revision.
		sha, err = c.Git.Resolve(ctx, url, rev)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("branch %s: %w", branch, err)
	}
	release := c.useCheckout(url, sha)
	dir, err := c.Git.Checkout(ctx, url, sha)
	if err != nil {
		release()
		return nil, nil, err
	}
	return &repoTip{url: url, branch: branch, sha: sha, dir: dir, deployed: deployed}, release, nil
}

// planApp traces and plans the jobs of one Application.
func (c *Controller) planApp(ctx context.Context, objs *config.Objects, jobs []*wbJob, tip func(url, rev string) (*repoTip, error)) {
	app := jobs[0].app
	name := app.Metadata.Name
	hold := func(reason string) {
		for _, j := range jobs {
			if j.held == "" {
				j.held = reason
			}
		}
	}
	h := objs.Host(app.Spec.Destination.Host)
	if h == nil {
		hold(fmt.Sprintf("host %q is not defined", app.Spec.Destination.Host))
		return
	}
	s := app.Spec.Source
	var src *repoTip
	if url, rev := s.GitRepository(); url != "" {
		var err error
		if src, err = tip(url, rev); err != nil {
			hold(err.Error())
			return
		}
	}
	dir, sha := "", ""
	if src != nil {
		dir, sha = src.dir, src.sha
	}
	prep, err := source.Prepare(ctx, app, h, dir, sha, c.Registry, c.Boot.SOPSAgeKeyFile, c.infisicalClient())
	if err != nil {
		hold("reading the source: " + err.Error())
		return
	}
	tr, err := newTracer(ctx, name, prep.ChartRef, prep.Version, c.Registry, prep.Lookup, prep.Values)
	if err != nil {
		hold("loading the chart: " + prep.RelativeErr(err).Error())
		return
	}
	base, err := tr.images(prep.Values, "")
	if err != nil {
		hold("rendering offline: " + prep.RelativeErr(err).Error())
		return
	}
	for _, j := range jobs {
		svc := j.svc.Name
		what := "tag"
		if j.svc.Pinned {
			what = "tag and digest"
		}
		if src != nil && base[svc] == images.WithTag(j.svc.Image, j.tag) {
			// The branch says so already (one other than the deployed one).
			j.commit = src.sha
			continue
		}
		if base[svc] != j.svc.Image {
			j.held = fmt.Sprintf("rendered offline, %s's image is %s, not %s, so composecd can't trace its %s", svc, orNothing(base[svc]), j.svc.Image, what)
			continue
		}
		srcs, err := tr.trace(svc, base, j.svc.TagKey)
		if err != nil {
			j.held = "tracing the tag: " + prep.RelativeErr(err).Error()
			continue
		}
		want := images.WithTag(j.svc.Image, j.tag)
		found := forImage(srcs, want)
		var keys []string
		for _, t := range found {
			keys = append(keys, t.Key)
		}
		switch {
		case len(found) == 0 && j.svc.TagKey != "":
			j.held = fmt.Sprintf("the %s label names %s, which doesn't set the %s of %s", tagKeyLabel, j.svc.TagKey, what, svc)
			continue
		case len(found) == 0 && strings.Contains(j.svc.Image, tr.chart.Metadata.AppVersion) && tr.dir == "" && tr.chart.Metadata.AppVersion != "":
			j.held = "the tag looks like the OCI chart's appVersion, which only a new chart version changes"
			continue
		case len(found) == 0:
			j.held = fmt.Sprintf("no values key sets the %s of %s (it may be written in a template); move it into the values, or set the %s label",
				what, svc, tagKeyLabel)
			continue
		case len(found) > 1:
			j.held = fmt.Sprintf("more than one key sets the %s of %s (%s); set the %s label to the one to edit", what, svc, strings.Join(keys, ", "), tagKeyLabel)
			continue
		}
		t := found[0]
		value, _ := t.valueFor(want)
		j.key, j.feeds = t.Key, t.feeds
		// The edit must render the new image, for every service the key sets.
		var got map[string]string
		if t.Key == KeyAppVersion {
			got, err = tr.images(prep.Values, value)
		} else {
			got, err = tr.images(values.Merge(prep.Values, nested(yamledit.Key(t.Key), value)), "")
		}
		if err != nil {
			j.held = fmt.Sprintf("setting %s to %s doesn't render: %v", t.Key, value, prep.RelativeErr(err))
			continue
		}
		for _, f := range t.feeds {
			if w := images.WithTag(base[f], j.tag); got[f] != w {
				j.held = fmt.Sprintf("setting %s to %s renders %s for %s, not %s; set the %s label", t.Key, value, got[f], f, w, tagKeyLabel)
				break
			}
		}
		if j.held != "" {
			continue
		}
		c.locate(objs, j, prep, tr, src, value, tip)
	}
}

func orNothing(s string) string {
	if s == "" {
		return "missing"
	}
	return s
}

// layer is one source of an Application's values, in the order they merge.
type layer struct {
	// kind is chart (a Git chart's values.yaml), packaged (an OCI chart's),
	// file, sops, infisical or inline.
	kind string
	// path is the file's path in the source repository.
	path  string
	index int
	root  *yaml.Node
}

func (l layer) sets(key []string, packaged, inline map[string]interface{}, secretPaths [][]string) bool {
	switch l.kind {
	case "packaged":
		return lookupKey(packaged, key) != nil
	case "inline":
		return lookupKey(inline, key) != nil
	case "infisical":
		return slices.ContainsFunc(secretPaths, func(p []string) bool { return slices.Equal(p, key) })
	}
	return l.root != nil && yamledit.Find(l.root, key) != nil
}

// appFiles are the plain values files in its source repository an
// Application reads, as repository paths: a Git chart's values.yaml, then its
// valueFiles.
func appFiles(a *config.Application) []string {
	s := a.Spec.Source
	var out []string
	if !s.IsOCI() {
		out = append(out, path.Join(s.Path, "values.yaml"))
		for _, vf := range s.ValueFiles {
			out = append(out, path.Join(s.Path, vf))
		}
	} else if s.ValuesFrom != nil {
		for _, vf := range s.ValueFiles {
			out = append(out, path.Clean(vf))
		}
	}
	return out
}

// readers are the Applications other than app that read file p of the
// repository at url.
func readers(objs *config.Objects, app *config.Application, url, p string) []string {
	var out []string
	for _, a := range objs.Applications {
		if a == app || a.Metadata.Name == app.Metadata.Name || NormalizeRepoURL(sourceRepo(a)) != NormalizeRepoURL(url) {
			continue
		}
		if slices.Contains(appFiles(a), p) {
			out = append(out, a.Metadata.Name)
		}
	}
	sort.Strings(out)
	return out
}

// locate finds the file j's key is set in, last, and plans the edit there,
// or holds j when that file can't be written.
func (c *Controller) locate(objs *config.Objects, j *wbJob, prep *source.Prepared, tr *tracer, src *repoTip, value string,
	tip func(url, rev string) (*repoTip, error)) {
	app, s := j.app, j.app.Spec.Source
	edit := func(t *repoTip, p string, key []string) *wbEdit {
		return &wbEdit{repo: t.url, branch: t.branch, parent: t.sha, dir: t.dir, path: p, key: key, value: value}
	}
	use := func(e *wbEdit) {
		j.edit, j.repo, j.file, j.key = e, e.repo, e.path, strings.Join(e.key, ".")
		if e.app != "" {
			j.key = strings.TrimPrefix(j.key, "spec.source.values.")
		}
		j.deployed = c.deploysBranch(e)
	}
	if j.key == KeyAppVersion {
		if src == nil || s.IsOCI() {
			j.held = "the tag is the OCI chart's appVersion, which only a new chart version changes"
			return
		}
		e := edit(src, path.Join(s.Path, "Chart.yaml"), []string{"appVersion"})
		for _, a := range objs.Applications {
			if a.Metadata.Name != app.Metadata.Name && !a.Spec.Source.IsOCI() && path.Clean(a.Spec.Source.Path) == path.Clean(s.Path) &&
				NormalizeRepoURL(sourceRepo(a)) == NormalizeRepoURL(src.url) {
				e.readers = append(e.readers, a.Metadata.Name)
			}
		}
		use(e)
		return
	}
	key := yamledit.Key(j.key)

	// The layers, lowest first.
	var layers []layer
	read := func(kind, p string, index int) layer {
		l := layer{kind: kind, path: p, index: index}
		if src == nil {
			return l
		}
		data, err := os.ReadFile(filepath.Join(src.dir, filepath.FromSlash(p)))
		if err != nil {
			return l
		}
		if kind == "file" {
			if enc, _ := sops.Encrypted(p, data); enc {
				l.kind = "sops"
			}
		}
		if root, err := yamledit.Root(data); err == nil {
			l.root = root
		}
		return l
	}
	if s.IsOCI() {
		layers = append(layers, layer{kind: "packaged", index: -1})
	} else {
		layers = append(layers, read("chart", path.Join(s.Path, "values.yaml"), -1))
	}
	files := appFiles(app)
	if !s.IsOCI() {
		files = files[1:]
	}
	for i, p := range files {
		layers = append(layers, read("file", p, i))
	}
	var sopsPaths [][]string
	for _, l := range layers {
		if l.kind == "sops" && l.root != nil {
			sopsPaths = append(sopsPaths, secretKeys(l.root, nil)...)
		}
	}
	var infisicalPaths [][]string
	for _, p := range prep.SecretPaths {
		if !slices.ContainsFunc(sopsPaths, func(q []string) bool { return slices.Equal(p, q) }) {
			infisicalPaths = append(infisicalPaths, p)
		}
	}
	if s.Infisical != nil {
		layers = append(layers, layer{kind: "infisical", index: len(files)})
	}
	layers = append(layers, layer{kind: "inline", index: len(files) + 1})
	var win *layer
	for i := range layers {
		if layers[i].sets(key, tr.chart.Values, s.Values, infisicalPaths) {
			win = &layers[i]
		}
	}
	// fallback is the app's last values file of its own after the winning
	// layer, which overrides it for this app only.
	fallback := func(after int) *wbEdit {
		for i := len(layers) - 1; i >= 0; i-- {
			l := layers[i]
			if l.kind == "file" && l.index > after && l.root != nil && len(readers(objs, app, src.url, l.path)) == 0 {
				return edit(src, l.path, key)
			}
		}
		return nil
	}
	switch {
	case win == nil:
		j.held = fmt.Sprintf("can't find which values file sets %s", j.key)
	case win.kind == "sops":
		j.held = fmt.Sprintf("the tag is in %s, which SOPS encrypts", win.path)
		j.repo, j.file = src.url, win.path
	case win.kind == "infisical":
		j.held = "the tag comes from Infisical (source.infisical), not Git"
	case win.kind == "inline" && app.Metadata.Labels[LabelApplicationSet] != "":
		j.held = fmt.Sprintf("the tag is in the inline values of ApplicationSet %s's template, which composecd doesn't edit",
			app.Metadata.Labels[LabelApplicationSet])
	case win.kind == "inline":
		root := c.Boot.Root
		t, err := tip(root.RepoURL, root.TargetRevision)
		if err != nil {
			j.held = err.Error()
			return
		}
		file, _, _ := strings.Cut(app.Origin, ":")
		e := edit(t, path.Join(root.Path, file), append([]string{"spec", "source", "values"}, key...))
		e.app = app.Metadata.Name
		use(e)
	case win.kind == "packaged":
		var e *wbEdit
		if src != nil {
			e = fallback(-1)
		}
		if e == nil {
			j.held = fmt.Sprintf("the tag is in the OCI chart's packaged values, and %s has no Git values file of its own to override it in; add one to valueFiles",
				app.Metadata.Name)
			return
		}
		use(e)
	default:
		e := edit(src, win.path, key)
		if e.readers = readers(objs, app, src.url, win.path); len(e.readers) > 0 {
			e.fallback = fallback(win.index)
		}
		use(e)
	}
}

// secretKeys are the key paths of a SOPS file's values, leaving out its sops
// metadata.
func secretKeys(n *yaml.Node, prefix []string) [][]string {
	var out [][]string
	if n.Kind != yaml.MappingNode {
		return [][]string{prefix}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if len(prefix) == 0 && k == "sops" {
			continue
		}
		out = append(out, secretKeys(n.Content[i+1], append(append([]string{}, prefix...), k))...)
	}
	return out
}

// deploysBranch is true when e's branch is the one its app deploys from.
func (c *Controller) deploysBranch(e *wbEdit) bool {
	b := c.Boot.ImageUpdates.WriteBack.Branch
	return b == "" || b == e.branch
}

// commitWriteBack applies the planned edits, one commit per repository. Jobs
// whose push was rejected because the branch moved on are returned for a
// retry when retry is set, and held otherwise.
func (c *Controller) commitWriteBack(ctx context.Context, jobs []*wbJob, retry bool) []*wbJob {
	type repoKey struct{ url, branch, parent string }
	byRepo := map[repoKey][]*wbJob{}
	var order []repoKey
	for _, j := range jobs {
		if j.held != "" || j.edit == nil {
			continue
		}
		k := repoKey{j.edit.repo, j.edit.branch, j.edit.parent}
		if _, ok := byRepo[k]; !ok {
			order = append(order, k)
		}
		byRepo[k] = append(byRepo[k], j)
	}
	var rejected []*wbJob
	for _, k := range order {
		group := byRepo[k]
		files := c.applyEdits(group)
		var live []*wbJob
		for _, j := range group {
			if j.held == "" {
				live = append(live, j)
			}
		}
		if len(live) == 0 {
			continue
		}
		var lines []string
		for _, j := range live {
			from, to := j.svc.Ref.Tag, j.tag
			if j.svc.Pinned {
				from = j.svc.Ref.Tag + "@" + images.Short(j.svc.Ref.Digest)
				if t, d, ok := strings.Cut(j.tag, "@"); ok {
					to = t + "@" + images.Short(d)
				}
			}
			lines = append(lines, fmt.Sprintf("%s/%s: %s %s → %s (%s %s)", j.app.Metadata.Name, j.svc.Name, images.Repo(j.svc.Image), from, to, j.file, j.key))
		}
		msg := "composecd: update images\n\n" + strings.Join(lines, "\n") + "\n\n" + TrailerImageUpdate + "\n"
		wb := c.Boot.ImageUpdates.WriteBack
		author := gitcache.Author{Name: wb.Author.Name, Email: wb.Author.Email}
		if author.Name == "" {
			author.Name = config.DefaultWriteBackName
		}
		if author.Email == "" {
			author.Email = config.DefaultWriteBackEmail
		}
		if c.beforePush != nil {
			c.beforePush(k.url)
		}
		sha, err := c.Git.CommitFiles(ctx, k.url, gitcache.Commit{Branch: k.branch, Parent: k.parent, Files: files, Message: msg,
			Author: author, SigningKeyFile: wb.SigningKeyFile})
		switch {
		case errors.Is(err, gitcache.ErrRejected) && retry:
			rejected = append(rejected, live...)
			continue
		case errors.Is(err, gitcache.ErrRejected):
			err = fmt.Errorf("%w, %d times", err, writeBackRetries+1)
		}
		for _, j := range live {
			switch {
			case err != nil:
				j.held = "writing to Git: " + err.Error()
			case sha == "":
				// Git says so already; the next render shows it.
				j.commit = k.parent
			default:
				j.commit = sha
			}
		}
		if sha != "" {
			c.wbMu.Lock()
			if c.ownCommits == nil {
				c.ownCommits = map[string]bool{}
			}
			c.ownCommits[sha] = true
			c.wbMu.Unlock()
		}
	}
	return rejected
}

// applyEdits returns the new contents of the files group's jobs edit, all in
// one repository, holding jobs whose edit fails or conflicts.
func (c *Controller) applyEdits(group []*wbJob) map[string][]byte {
	files := map[string][]byte{}
	sort.SliceStable(group, func(a, b int) bool { return group[a].edit.path < group[b].edit.path })
	for _, j := range group {
		e := j.edit
		for _, o := range group {
			if o != j && o.held == "" && o.edit.same(e) && o.edit.value != e.value {
				j.held = fmt.Sprintf("%s/%s needs %s in %s to be %s at the same time", o.app.Metadata.Name, o.svc.Name, j.key, e.path, o.edit.value)
			}
		}
		if j.held != "" {
			continue
		}
		data, ok := files[e.path]
		if !ok {
			var err error
			if data, err = os.ReadFile(filepath.Join(e.dir, filepath.FromSlash(e.path))); err != nil {
				j.held = fmt.Sprintf("reading %s: %v", e.path, err)
				continue
			}
		}
		out, err := setIn(data, e.app, e.key, e.value)
		if err == nil {
			// What was written must read back as the value.
			var n *yaml.Node
			if n, err = findIn(out, e.app, e.key); err == nil && (n == nil || n.Value != e.value) {
				err = errors.New("the edit doesn't read back")
			}
		}
		if err != nil {
			j.held = fmt.Sprintf("editing %s %s: %v", e.path, j.key, err)
			continue
		}
		files[e.path] = out
	}
	return files
}

// docOf is the top node of data to edit: the Application named app's
// document, or the only one.
func docOf(data []byte, app string) (*yaml.Node, error) {
	if app == "" {
		return yamledit.Root(data)
	}
	docs, err := yamledit.Docs(data)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if d == nil {
			continue
		}
		if k := yamledit.Find(d, []string{"kind"}); k == nil || k.Value != config.KindApplication {
			continue
		}
		if n := yamledit.Find(d, []string{"metadata", "name"}); n != nil && n.Value == app {
			return d, nil
		}
	}
	return nil, fmt.Errorf("no Application %s in the file", app)
}

func setIn(data []byte, app string, key []string, value string) ([]byte, error) {
	doc, err := docOf(data, app)
	if err != nil {
		return nil, err
	}
	return yamledit.Set(data, doc, key, value)
}

func findIn(data []byte, app string, key []string) (*yaml.Node, error) {
	doc, err := docOf(data, app)
	if err != nil {
		return nil, err
	}
	return yamledit.Find(doc, key), nil
}

func (c *Controller) setWBState(app, svc string, st *wbState) {
	c.wbMu.Lock()
	defer c.wbMu.Unlock()
	if c.wbStates == nil {
		c.wbStates = map[string]map[string]*wbState{}
	}
	if c.wbStates[app] == nil {
		c.wbStates[app] = map[string]*wbState{}
	}
	c.wbStates[app][svc] = st
}

// wbStateOf is a copy of the last write-back of app's svc, or nil.
func (c *Controller) wbStateOf(app, svc string) *wbState {
	c.wbMu.Lock()
	defer c.wbMu.Unlock()
	if st := c.wbStates[app][svc]; st != nil {
		cp := *st
		return &cp
	}
	return nil
}

// ImageUpdateCommit is true when sha is a commit composecd's image write-back
// pushed, whose webhook needs no refresh: composecd applied it already.
func (c *Controller) ImageUpdateCommit(sha string) bool {
	c.wbMu.Lock()
	defer c.wbMu.Unlock()
	return sha != "" && c.ownCommits[sha]
}

// queueWriteBack writes app's pending candidates in the background, once per
// candidate and source, then reconciles it.
func (c *Controller) queueWriteBack(app, key string) {
	c.wbMu.Lock()
	if c.wbQueued == nil {
		c.wbQueued = map[string]bool{}
	}
	if c.wbQueued[key] {
		c.wbMu.Unlock()
		return
	}
	c.wbQueued[key] = true
	c.wbMu.Unlock()
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		res := c.writeBack(c.baseContext(), []string{app})
		if res.root {
			c.Refresh()
		}
		if len(res.apps) == 0 {
			return
		}
		c.bumpGeneration()
		for name := range res.apps {
			if a, err := c.application(name); err == nil {
				c.reconcileAsync(a, false)
			}
		}
	}()
}

// wbView is how write-back stands for one service with a candidate.
type wbView struct {
	tag string
	// inMemory is set when an admin applied the update in memory.
	inMemory bool
	decision *store.ImageDecision
	// state is the last write-back, nil while it is still to be written.
	state       *wbState
	fingerprint string
}

// writeBackViews works out, for each of app's services with a candidate, how
// write-back stands: dropping decisions whose source changed, or that a newer
// candidate replaces, and queueing candidates still to be written.
func (c *Controller) writeBackViews(ctx context.Context, app *config.Application, prep *source.Prepared, svcs []images.Service,
	picks map[string]store.ImageTag) (map[string]*wbView, []string) {
	name := app.Metadata.Name
	var warnings []string
	decisions, err := c.Store.ImageDecisions(name)
	if err != nil {
		warnings = append(warnings, "reading image decisions: "+err.Error())
	}
	drop := func(svc, why string) {
		c.Log.Info("dropping an image decision", "app", name, "service", svc, "reason", why)
		if err := c.Store.DeleteImageDecision(name, svc); err != nil {
			c.Log.Error("dropping an image decision", "app", name, "service", svc, "error", err)
		}
	}
	views := map[string]*wbView{}
	for _, s := range svcs {
		p, ok := picks[s.Name]
		if !s.MovesTags() || !ok || !pickedFor(p, s) || p.Tag == "" {
			continue
		}
		d, decided := decisions[s.Name]
		if p.Tag == s.Ref.Tag {
			if decided {
				drop(s.Name, "Git renders the tag picked")
			}
			continue
		}
		st := c.wbStateOf(name, s.Name)
		repo, file, key := "", "", ""
		switch {
		case decided:
			repo, file, key = d.Repo, d.File, d.Key
		case st != nil:
			repo, file, key = st.repo, st.file, st.key
		}
		v := &wbView{tag: p.Tag, fingerprint: c.imageFingerprint(ctx, app, prep, s, repo, file, key)}
		if decided && d.Fingerprint != v.fingerprint {
			drop(s.Name, "its source changed in Git")
			decided = false
		}
		if decided && d.Choice == store.DecisionDismiss && d.Candidate != p.Tag {
			drop(s.Name, "a newer tag "+p.Tag+" replaces "+d.Candidate)
			decided = false
		}
		if decided {
			v.decision, v.inMemory = &d, d.Choice == store.DecisionMemory
		} else {
			c.adoptWriteBack(name, s, v, st)
		}
		views[s.Name] = v
	}
	return views, warnings
}

// adoptWriteBack sets v.state to st, the last write-back of app name's s, when
// it wrote v's candidate for s's image and source as they are now, and queues
// the write otherwise.
func (c *Controller) adoptWriteBack(name string, s images.Service, v *wbView, st *wbState) {
	if st == nil || st.candidate != v.tag || st.image != s.Image || st.fingerprint != "" && st.fingerprint != v.fingerprint {
		c.queueWriteBack(name, strings.Join([]string{name, s.Name, v.tag, s.Image, v.fingerprint}, "\x00"))
		return
	}
	if st.fingerprint == "" {
		c.wbMu.Lock()
		if cur := c.wbStates[name][s.Name]; cur != nil {
			cur.fingerprint = v.fingerprint
		}
		c.wbMu.Unlock()
	}
	v.state = st
}

// pinnedWriteBackView is how writing cand, the new tag@digest of s, an image
// pinned in Git, stands. Admins can't apply it in memory or dismiss it: an
// update write-back can't write is refused, as without write-back.
func (c *Controller) pinnedWriteBackView(ctx context.Context, app *config.Application, prep *source.Prepared, s images.Service, cand string) *wbView {
	name := app.Metadata.Name
	st := c.wbStateOf(name, s.Name)
	repo, file, key := "", "", ""
	if st != nil {
		repo, file, key = st.repo, st.file, st.key
	}
	v := &wbView{tag: cand, fingerprint: c.imageFingerprint(ctx, app, prep, s, repo, file, key)}
	c.adoptWriteBack(name, s, v, st)
	return v
}

// imageFingerprint identifies what s's tag comes from in Git, so a decision
// about it lapses when that changes: its tag-key label, the Application's
// source, the chart, and the value at key in file, when known.
func (c *Controller) imageFingerprint(ctx context.Context, app *config.Application, prep *source.Prepared, s images.Service, repo, file, key string) string {
	h := sha256.New()
	add := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	spec, _ := json.Marshal(app.Spec.Source)
	add(s.TagKey, string(spec))
	if src := app.Spec.Source; src.IsOCI() {
		add(prep.ChartRef)
	} else if tree, err := c.Git.TreeID(ctx, src.RepoURL, prep.Revision, src.Path); err == nil {
		add(tree)
	}
	if file != "" {
		add(repo, file, key, c.valueAt(ctx, app, prep, repo, file, key))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// valueAt is the text at key in file of repo, as the app renders it now.
func (c *Controller) valueAt(ctx context.Context, app *config.Application, prep *source.Prepared, repo, file, key string) string {
	var dir, doc string
	switch {
	case NormalizeRepoURL(repo) == NormalizeRepoURL(sourceRepo(app)) && prep.Root != "":
		dir = prep.Root
	case NormalizeRepoURL(repo) == NormalizeRepoURL(c.Boot.Root.RepoURL):
		rev := c.Root().Revision
		if rev == "" {
			return ""
		}
		var err error
		if dir, err = c.Git.Checkout(ctx, c.Boot.Root.RepoURL, rev); err != nil {
			return ""
		}
	default:
		return ""
	}
	path := yamledit.Key(key)
	if file == path2Origin(app.Origin, c.Boot.Root.Path) {
		doc, path = app.Metadata.Name, append([]string{"spec", "source", "values"}, path...)
	}
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file)))
	if err != nil {
		return ""
	}
	n, err := findIn(data, doc, path)
	if err != nil || n == nil {
		return ""
	}
	return n.Value
}

// path2Origin is the repository path of the file an Application is defined in.
func path2Origin(origin, rootPath string) string {
	file, _, _ := strings.Cut(origin, ":")
	return path.Join(rootPath, file)
}

// DecideImage records an admin's decision about the update held for app's
// service: apply it in memory (choice store.DecisionMemory), diverging from
// Git, or dismiss it until a newer tag (store.DecisionDismiss). It is logged
// as an operation, and the app reconciles.
func (c *Controller) DecideImage(app, service, choice, user string) (*store.ImageDecision, error) {
	a, err := c.application(app)
	if err != nil {
		return nil, err
	}
	if choice != store.DecisionMemory && choice != store.DecisionDismiss {
		return nil, fmt.Errorf("choice %q must be %s or %s", choice, store.DecisionMemory, store.DecisionDismiss)
	}
	c.mu.RLock()
	var im *ImageStatus
	if s, ok := c.apps[app]; ok {
		for i := range s.status.Images {
			if s.status.Images[i].Service == service {
				cp := s.status.Images[i]
				im = &cp
			}
		}
	}
	c.mu.RUnlock()
	if im == nil {
		return nil, fmt.Errorf("service %q of %s: %w", service, app, ErrNotFound)
	}
	if im.Held == "" {
		return nil, fmt.Errorf("%s/%s: %w", app, service, ErrNotHeld)
	}
	d := store.ImageDecision{App: app, Service: service, Candidate: im.Candidate, Choice: choice, User: user,
		Fingerprint: im.fingerprint, Repo: im.repo, File: im.File, Key: im.Key, DecidedAt: time.Now()}
	if err := c.Store.SetImageDecision(d); err != nil {
		return nil, err
	}
	gitTag := ""
	if r, err := images.Parse(im.Image); err == nil {
		gitTag = r.Tag
	}
	msg := fmt.Sprintf("%s (%s): dismissed %s until a newer tag; Git keeps %s", images.Repo(im.Image), service, im.Candidate, gitTag)
	if choice == store.DecisionMemory {
		msg = fmt.Sprintf("%s (%s): applying %s in memory; the deployment diverges from Git, which says %s", images.Repo(im.Image), service, im.Candidate, gitTag)
	}
	now := time.Now()
	op := &store.Operation{App: app, Kind: store.KindImageDecision, Trigger: store.TriggerManual, User: user, StartedAt: now}
	if err := c.Store.StartOperation(op); err == nil {
		op.Phase, op.Message, op.Log = store.PhaseSucceeded, msg, msg+"\nHeld because: "+im.Held+"\n"
		if err := c.Store.FinishOperation(op); err != nil {
			c.Log.Error("recording an image decision", "app", app, "error", err)
		}
	}
	c.Log.Info("image decision", "app", app, "service", service, "choice", choice, "candidate", im.Candidate, "user", user)
	c.bumpGeneration()
	c.reconcileAsync(a, false)
	return &d, nil
}
