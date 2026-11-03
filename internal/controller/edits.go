package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/features"
	"github.com/trevorswanson/composecd/internal/gitcache"
	"github.com/trevorswanson/composecd/internal/yamledit"
)

// Errors of edits to Git.
var (
	// ErrEditStale is an edit whose plan changed since it was reviewed,
	// because Git moved on: review it again.
	ErrEditStale = errors.New("the root repository changed since this edit was reviewed; review it again")
	// ErrNoChange is an edit that changes nothing.
	ErrNoChange = errors.New("the edit changes nothing")
)

// TrailerEditedBy ends the message of the commits edits make, naming who
// made the edit.
const TrailerEditedBy = "Composecd-Edited-By"

// EditRequest is a change to an object in the root, which PlanEdit shows as
// a diff and ApplyEdit commits. It is one of: Set and Unset, settings of the
// object by their paths; Document, the object's whole YAML; Create, a new
// object; or Delete.
type EditRequest struct {
	// Kind and Name are the object: Host, Application, ApplicationSet or
	// SyncWindow.
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Set gives settings new values, as YAML; Unset removes them.
	Set   map[string]string `json:"set,omitempty"`
	Unset []string          `json:"unset,omitempty"`
	// Document is the object's new YAML (with Create, the new object's).
	Document string `json:"document,omitempty"`
	Create   bool   `json:"create,omitempty"`
	// File is the file a new object goes in, under the root's path; a file
	// that doesn't exist yet is created.
	File   string `json:"file,omitempty"`
	Delete bool   `json:"delete,omitempty"`
}

// EditPlan is the commit an edit makes: one file of the root repository,
// before and after.
type EditPlan struct {
	Request EditRequest `json:"request"`
	Repo    string      `json:"repo"`
	Branch  string      `json:"branch"`
	// Parent is the commit the edit is made against.
	Parent string `json:"parent"`
	// File is the file changed, from the repository's root; After is ""
	// when the edit removes it.
	File    string `json:"file"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Removed bool   `json:"removed,omitempty"`
	// Message is the commit message.
	Message string `json:"message"`
	// Warnings are what the edit does besides changing Git.
	Warnings []string `json:"warnings,omitempty"`
	// Token identifies exactly this plan: committing needs it, so what is
	// committed is what was reviewed.
	Token string `json:"token"`
}

// EditResult is a committed edit.
type EditResult struct {
	Plan   *EditPlan `json:"plan"`
	Commit string    `json:"commit"`
}

// editKinds are the kinds edits may change, with the scope of their settings.
var editKinds = map[string]features.Scope{
	config.KindHost:        features.Host,
	config.KindApplication: features.Application,
	"ApplicationSet":       features.ApplicationSet,
	config.KindSyncWindow:  "",
}

// editTip is the tip of the root's branch, checked out.
type editTip struct {
	url, branch, sha, dir string
	// root is the directory of the objects in dir.
	root string
}

// rootTip fetches the root and checks out its branch's tip. Edits need the
// root's targetRevision to be a branch.
func (c *Controller) rootTip(ctx context.Context) (*editTip, func(), error) {
	root := c.Boot.Root
	if err := c.Git.Fetch(ctx, root.RepoURL); err != nil {
		return nil, nil, fmt.Errorf("fetching the root: %w", err)
	}
	branch, err := c.Git.Branch(ctx, root.RepoURL, root.TargetRevision)
	if err != nil {
		return nil, nil, fmt.Errorf("composecd commits edits to the root's targetRevision, which must be a branch: %w", err)
	}
	sha, err := c.Git.Resolve(ctx, root.RepoURL, branch)
	if err != nil {
		return nil, nil, err
	}
	release := c.useCheckout(root.RepoURL, sha)
	dir, err := c.Git.Checkout(ctx, root.RepoURL, sha)
	if err != nil {
		release()
		return nil, nil, err
	}
	return &editTip{url: root.RepoURL, branch: branch, sha: sha, dir: dir, root: filepath.Join(dir, filepath.FromSlash(root.Path))}, release, nil
}

// EditSource is the object's YAML document as it is at the tip of the root's
// branch, and its file under the root's path.
func (c *Controller) EditSource(ctx context.Context, kind, name string) (file, document string, err error) {
	tip, release, err := c.rootTip(ctx)
	if err != nil {
		return "", "", err
	}
	defer release()
	file, data, doc, err := findObject(tip, kind, name)
	if err != nil {
		return "", "", err
	}
	return file, string(data[doc.Start:doc.End]), nil
}

// PlanEdit makes req against the tip of the root's branch, checks that the
// objects are still valid after it, and returns the commit it would make.
func (c *Controller) PlanEdit(ctx context.Context, req EditRequest) (*EditPlan, error) {
	tip, release, err := c.rootTip(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return c.planEdit(tip, req)
}

// ApplyEdit commits req, if it still plans as the plan whose Token is token,
// and refreshes, so the change deploys like any other change to Git.
func (c *Controller) ApplyEdit(ctx context.Context, req EditRequest, token, user string) (*EditResult, error) {
	tip, release, err := c.rootTip(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	plan, err := c.planEdit(tip, req)
	if err != nil {
		return nil, err
	}
	if plan.Token != token {
		return nil, ErrEditStale
	}
	msg := plan.Message
	if user != "" {
		msg += "\n" + TrailerEditedBy + ": " + user + "\n"
	}
	var content []byte
	if !plan.Removed {
		content = []byte(plan.After)
	}
	wb := c.Boot.ImageUpdates.WriteBack
	author := gitcache.Author{Name: wb.Author.Name, Email: wb.Author.Email}
	if author.Name == "" {
		author.Name = config.DefaultWriteBackName
	}
	if author.Email == "" {
		author.Email = config.DefaultWriteBackEmail
	}
	sha, err := c.Git.CommitFiles(ctx, tip.url, gitcache.Commit{Branch: tip.branch, Parent: tip.sha, Files: map[string][]byte{plan.File: content},
		Message: msg, Author: author, SigningKeyFile: wb.SigningKeyFile})
	if errors.Is(err, gitcache.ErrRejected) {
		return nil, ErrEditStale
	}
	if err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}
	c.Log.Info("committed an edit", "user", user, "commit", sha, "file", plan.File, "kind", req.Kind, "name", req.Name)
	c.Refresh()
	return &EditResult{Plan: plan, Commit: sha}, nil
}

func (c *Controller) planEdit(tip *editTip, req EditRequest) (*EditPlan, error) {
	if _, ok := editKinds[req.Kind]; !ok {
		return nil, invalid("kind must be Host, Application, ApplicationSet or SyncWindow")
	}
	plan := &EditPlan{Request: req, Repo: tip.url, Branch: tip.branch, Parent: tip.sha}
	var rel string
	var data, after []byte
	var what string
	switch {
	case req.Create:
		var err error
		if rel, err = newObjectFile(req.File); err != nil {
			return nil, err
		}
		if err := checkDocument(req.Document, req.Kind, ""); err != nil {
			return nil, err
		}
		data, err = os.ReadFile(filepath.Join(tip.root, filepath.FromSlash(rel)))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		after = yamledit.AppendDocument(data, req.Document)
		name, _ := documentName(req.Document)
		what = fmt.Sprintf("add %s %s", req.Kind, name)
	default:
		var doc yamledit.Document
		var err error
		if rel, data, doc, err = findObject(tip, req.Kind, req.Name); err != nil {
			return nil, err
		}
		switch {
		case req.Delete:
			after = yamledit.RemoveDocument(data, doc)
			plan.Removed = after == nil
			what = fmt.Sprintf("remove %s %s", req.Kind, req.Name)
			plan.Warnings = append(plan.Warnings, deleteWarning(req.Kind))
		case req.Document != "":
			if err := checkDocument(req.Document, req.Kind, req.Name); err != nil {
				return nil, err
			}
			if name, _ := documentName(req.Document); name != req.Name {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("This renames %s to %s: composecd treats it as a new object, and the old one as removed. %s", req.Name, name, deleteWarning(req.Kind)))
			}
			after = yamledit.ReplaceDocument(data, doc, req.Document)
			what = fmt.Sprintf("edit %s %s", req.Kind, req.Name)
		default:
			if after, err = setSettings(data, doc, req); err != nil {
				return nil, err
			}
			what = fmt.Sprintf("change %s of %s %s", settingList(req), req.Kind, req.Name)
		}
	}
	if string(after) == string(data) {
		return nil, ErrNoChange
	}
	// The objects must still load, with the edit, as composecd will read them.
	if _, err := config.LoadDirWith(tip.root, map[string][]byte{rel: after}); err != nil {
		return nil, invalid("after this edit, Git's objects aren't valid: %v", err)
	}
	plan.File = path.Join(c.Boot.Root.Path, rel)
	plan.Before, plan.After = string(data), string(after)
	plan.Message = "composecd: " + what + "\n"
	sum := sha256.Sum256([]byte(plan.Parent + "\x00" + plan.File + "\x00" + plan.After + "\x00" + fmt.Sprint(plan.Removed)))
	plan.Token = hex.EncodeToString(sum[:])
	return plan, nil
}

// setSettings applies req's Unset and Set, in path order, to the document doc
// of data.
func setSettings(data []byte, doc yamledit.Document, req EditRequest) ([]byte, error) {
	scope := editKinds[req.Kind]
	if scope == "" {
		return nil, invalid("%s settings are edited as YAML", req.Kind)
	}
	known := map[string]bool{}
	for _, f := range features.For(scope) {
		if !f.Item() {
			known[f.Path] = true
		}
	}
	if scope == features.ApplicationSet {
		// Its template is an Application.
		for _, f := range features.For(features.Application) {
			if !f.Item() {
				known["spec.template."+f.Path] = true
			}
		}
	}
	paths := append([]string(nil), req.Unset...)
	for p := range req.Set {
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return nil, ErrNoChange
	}
	sort.Strings(paths)
	line, err := topLine(data, doc)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		if !known[p] {
			return nil, invalid("%s is not a setting of %s", p, req.Kind)
		}
		top, err := topAt(data, line)
		if err != nil {
			return nil, err
		}
		text, set := req.Set[p]
		if !set {
			if data, err = yamledit.Delete(data, top, yamledit.Key(p)); err != nil {
				return nil, fmt.Errorf("removing %s: %w", p, err)
			}
			continue
		}
		var v yaml.Node
		if err := yaml.Unmarshal([]byte(text), &v); err != nil || len(v.Content) != 1 {
			return nil, invalid("%s: the value isn't valid YAML: %v", p, err)
		}
		if data, err = yamledit.SetValue(data, top, yamledit.Key(p), v.Content[0]); err != nil {
			return nil, fmt.Errorf("setting %s: %w", p, err)
		}
	}
	return data, nil
}

// findObject finds the file (under the root's path) and document that define
// the object kind/name at tip.
func findObject(tip *editTip, kind, name string) (string, []byte, yamledit.Document, error) {
	objs, err := config.LoadDir(tip.root)
	if err != nil {
		return "", nil, yamledit.Document{}, fmt.Errorf("reading the root at %s: %w", short(tip.sha), err)
	}
	var origin string
	switch kind {
	case config.KindHost:
		if h := objs.Host(name); h != nil {
			origin = h.Origin
		}
	case config.KindApplication:
		if a := objs.Application(name); a != nil {
			origin = a.Origin
		}
	case "ApplicationSet":
		for _, s := range objs.ApplicationSets {
			if s.Metadata.Name == name {
				origin = s.Origin
			}
		}
	case config.KindSyncWindow:
		for _, w := range objs.SyncWindows {
			if w.Metadata.Name == name {
				origin = w.Origin
			}
		}
	}
	if origin == "" {
		if kind == config.KindApplication {
			return "", nil, yamledit.Document{}, fmt.Errorf("application %q isn't defined in Git at %s (an ApplicationSet's Applications are changed through its template): %w", name, short(tip.sha), ErrNotFound)
		}
		return "", nil, yamledit.Document{}, fmt.Errorf("%s %q isn't defined in Git at %s: %w", strings.ToLower(kind), name, short(tip.sha), ErrNotFound)
	}
	i := strings.LastIndexByte(origin, ':')
	rel := origin[:i]
	line, err := strconv.Atoi(origin[i+1:])
	if err != nil {
		return "", nil, yamledit.Document{}, fmt.Errorf("%s: %w", origin, err)
	}
	data, err := os.ReadFile(filepath.Join(tip.root, filepath.FromSlash(rel)))
	if err != nil {
		return "", nil, yamledit.Document{}, err
	}
	top, err := topAt(data, line)
	if err != nil {
		return "", nil, yamledit.Document{}, err
	}
	return rel, data, yamledit.DocumentAt(data, top), nil
}

// topAt is the top node of data's document whose top node is on line.
func topAt(data []byte, line int) (*yaml.Node, error) {
	docs, err := yamledit.Docs(data)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if d != nil && d.Line == line && d.Kind == yaml.MappingNode {
			return d, nil
		}
	}
	return nil, fmt.Errorf("no object at line %d", line)
}

// topLine is the line of the top node of document doc of data.
func topLine(data []byte, doc yamledit.Document) (int, error) {
	docs, err := yamledit.Docs(data)
	if err != nil {
		return 0, err
	}
	start := strings.Count(string(data[:doc.Start]), "\n") + 1
	end := strings.Count(string(data[:doc.End]), "\n") + 1
	for _, d := range docs {
		if d != nil && d.Line >= start && d.Line <= end {
			return d.Line, nil
		}
	}
	return 0, errors.New("the object's document is empty")
}

// checkDocument checks that text is one composecd object of kind. Whether it
// is valid with the rest of Git is checked with them.
func checkDocument(text, kind, name string) error {
	docs, err := yamledit.Docs([]byte(text))
	if err != nil {
		return invalid("the YAML doesn't parse: %v", err)
	}
	if len(docs) != 1 || docs[0] == nil {
		return invalid("the YAML must be exactly one object, not %d documents", len(docs))
	}
	var h struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := docs[0].Decode(&h); err != nil || h.APIVersion != config.APIVersion || h.Kind != kind {
		return invalid("the object must be apiVersion: %s, kind: %s", config.APIVersion, kind)
	}
	return nil
}

func documentName(text string) (string, error) {
	var h struct {
		Metadata config.Metadata `yaml:"metadata"`
	}
	err := yaml.Unmarshal([]byte(text), &h)
	return h.Metadata.Name, err
}

// newObjectFile cleans the file a new object goes in, under the root's path.
func newObjectFile(file string) (string, error) {
	file = strings.ReplaceAll(file, "\\", "/")
	if file == "" {
		return "", invalid("a new object needs a file, such as apps.yaml")
	}
	if strings.HasPrefix(file, "/") || strings.Contains("/"+file+"/", "/../") {
		return "", invalid("the file must be a path under the root, without ..")
	}
	rel := path.Clean(file)
	if strings.Contains("/"+rel, "/.") {
		return "", invalid("the file can't be hidden or in a hidden directory")
	}
	if ext := path.Ext(rel); ext != ".yaml" && ext != ".yml" {
		return "", invalid("the file must end in .yaml or .yml")
	}
	return rel, nil
}

func settingList(req EditRequest) string {
	var paths []string
	for p := range req.Set {
		paths = append(paths, p)
	}
	paths = append(paths, req.Unset...)
	sort.Strings(paths)
	if len(paths) > 3 {
		return fmt.Sprintf("%d settings", len(paths))
	}
	return strings.Join(paths, ", ")
}

func deleteWarning(kind string) string {
	switch kind {
	case config.KindApplication:
		return "With automated prune, composecd uninstalls the release (named volumes are kept); without it, the release stays, shown as orphaned on its host."
	case config.KindHost:
		return "Applications that still target the host stop reconciling."
	case "ApplicationSet":
		return "Its Applications go with it: with automated prune, their releases are uninstalled."
	}
	return "The window stops applying."
}

// invalidEdit is an edit composecd can't make as asked.
type invalidEdit struct{ msg string }

func (e invalidEdit) Error() string { return e.msg }

// InvalidEdit is true for an edit that is wrong as asked, rather than one
// composecd failed to make.
func InvalidEdit(err error) bool { return errors.As(err, new(invalidEdit)) }

func invalid(format string, args ...interface{}) error {
	return invalidEdit{fmt.Sprintf(format, args...)}
}
