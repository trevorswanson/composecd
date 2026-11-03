// Package yamledit changes one value of a YAML file in place, the way a
// person would: only the bytes of that value change, so comments, quoting,
// key order and indentation are kept. It parses with yaml.v3 only to find
// where things are.
package yamledit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
	sigsyaml "sigs.k8s.io/yaml"
)

// Docs parses data's documents and returns the top node of each (nil for an
// empty document).
func Docs(data []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var top *yaml.Node
		if len(doc.Content) > 0 {
			top = doc.Content[0]
		}
		out = append(out, top)
	}
}

// Root is the top mapping of data, a single-document file: an empty file has
// an empty one.
func Root(data []byte) (*yaml.Node, error) {
	docs, err := Docs(data)
	if err != nil {
		return nil, err
	}
	switch {
	case len(docs) > 1:
		return nil, errors.New("the file has more than one YAML document")
	case len(docs) == 0 || docs[0] == nil:
		return &yaml.Node{Kind: yaml.MappingNode}, nil
	case docs[0].Kind != yaml.MappingNode:
		return nil, errors.New("the file is not a YAML mapping")
	}
	return docs[0], nil
}

// Key splits a dotted values key (apps.web.tag) into its segments.
func Key(dotted string) []string {
	if dotted == "" {
		return nil
	}
	return strings.Split(dotted, ".")
}

// Find returns the node at path under mapping m, or nil.
func Find(m *yaml.Node, path []string) *yaml.Node {
	n, _, _ := walk(m, path)
	if len(path) == 0 {
		return nil
	}
	return n
}

// walk follows path from m. It returns the node at path (nil if missing), the
// deepest mapping reached, the key node holding that mapping (nil for m
// itself), and how many segments it matched.
func walk(m *yaml.Node, path []string) (*yaml.Node, *yaml.Node, int) {
	cur := m
	for i, seg := range path {
		if cur == nil || cur.Kind != yaml.MappingNode {
			return nil, nil, i
		}
		var next *yaml.Node
		for j := 0; j+1 < len(cur.Content); j += 2 {
			if cur.Content[j].Value == seg {
				next = cur.Content[j+1]
			}
		}
		if next == nil {
			return nil, cur, i
		}
		if i == len(path)-1 {
			return next, cur, len(path)
		}
		cur = next
	}
	return cur, cur, len(path)
}

// Set sets the scalar at path under mapping m, a node of data, to value. An
// existing scalar keeps its quoting style, unless plain would read as another
// value; a missing key is appended to the deepest block mapping on its path,
// quoted. It returns the edited data.
func Set(data []byte, m *yaml.Node, path []string, value string) ([]byte, error) {
	if len(path) == 0 {
		return nil, errors.New("empty key")
	}
	if strings.ContainsAny(value, "\n\r") {
		return nil, errors.New("the value spans lines")
	}
	if n, _, _ := walk(m, path); n != nil {
		return Replace(data, n, value)
	}
	return add(data, m, path, value)
}

// Replace replaces scalar n, a node of data, with value, keeping its quoting
// style: only n's bytes change.
func Replace(data []byte, n *yaml.Node, value string) ([]byte, error) {
	switch {
	case n.Kind == yaml.AliasNode:
		return nil, errors.New("the value is an alias of another; edit the anchor")
	case n.Kind != yaml.ScalarNode:
		return nil, errors.New("the value is not a scalar")
	case n.Anchor != "":
		return nil, errors.New("the value is an anchor other keys may use")
	case n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		return nil, errors.New("the value is a block scalar")
	case n.Style&yaml.TaggedStyle != 0:
		return nil, errors.New("the value has an explicit tag")
	}
	start, err := offset(data, n.Line, n.Column)
	if err != nil {
		return nil, err
	}
	var end int
	var text string
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		if end = closeDouble(data, start); end < 0 {
			return nil, errors.New("unterminated double-quoted value")
		}
		text = quoteDouble(value)
	case n.Style&yaml.SingleQuotedStyle != 0:
		if end = closeSingle(data, start); end < 0 {
			return nil, errors.New("unterminated single-quoted value")
		}
		text = "'" + strings.ReplaceAll(value, "'", "''") + "'"
	default:
		end = start + len(n.Value)
		if end > len(data) || string(data[start:end]) != n.Value {
			return nil, fmt.Errorf("line %d: the value isn't written as it reads", n.Line)
		}
		text = value
		if !plainSafe(value) {
			text = quoteDouble(value)
		}
	}
	out := make([]byte, 0, len(data)+len(text))
	out = append(append(append(out, data[:start]...), text...), data[end:]...)
	return out, nil
}

// add appends path's missing keys to the deepest block mapping on it.
func add(data []byte, m *yaml.Node, path []string, value string) ([]byte, error) {
	_, parent, depth := walk(m, path)
	if parent == nil {
		return nil, fmt.Errorf("%s is not a mapping", strings.Join(path[:depth], "."))
	}
	if parent.Style&yaml.FlowStyle != 0 {
		return nil, fmt.Errorf("can't add a key to the flow mapping at %s", strings.Join(path[:depth], "."))
	}
	// Indent like the mapping's keys, and nest by the file's step.
	indent, step := 0, 2
	if len(parent.Content) > 0 {
		indent = parent.Content[0].Column - 1
	}
	if c := firstNested(m); c > 0 {
		step = c
	}
	var b strings.Builder
	for i, seg := range path[depth:] {
		b.WriteString(strings.Repeat(" ", indent+i*step))
		b.WriteString(quoteKey(seg))
		b.WriteString(":")
		if i == len(path[depth:])-1 {
			b.WriteString(" " + quoteDouble(value))
		}
		b.WriteString("\n")
	}
	at, err := after(data, m, parent)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(data)+b.Len()+1)
	out = append(out, data[:at]...)
	if at > 0 && data[at-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(append(out, b.String()...), data[at:]...)
	return out, nil
}

// after is the offset new keys of mapping parent go at: the start of the line
// of the next key that isn't inside parent, before the blank and comment lines
// that lead up to it, or the end of the file.
func after(data []byte, root, parent *yaml.Node) (int, error) {
	if len(parent.Content) == 0 {
		if parent == root {
			return len(data), nil
		}
		return 0, errors.New("can't add a key to an empty mapping")
	}
	col := parent.Content[0].Column
	last := maxLine(parent)
	next := 0
	var visit func(n *yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for j := 0; j < len(n.Content); j += 2 {
				k := n.Content[j]
				if k.Line > last && k.Column < col && (next == 0 || k.Line < next) {
					next = k.Line
				}
			}
		}
		for _, c := range n.Content {
			visit(c)
		}
	}
	visit(root)
	var at int
	if next == 0 {
		// The end of the document: the next one's separator, or the end.
		at = len(data)
		start, err := offset(data, last, 1)
		if err != nil {
			return 0, err
		}
		for i := start; i < len(data); {
			end := bytes.IndexByte(data[i:], '\n')
			if end < 0 {
				break
			}
			i += end + 1
			if bytes.HasPrefix(data[i:], []byte("---")) || bytes.HasPrefix(data[i:], []byte("...")) {
				at = i
				break
			}
		}
		if at == len(data) {
			return at, nil
		}
	} else {
		var err error
		if at, err = offset(data, next, 1); err != nil {
			return 0, err
		}
	}
	// Leave the comments and blank lines above the next key with it.
	for at > 0 {
		prev := bytes.LastIndexByte(data[:at-1], '\n') + 1
		line := strings.TrimSpace(string(data[prev : at-1]))
		if line != "" && !strings.HasPrefix(line, "#") {
			break
		}
		at = prev
	}
	return at, nil
}

func maxLine(n *yaml.Node) int {
	m := n.Line
	for _, c := range n.Content {
		m = max(m, maxLine(c))
	}
	return m
}

// firstNested is how far the file's first nested block mapping is indented
// from its parent, or 0.
func firstNested(n *yaml.Node) int {
	if n.Kind == yaml.MappingNode && n.Style&yaml.FlowStyle == 0 {
		for j := 0; j+1 < len(n.Content); j += 2 {
			k, v := n.Content[j], n.Content[j+1]
			if v.Kind == yaml.MappingNode && v.Style&yaml.FlowStyle == 0 && len(v.Content) > 0 && v.Content[0].Column > k.Column {
				return v.Content[0].Column - k.Column
			}
			if d := firstNested(v); d > 0 {
				return d
			}
		}
	}
	return 0
}

// offset converts a 1-based line and character column to a byte offset.
func offset(data []byte, line, col int) (int, error) {
	at := 0
	for l := 1; l < line; l++ {
		i := bytes.IndexByte(data[at:], '\n')
		if i < 0 {
			return 0, fmt.Errorf("line %d is past the end", line)
		}
		at += i + 1
	}
	for c := 1; c < col; c++ {
		if at >= len(data) || data[at] == '\n' {
			return 0, fmt.Errorf("line %d has no column %d", line, col)
		}
		_, size := utf8.DecodeRune(data[at:])
		at += size
	}
	return at, nil
}

// closeDouble is the offset after the double-quoted scalar at start.
func closeDouble(data []byte, start int) int {
	for i := start + 1; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return -1
}

// closeSingle is the offset after the single-quoted scalar at start.
func closeSingle(data []byte, start int) int {
	for i := start + 1; i < len(data); i++ {
		if data[i] == '\'' {
			if i+1 < len(data) && data[i+1] == '\'' {
				i++
				continue
			}
			return i + 1
		}
	}
	return -1
}

func quoteDouble(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// plainSafe is true when s, written plain, reads back as a value that prints
// as s, as values files are read (YAML 1.1, numbers as float64): a string, or
// a number such as 1.27, but not 1.30 (1.3) or yes (true).
func plainSafe(s string) bool {
	var v map[string]interface{}
	if s == "" || sigsyaml.Unmarshal([]byte("v: "+s+"\n"), &v) != nil {
		return false
	}
	switch got := v["v"].(type) {
	case string:
		return got == s
	case nil:
		return false
	default:
		return fmt.Sprint(got) == s
	}
}

func quoteKey(k string) string {
	if plainSafe(k) && !strings.ContainsAny(k, ":#{}[],&*!|>'\"%@`") {
		if _, err := fmt.Sscanf(k, "%f", new(float64)); err != nil {
			return k
		}
	}
	return quoteDouble(k)
}
