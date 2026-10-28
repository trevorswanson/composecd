package masking

import (
	"reflect"
	"strings"
	"testing"
)

func TestLeavesAndMasking(t *testing.T) {
	leaves := Leaves(map[string]interface{}{"nested": []interface{}{12345, true, nil, "abc", "pāss", "first-line\nsecond-line"}, "secret": "long-secret"})
	for _, s := range []string{"12345", "true", "pāss", "first-line\nsecond-line", "first-line", "second-line", "long-secret"} {
		found := false
		for _, v := range leaves {
			found = found || v == s
		}
		if !found {
			t.Errorf("missing %q", s)
		}
	}
	for _, s := range leaves {
		if s == "abc" {
			t.Fatal("short value collected")
		}
	}
	m := New([]string{"long-secret", "secret", "••••", "long-secret"})
	if got := m.Text("long-secret secret"); got != Replacement+" "+Replacement {
		t.Fatal(got)
	}
}
func TestRedactCopiesWithoutChangingKeysOrTypes(t *testing.T) {
	type output struct {
		Name   string                 `json:"name"`
		Number int                    `json:"number"`
		Values map[string]interface{} `json:"values"`
	}
	in := output{Name: "password with \"quotes\"\nand a newline", Number: 12345, Values: map[string]interface{}{"secret-key": []interface{}{"long-secret", true}}}
	var out output
	if err := New([]string{in.Name, "long-secret", "name", "secret-key", "12345"}).Redact(in, &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != Replacement || out.Number != 12345 || out.Values["secret-key"] == nil {
		t.Fatalf("%+v", out)
	}
	if !strings.Contains(in.Name, "quotes") || !reflect.DeepEqual(in.Values["secret-key"], []interface{}{"long-secret", true}) {
		t.Fatal("mutated original")
	}
}

func TestNumericValuesAreMasked(t *testing.T) {
	in := map[string]interface{}{"password": 12345, "nested": map[string]interface{}{"token": "private-secret"}, "short": 123}
	out := New(Leaves(in)).Values(in)
	if out["password"] != Replacement || out["short"] != 123 {
		t.Fatal(out)
	}
	if in["password"] != 12345 || in["nested"].(map[string]interface{})["token"] != "private-secret" {
		t.Fatal("mutated user values")
	}
}
