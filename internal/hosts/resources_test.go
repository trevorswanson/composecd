package hosts

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseResources(t *testing.T) {
	anon := strings.Repeat("ab", 32)
	containers := `[{"Name":"/web-app-1","Config":{"Image":"nginx","Labels":{"com.docker.compose.project":"web","com.docker.compose.service":"app"}},
		"State":{"Status":"running","Health":{"Status":"healthy"}},
		"NetworkSettings":{"Networks":{"web_default":{},"proxy":{},"host":{}}},
		"Mounts":[{"Type":"volume","Name":"web_data"},{"Type":"volume","Name":"` + anon + `"},{"Type":"bind"}]},
		{"Name":"/loose","Config":{"Image":"busybox"},"State":{"Status":"exited"},"NetworkSettings":{"Networks":{"bridge":{}}}}]`
	networks := `[{"Name":"web_default","Driver":"bridge","Labels":{"com.docker.compose.project":"web"}},
		{"Name":"proxy","Driver":"bridge","Labels":{"com.docker.compose.project":"traefik"}},
		{"Name":"bridge","Driver":"bridge"},{"Name":"host","Driver":"host"},{"Name":"none","Driver":"null"}]`
	volumes := `[{"Name":"web_data","Driver":"local","Labels":{"com.docker.compose.project":"web"}},{"Name":"` + anon + `","Driver":"local"}]`
	got, err := ParseResources([]byte(containers), []byte(networks), []byte(volumes))
	if err != nil {
		t.Fatal(err)
	}
	want := &Resources{
		Containers: []ResourceContainer{
			{Name: "loose", Image: "busybox", State: "exited", Networks: []string{"bridge"}},
			{Name: "web-app-1", Image: "nginx", State: "running", Health: "healthy", Project: "web", Service: "app",
				Networks: []string{"proxy", "web_default"}, Volumes: []string{"web_data"}},
		},
		Networks: []ResourceNetwork{{Name: "bridge", Driver: "bridge"}, {Name: "proxy", Driver: "bridge", Project: "traefik"},
			{Name: "web_default", Driver: "bridge", Project: "web"}},
		Volumes: []ResourceVolume{{Name: "web_data", Driver: "local", Project: "web"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
