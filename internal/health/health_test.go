package health

import (
	"testing"

	"github.com/trevorswanson/composetpl/pkg/compose"
)

func TestAssess(t *testing.T) {
	run := compose.Container{Name: "a-web-1", State: "running"}
	cases := []struct {
		name       string
		deployed   bool
		containers []compose.Container
		want       Status
		msg        string
	}{
		{"not deployed", false, nil, Missing, "not deployed"},
		{"no containers", true, nil, Missing, "no containers"},
		{"running", true, []compose.Container{run, {Name: "a-db-1", State: "running", Health: "healthy"}}, Healthy, ""},
		{"init job done", true, []compose.Container{run, {Name: "a-init-1", State: "exited"}}, Healthy, ""},
		{"starting", true, []compose.Container{run, {Name: "a-db-1", State: "running", Health: "starting"}}, Progressing, "a-db-1 is starting"},
		{"crashed", true, []compose.Container{
			{Name: "a-db-1", State: "running", Health: "starting"},
			{Name: "a-web-1", State: "exited", ExitCode: 137},
			{Name: "a-api-1", State: "running", Health: "unhealthy"},
		}, Degraded, "a-api-1 is unhealthy; a-web-1 exited with code 137"},
		{"paused", true, []compose.Container{run, {Name: "a-db-1", State: "paused"}}, Suspended, "a-db-1 is paused"},
		{"restarting", true, []compose.Container{{Name: "a-web-1", State: "restarting"}}, Degraded, "a-web-1 is restarting"},
		{"odd state", true, []compose.Container{{Name: "a-web-1", State: "weird"}}, Unknown, `a-web-1 is in state "weird"`},
	}
	for _, tc := range cases {
		got, msg := Assess(tc.deployed, tc.containers)
		if got != tc.want || msg != tc.msg {
			t.Errorf("%s: got %s %q, want %s %q", tc.name, got, msg, tc.want, tc.msg)
		}
	}
}
