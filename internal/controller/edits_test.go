package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func newEditEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	// Pushes go to a branch no work tree has checked out.
	e.git("config", "core.bare", "true")
	return e
}

func TestEditSettingsPlansThenCommitsWhatWasReviewed(t *testing.T) {
	e := newEditEnv(t)
	ctx := context.Background()
	req := EditRequest{Kind: "Application", Name: "web",
		Set:   map[string]string{"spec.syncPolicy.automated.selfHeal": "true", "spec.dependsOn": "[db]"},
		Unset: []string{"spec.syncPolicy.options.wait"}}
	// dependsOn names an app that doesn't exist: the plan says why.
	if _, err := e.c.PlanEdit(ctx, req); !InvalidEdit(err) || !strings.Contains(err.Error(), "db") {
		t.Fatalf("invalid edit: %v", err)
	}
	req.Set["spec.dependsOn"] = "[manual]"
	plan, err := e.c.PlanEdit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if plan.File != "composecd/apps.yaml" || plan.Branch != "main" || !strings.Contains(plan.After, "syncPolicy: {automated: {selfHeal: true}, options: {}}") ||
		!strings.Contains(plan.After, "dependsOn: [manual]") || !strings.Contains(plan.After, "name: manual") {
		t.Fatalf("plan:\n%s", plan.After)
	}
	if git := e.git("show", "HEAD:composecd/apps.yaml"); strings.Contains(git, "selfHeal") {
		t.Fatal("planning committed")
	}

	// A token from another plan is refused.
	if _, err := e.c.ApplyEdit(ctx, req, "not-the-token", "alice"); !errors.Is(err, ErrEditStale) {
		t.Fatalf("stale token: %v", err)
	}
	res, err := e.c.ApplyEdit(ctx, req, plan.Token, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.git("show", "HEAD:composecd/apps.yaml"); got+"\n" != plan.After {
		t.Errorf("committed:\n%s\nreviewed:\n%s", got, plan.After)
	}
	msg := e.git("log", "-1", "--format=%B")
	if e.git("rev-parse", "HEAD") != res.Commit || !strings.Contains(msg, "composecd: change spec.dependsOn, spec.syncPolicy.automated.selfHeal, spec.syncPolicy.options.wait of Application web") ||
		!strings.Contains(msg, TrailerEditedBy+": alice") {
		t.Errorf("commit %s: %s", res.Commit, msg)
	}

	// The same plan again is stale: Git moved on.
	if _, err := e.c.ApplyEdit(ctx, req, plan.Token, "alice"); err == nil {
		t.Fatal("applied twice")
	}
}

func TestEditDocumentsCreateAndDelete(t *testing.T) {
	e := newEditEnv(t)
	ctx := context.Background()
	file, doc, err := e.c.EditSource(ctx, "Application", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if file != "apps.yaml" || !strings.Contains(doc, "name: manual") || strings.Contains(doc, "name: web") {
		t.Fatalf("%s: %q", file, doc)
	}
	edited := strings.Replace(doc, "path: charts/web", "path: charts/web, valueFiles: [values.yaml]", 1)
	plan, err := e.c.PlanEdit(ctx, EditRequest{Kind: "Application", Name: "manual", Document: edited})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.After, "valueFiles: [values.yaml]") || !strings.Contains(plan.After, "name: web") {
		t.Errorf("document edit:\n%s", plan.After)
	}
	if _, err := e.c.PlanEdit(ctx, EditRequest{Kind: "Host", Name: "manual", Document: edited}); err == nil {
		t.Error("edited a document as another kind")
	}

	newApp := appYAML("extra", false, "")
	plan, err = e.c.PlanEdit(ctx, EditRequest{Kind: "Application", Create: true, File: "more/extra.yaml", Document: newApp})
	if err != nil {
		t.Fatal(err)
	}
	if plan.File != "composecd/more/extra.yaml" || plan.Before != "" || plan.After != newApp {
		t.Errorf("create: %+v", plan)
	}
	if _, err := e.c.ApplyEdit(ctx, plan.Request, plan.Token, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.PlanEdit(ctx, EditRequest{Kind: "Application", Create: true, File: "apps.yaml", Document: newApp}); !InvalidEdit(err) {
		t.Errorf("duplicate name: %v", err)
	}
	for _, file := range []string{"../escape.yaml", "notes.txt", ".hidden/x.yaml"} {
		if _, err := e.c.PlanEdit(ctx, EditRequest{Kind: "Application", Create: true, File: file, Document: appYAML("x", false, "")}); !InvalidEdit(err) {
			t.Errorf("file %s: %v", file, err)
		}
	}

	// Removing the only object in a file removes the file.
	plan, err = e.c.PlanEdit(ctx, EditRequest{Kind: "Application", Name: "extra", Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Removed || len(plan.Warnings) == 0 {
		t.Errorf("delete: %+v", plan)
	}
	if _, err := e.c.ApplyEdit(ctx, plan.Request, plan.Token, "bob"); err != nil {
		t.Fatal(err)
	}
	if out := e.git("ls-files", "composecd/more"); out != "" {
		t.Errorf("file kept: %s", out)
	}
}

func TestEditNeedsABranch(t *testing.T) {
	e := newEditEnv(t)
	e.git("tag", "v1")
	e.c.Boot.Root.TargetRevision = "v1"
	if _, err := e.c.PlanEdit(context.Background(), EditRequest{Kind: "Application", Name: "web", Unset: []string{"spec.dependsOn"}}); err == nil || !strings.Contains(err.Error(), "must be a branch") {
		t.Fatalf("%v", err)
	}
}

func TestEditRefusesUnknownSettingsAndGeneratedApps(t *testing.T) {
	e := newEditEnv(t)
	ctx := context.Background()
	if _, err := e.c.PlanEdit(ctx, EditRequest{Kind: "Application", Name: "web", Set: map[string]string{"metadata.name": "other"}}); !InvalidEdit(err) {
		t.Errorf("unknown setting: %v", err)
	}
	if _, err := e.c.PlanEdit(ctx, EditRequest{Kind: "Application", Name: "generated", Unset: []string{"spec.dependsOn"}}); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "template") {
		t.Errorf("missing app: %v", err)
	}
	if _, err := e.c.PlanEdit(ctx, EditRequest{Kind: "Application", Name: "web", Unset: []string{"spec.dependsOn"}}); !errors.Is(err, ErrNoChange) {
		t.Errorf("no change: %v", err)
	}
}

func TestEditApplicationSetTemplate(t *testing.T) {
	e := newEnv(t)
	e.commit(map[string]string{"composecd/sets.yaml": directorySet("stacks", "charts/*")})
	e.git("config", "core.bare", "true")
	plan, err := e.c.PlanEdit(context.Background(), EditRequest{Kind: "ApplicationSet", Name: "stacks",
		Set: map[string]string{"spec.template.spec.syncPolicy.options.wait": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.After, "options: {wait: true}") || !strings.Contains(plan.After, "'gen-{{ .path.basename }}'") {
		t.Errorf("template edit:\n%s", plan.After)
	}
}
