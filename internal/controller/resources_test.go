package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestHostObject(t *testing.T) {
	e := newEnv(t)
	e.c.RefreshAll(context.Background())
	fake := e.backend.fakes["nas"]
	fake.Objects["container/loose"] = decode(t, `{"Id": "abc", "Name": "/loose", "Config": {"Image": "busybox:1",
		"Env": ["PATH=/bin", "DB_PASSWORD=hunter2"], "Labels": {"tier": "x"}}, "HostConfig": {"Privileged": false}}`)
	fake.Objects["container/web-web-1"] = decode(t, `{"Name": "/web-web-1", "Config": {"Image": "nginx:1",
		"Env": ["MODE=prod"], "Labels": {"com.docker.compose.project": "web"}}}`)
	fake.Objects["network/other_default"] = decode(t, `{"Name": "other_default", "Id": "n1", "Driver": "bridge",
		"Labels": {"com.docker.compose.project": "other"}}`)
	fake.Objects["image/busybox:1"] = decode(t, `{"Id": "sha256:1", "RepoTags": ["busybox:1"], "Config": {"Env": ["PATH=/bin"]}}`)
	ctx := context.Background()

	// Nobody deployed it, so even an admin doesn't see its environment values.
	o, err := e.c.HostObject(ctx, "nas", "container", "loose", false)
	if err != nil {
		t.Fatal(err)
	}
	if o.App != "" || o.Project != "" || o.Image != "busybox:1" || !o.Redacted || strings.Contains(o.Inspect.YAML, "hunter2") ||
		!strings.Contains(o.Inspect.YAML, "DB_PASSWORD=<redacted ") || strings.Contains(o.Inspect.YAML, "Id: abc") {
		t.Fatalf("unmanaged: %+v", o)
	}
	if len(o.Inspect.Folded) == 0 || o.Inspect.Folded[0].Path != "Engine metadata" {
		t.Fatalf("folds: %+v", o.Inspect.Folded)
	}
	// An Application's container shows its environment to admins only.
	if o, err = e.c.HostObject(ctx, "nas", "container", "web-web-1", false); err != nil || o.App != "web" || o.Redacted ||
		!strings.Contains(o.Inspect.YAML, "MODE=prod") {
		t.Fatalf("managed: %+v %v", o, err)
	}
	if o, err = e.c.HostObject(ctx, "nas", "container", "web-web-1", true); err != nil || !o.Redacted || strings.Contains(o.Inspect.YAML, "prod") {
		t.Fatalf("managed, read-only: %+v %v", o, err)
	}
	// The cache is shared: redacting a copy left the admin's view alone.
	if o, err = e.c.HostObject(ctx, "nas", "container", "web-web-1", false); err != nil || !strings.Contains(o.Inspect.YAML, "MODE=prod") {
		t.Fatalf("cache: %+v %v", o, err)
	}
	if o, err = e.c.HostObject(ctx, "nas", "network", "other_default", true); err != nil || o.Project != "other" || o.App != "" ||
		o.Redacted || !strings.Contains(o.Inspect.YAML, "Driver: bridge") {
		t.Fatalf("network: %+v %v", o, err)
	}
	if o, err = e.c.HostObject(ctx, "nas", "image", "busybox:1", true); err != nil || strings.Contains(o.Inspect.YAML, "sha256:1") {
		t.Fatalf("image: %+v %v", o, err)
	}
	for _, c := range []struct {
		host, kind, name string
		want             error
	}{
		{"nas", "container", "gone", ErrNotFound},
		{"elsewhere", "container", "loose", ErrNotFound},
		{"nas", "service", "web", ErrInvalidObject},
		{"nas", "volume", "", ErrInvalidObject},
	} {
		if _, err := e.c.HostObject(ctx, c.host, c.kind, c.name, true); !errors.Is(err, c.want) {
			t.Errorf("%+v: %v", c, err)
		}
	}
}
