package features

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/trevorswanson/composecd/internal/config"
)

// Inputs are how a setting is edited.
const (
	// InputBool is true, false or unset.
	InputBool = "bool"
	// InputNumber and InputText are a scalar; InputSelect one of Options.
	InputNumber = "number"
	InputText   = "text"
	InputSelect = "select"
	// InputList is a list of strings, one per line.
	InputList = "list"
	// InputYAML is any YAML: a mapping, or a list of mappings.
	InputYAML = "yaml"
	// InputSection is a section that is on when present.
	InputSection = "section"
)

// inputs are the inputs of every setting, by scope and path, from the types
// the settings decode into.
var inputs = func() map[Scope]map[string]string {
	roots := map[Scope]reflect.Type{
		Server: reflect.TypeOf(config.Bootstrap{}), Host: reflect.TypeOf(config.Host{}),
		Application: reflect.TypeOf(config.Application{}), ApplicationSet: reflect.TypeOf(config.ApplicationSet{}),
	}
	out := map[Scope]map[string]string{}
	for scope, t := range roots {
		out[scope] = map[string]string{}
		walkInputs(t, "", out[scope])
	}
	return out
}()

func walkInputs(t reflect.Type, prefix string, out map[string]string) {
	marshaler := reflect.TypeOf((*yaml.Marshaler)(nil)).Elem()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		ft := f.Type
		pointer := ft.Kind() == reflect.Pointer
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case ft.Implements(marshaler) || reflect.PointerTo(ft).Implements(marshaler):
			out[path] = InputText
		case ft.Kind() == reflect.Bool:
			out[path] = InputBool
		case ft.Kind() >= reflect.Int && ft.Kind() <= reflect.Float64:
			out[path] = InputNumber
		case ft.Kind() == reflect.String:
			out[path] = InputText
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.String:
			out[path] = InputList
		case ft.Kind() == reflect.Struct:
			if pointer {
				out[path] = InputSection
			}
			walkInputs(ft, path, out)
		default:
			out[path] = InputYAML
		}
	}
}

// Input is how f is edited: one of the Input constants.
func (f Feature) Input() string {
	if f.Options != nil {
		return InputSelect
	}
	if in := inputs[f.Scope][f.Path]; in != "" {
		return in
	}
	return InputYAML
}

// raw is v, a setting's value as written, as its input shows it.
func raw(input string, v interface{}) string {
	switch input {
	case InputSection:
		return "on"
	case InputList:
		if list, ok := v.([]interface{}); ok {
			lines := make([]string, len(list))
			for i, item := range list {
				lines[i] = fmt.Sprint(item)
			}
			return strings.Join(lines, "\n")
		}
	case InputYAML:
		if v == nil {
			return ""
		}
		data, err := yaml.Marshal(v)
		if err == nil {
			return strings.TrimRight(string(data), "\n")
		}
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// Value converts what an input of f says to the YAML that sets f, or "" to
// remove it.
func (f Feature) Value(text string) (string, error) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return "", nil
	}
	switch f.Input() {
	case InputSection:
		return "{}", nil
	case InputBool:
		if text != "true" && text != "false" {
			return "", fmt.Errorf("%s must be true or false", f.Path)
		}
		return text, nil
	case InputNumber:
		if _, err := strconv.ParseFloat(text, 64); err != nil {
			return "", fmt.Errorf("%s must be a number", f.Path)
		}
		return text, nil
	case InputSelect:
		ok := false
		for _, o := range f.Options {
			ok = ok || o == text
		}
		if !ok {
			return "", fmt.Errorf("%s must be one of %s", f.Path, strings.Join(f.Options, ", "))
		}
		return encodeString(text)
	case InputText:
		return encodeString(text)
	case InputList:
		seq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, line := range strings.Split(text, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: line})
			}
		}
		return encodeNode(seq)
	}
	var v yaml.Node
	if err := yaml.Unmarshal([]byte(text), &v); err != nil {
		return "", fmt.Errorf("%s isn't valid YAML: %v", f.Path, err)
	}
	return text, nil
}

func encodeString(s string) (string, error) {
	return encodeNode(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s})
}

func encodeNode(n *yaml.Node) (string, error) {
	data, err := yaml.Marshal(n)
	return strings.TrimRight(string(data), "\n"), err
}

// Find is the feature of scope at path.
func Find(scope Scope, path string) (Feature, bool) {
	for _, f := range registry {
		if f.Scope == scope && f.Path == path {
			return f, true
		}
	}
	return Feature{}, false
}
