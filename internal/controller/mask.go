package controller

import (
	"fmt"
	"sort"
	"strings"
)

// Mask hides configured value leaves and environment values of at least four
// characters in output. Encrypted values will join this set in Phase 3c.
func (c *Controller) Mask(name, text string) string {
	app, err := c.application(name)
	if err != nil {
		return text
	}
	var secrets []string
	var leaves func(interface{})
	leaves = func(v interface{}) {
		switch v := v.(type) {
		case map[string]interface{}:
			for _, x := range v {
				leaves(x)
			}
		case []interface{}:
			for _, x := range v {
				leaves(x)
			}
		case nil:
		default:
			s := fmt.Sprint(v)
			if len(s) >= 4 {
				secrets = append(secrets, s)
			}
		}
	}
	leaves(app.Spec.Source.Values)
	for _, v := range app.Spec.Source.Env {
		leaves(v)
	}
	if h := c.Hosts.Get(app.Spec.Destination.Host); h != nil {
		for _, v := range h.Spec.Spec.Env {
			leaves(v)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, s := range secrets {
		text = strings.ReplaceAll(text, s, "••••••")
	}
	return text
}
