package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/trevorswanson/composecd/internal/auth"
	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/features"
)

// Edits change the root repository: a form or the YAML editor plans one, the
// review page shows its diff, and only committing it from there changes Git.

// kindApplicationSet is the ApplicationSet kind, as edits name it.
const kindApplicationSet = "ApplicationSet"

// editKinds are the kinds edits change, by the name their URLs use.
var editKinds = map[string]string{
	"application":    config.KindApplication,
	"host":           config.KindHost,
	"applicationset": kindApplicationSet,
	"window":         config.KindSyncWindow,
}

// editPages are the pages of an edit; none takes live updates, which would
// replace what is being typed.
var editPages = []string{"edit", "review", "committed"}

// Edit is an edit as its pages show it.
type Edit struct {
	// Kind and Name are the object; Name is "" for a new one.
	Kind, Name string
	// Document and File are the YAML editor's; File only for a new object.
	Document, File string
	Plan           *controller.EditPlan
	Rows           []DiffRow
	Stat           Stat
	// Request is the plan's request as JSON, which the commit form sends back.
	Request string
	Commit  string
}

// URLKind is the kind as edit URLs name it.
func (e *Edit) URLKind() string {
	for k, v := range editKinds {
		if v == e.Kind {
			return k
		}
	}
	return ""
}

// ObjectURL is the page of the object, or of its kind once it is deleted.
func (e *Edit) ObjectURL() string {
	gone := e.Plan != nil && e.Plan.Request.Delete
	name := url.PathEscape(e.Name)
	if e.Name == "" && e.Plan != nil {
		name = url.PathEscape(documentName(e.Plan.Request.Document))
	}
	switch e.Kind {
	case config.KindApplication:
		if gone || name == "" {
			return "/apps"
		}
		return "/apps/" + name + "?tab=configuration"
	case config.KindHost:
		if gone || name == "" {
			return "/hosts"
		}
		return "/hosts/" + name
	case kindApplicationSet:
		if gone || name == "" {
			return "/applicationsets"
		}
		return "/applicationsets/" + name
	}
	return "/windows"
}

// documentName is the metadata.name of a YAML document, if it has one.
func documentName(doc string) string {
	var head struct {
		Metadata config.Metadata `yaml:"metadata"`
	}
	_ = yaml.Unmarshal([]byte(doc), &head)
	return head.Metadata.Name
}

// newDocuments are where a new object starts in the YAML editor.
var newDocuments = map[string]string{
	config.KindApplication: `apiVersion: composecd/v1alpha1
kind: Application
metadata:
  name: NAME
spec:
  source:
    path: charts/NAME
  destination:
    host: HOST
  syncPolicy:
    automated: {}
`,
	config.KindHost: `apiVersion: composecd/v1alpha1
kind: Host
metadata:
  name: NAME
spec:
  url: ssh://deploy@HOST
`,
	kindApplicationSet: `apiVersion: composecd/v1alpha1
kind: ApplicationSet
metadata:
  name: NAME
spec:
  generators:
    - gitDirectories:
        directories: ["charts/*"]
  template:
    metadata:
      name: "{{ .path.basename }}"
    spec:
      source: {path: "{{ .path.path }}"}
      destination: {host: HOST}
`,
	config.KindSyncWindow: `apiVersion: composecd/v1alpha1
kind: SyncWindow
metadata:
  name: NAME
spec:
  kind: deny
  schedule: "0 22 * * *"
  duration: 8h
  applications: ["*"]
`,
}

// EditPage is the YAML editor of an object, or of a new one.
func (s *Server) EditPage(w http.ResponseWriter, r *http.Request) {
	p := s.identify(w, r, auth.RoleAdmin)
	if p == nil {
		return
	}
	kind, ok := editKinds[r.PathValue("kind")]
	if !ok {
		pageError(w, controller.ErrNotFound)
		return
	}
	e := &Edit{Kind: kind, Name: r.PathValue("name")}
	if e.Name == "" {
		e.Document, e.File = newDocuments[kind], s.newFile(kind)
	} else {
		var err error
		if e.File, e.Document, err = s.C.EditSource(r.Context(), kind, e.Name); err != nil {
			pageError(w, err)
			return
		}
	}
	render(w, 200, "page", s.editData(p, "edit", e))
}

// newFile is the file a new object of kind goes in by default: the file of
// the first one in Git, or one named for the kind.
func (s *Server) newFile(kind string) string {
	var origins []string
	if objs := s.C.Objects(); objs != nil {
		switch kind {
		case config.KindApplication:
			for _, a := range objs.Applications {
				if a.Metadata.Labels[config.LabelApplicationSet] == "" {
					origins = append(origins, a.Origin)
				}
			}
		case config.KindHost:
			for _, h := range objs.Hosts {
				origins = append(origins, h.Origin)
			}
		case kindApplicationSet:
			for _, as := range objs.ApplicationSets {
				origins = append(origins, as.Origin)
			}
		case config.KindSyncWindow:
			for _, sw := range objs.SyncWindows {
				origins = append(origins, sw.Origin)
			}
		}
	}
	for _, o := range origins {
		if file, _, ok := strings.Cut(o, ":"); ok && !strings.Contains(file, " ") {
			return file
		}
	}
	return strings.ToLower(kind) + "s.yaml"
}

func (s *Server) editData(p *auth.Principal, page string, e *Edit) *Data {
	return &Data{User: p, Auth: !s.Auth.Disabled, OIDC: s.Auth.OIDC != nil, Admin: true, Version: s.Version, Root: s.C.Root(), Page: page, Name: e.Name, Edit: e}
}

// PlanEdit shows the diff of an edit from a settings form or the YAML editor.
func (s *Server) PlanEdit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	p := s.identify(w, r, auth.RoleAdmin)
	if p == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	req, err := editRequest(r.PostForm)
	e := &Edit{Kind: req.Kind, Name: req.Name}
	d := s.editData(p, "review", e)
	if err == nil {
		err = s.plan(r.Context(), e, req)
	}
	if err != nil {
		d.Error = err.Error()
		if errors.Is(err, controller.ErrNoChange) {
			d.Error = "This edit changes nothing. Go back and change a setting."
		}
		render(w, editStatus(err), "page", d)
		return
	}
	render(w, 200, "page", d)
}

// plan plans req into e.
func (s *Server) plan(ctx context.Context, e *Edit, req controller.EditRequest) error {
	plan, err := s.C.PlanEdit(ctx, req)
	if err != nil {
		return err
	}
	body, err := json.Marshal(plan.Request)
	if err != nil {
		return err
	}
	e.Plan, e.Request = plan, string(body)
	after := plan.After
	if plan.Removed {
		after = ""
	}
	e.Rows = sideBySide(plan.Before, after, false)
	e.Stat = rowStat(e.Rows)
	return nil
}

// CommitEdit commits a reviewed edit. When Git moved on since the review, it
// shows the edit planned again, to review again.
func (s *Server) CommitEdit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	p := s.identify(w, r, auth.RoleAdmin)
	if p == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	var req controller.EditRequest
	if err := json.Unmarshal([]byte(r.PostFormValue("request")), &req); err != nil {
		pageError(w, invalidRequest("invalid edit"))
		return
	}
	e := &Edit{Kind: req.Kind, Name: req.Name}
	res, err := s.C.ApplyEdit(r.Context(), req, r.PostFormValue("token"), p.Name)
	if errors.Is(err, controller.ErrEditStale) {
		d := s.editData(p, "review", e)
		d.Error = "Git changed since you reviewed this edit. This is the edit against Git as it is now: review it again."
		if err := s.plan(r.Context(), e, req); err != nil {
			d.Error = err.Error()
		}
		render(w, 409, "page", d)
		return
	}
	if err != nil {
		d := s.editData(p, "review", e)
		d.Error = err.Error()
		render(w, editStatus(err), "page", d)
		return
	}
	e.Plan, e.Commit = res.Plan, res.Commit
	render(w, 200, "page", s.editData(p, "committed", e))
}

// editStatus is the status of a page showing err.
func editStatus(err error) int {
	switch {
	case controller.InvalidEdit(err), errors.As(err, new(invalidRequest)):
		return http.StatusBadRequest
	case errors.Is(err, controller.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, controller.ErrNoChange), errors.Is(err, controller.ErrEditStale):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// editRequest reads an edit from a form. A settings form names each setting
// it shows in field, with its value as it was in o.PATH and as it is now in
// v.PATH; only those changed are edited, an emptied one is removed.
func editRequest(form url.Values) (controller.EditRequest, error) {
	req := controller.EditRequest{Kind: form.Get("kind"), Name: form.Get("name")}
	switch form.Get("mode") {
	case "create":
		req.Create, req.File, req.Document = true, strings.TrimSpace(form.Get("file")), form.Get("document")
		return req, nil
	case "document":
		req.Document = form.Get("document")
		return req, nil
	case "delete":
		req.Delete = true
		return req, nil
	}
	type change struct {
		f         features.Feature
		was, now  string
		on, wasOn bool
	}
	changes := map[string]change{}
	for _, path := range form["field"] {
		f, ok := settingFeature(req.Kind, path)
		if !ok {
			return req, invalidRequest(path + " is not a setting of " + req.Kind)
		}
		was, now := normalize(form.Get("o."+path)), normalize(form.Get("v."+path))
		changes[path] = change{f: f, was: was, now: now, on: now != "", wasOn: was != ""}
	}
	// A section turned off takes its settings with it; changes to them are
	// left out, rather than turning it back on.
	off := func(path string) bool {
		for p, c := range changes {
			if c.f.Input() == features.InputSection && !c.on && strings.HasPrefix(path, p+".") {
				return true
			}
		}
		return false
	}
	for path, c := range changes {
		if c.was == c.now || off(path) {
			continue
		}
		v, err := c.f.Value(c.now)
		if err != nil {
			return req, invalidRequest(err.Error())
		}
		if v == "" {
			req.Unset = append(req.Unset, path)
			continue
		}
		if req.Set == nil {
			req.Set = map[string]string{}
		}
		req.Set[path] = v
	}
	return req, nil
}

// settingFeature is the feature a settings form edits at path of kind; an
// ApplicationSet's template has an Application's.
func settingFeature(kind, path string) (features.Feature, bool) {
	switch kind {
	case config.KindApplication:
		return features.Find(features.Application, path)
	case config.KindHost:
		return features.Find(features.Host, path)
	case kindApplicationSet:
		if rest, ok := strings.CutPrefix(path, templatePrefix); ok {
			return features.Find(features.Application, rest)
		}
		return features.Find(features.ApplicationSet, path)
	}
	return features.Feature{}, false
}

// templatePrefix is the path of an ApplicationSet's template's settings.
const templatePrefix = "spec.template."

func normalize(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

// editable reports whether the settings on page can be edited in place, and
// why not when they can't.
func editable(d *Data) bool {
	if !d.Admin {
		return false
	}
	switch d.Page {
	case "detail":
		return d.Tab == "configuration" && d.Settings.Owner == "" && d.Settings.Error == ""
	case "host":
		return !d.Host.Retired && d.Settings.Error == ""
	case "applicationset":
		return d.Settings.Error == ""
	}
	return false
}
