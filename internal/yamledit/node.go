package yamledit

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file edits structure: it sets a key to any YAML value (a scalar, a
// list, a mapping), removes a key, and replaces, adds or removes whole
// documents. As with Set, only the edited part of the file changes: a key's
// own lines, or, inside a flow collection ({a: 1}), that collection, written
// again in flow style.

// SetValue sets the key at path under mapping m, the top node of a document
// of data, to v, creating the mappings on the way. A scalar replacing a
// scalar keeps its quoting where it can; anything else replaces the key's
// lines, keeping the old value's flow style. It returns the edited data.
func SetValue(data []byte, m *yaml.Node, path []string, v *yaml.Node) ([]byte, error) {
	if len(path) == 0 {
		return nil, errors.New("empty key")
	}
	steps := trace(m, path)
	if f, rest := flowAncestor(steps, path); f != nil {
		c := clone(f)
		if err := setIn(c, rest, v); err != nil {
			return nil, err
		}
		return rewrite(data, f, c)
	}
	if len(steps) == len(path) {
		s := steps[len(steps)-1]
		if s.val.Kind == yaml.ScalarNode && v.Kind == yaml.ScalarNode && s.val.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0 &&
			s.val.Anchor == "" && s.val.Tag != "!!null" {
			// A string keeps the old quoting, unless written plain it would
			// read as something else (true, 1.0).
			if (v.Tag == "!!str" || v.Tag == "") && (s.val.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 || plainString(v.Value)) {
				return Replace(data, s.val, v.Value)
			}
			return rewrite(data, s.val, v)
		}
		if s.val.Style&yaml.FlowStyle != 0 && v.Kind != yaml.ScalarNode {
			v = clone(v)
			flow(v)
		}
		return replacePair(data, m, s, path[len(path)-1], v)
	}
	// A key on the way that isn't a mapping (automated: with no value, say)
	// becomes one.
	k := len(steps)
	value := nest(path[k:], v)
	if k > 0 && steps[k-1].val.Kind != yaml.MappingNode {
		return replacePair(data, m, steps[k-1], path[k-1], nest(path[k:], v))
	}
	parent := m
	if k > 0 {
		parent = steps[k-1].val
	}
	return insert(data, m, parent, path[k], value.Content[1])
}

// Delete removes the key at path under mapping m, a node of data. A mapping
// left empty stays, as {}: an empty key would read as no value, which for a
// section such as automated means off. A missing key is no change.
func Delete(data []byte, m *yaml.Node, path []string) ([]byte, error) {
	if len(path) == 0 {
		return nil, errors.New("empty key")
	}
	steps := trace(m, path)
	if len(steps) < len(path) {
		return data, nil
	}
	if f, rest := flowAncestor(steps, path); f != nil {
		c := clone(f)
		deleteIn(c, rest)
		return rewrite(data, f, c)
	}
	s := steps[len(steps)-1]
	if len(s.parent.Content) == 2 && s.parent != m {
		return replacePair(data, m, steps[len(steps)-2], path[len(path)-2], &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle})
	}
	start, err := offset(data, s.key.Line, 1)
	if err != nil {
		return nil, err
	}
	if prefix := string(data[start:mustOffset(data, s.key.Line, s.key.Column)]); strings.TrimSpace(prefix) != "" {
		return nil, fmt.Errorf("line %d: can't remove the first key of a list item", s.key.Line)
	}
	end, err := lineEnd(data, nodeEnd(data, s.val))
	if err != nil {
		return nil, err
	}
	if end < len(data) {
		end++
	}
	return splice(data, start, end, ""), nil
}

// step is a key on a path, its value and the mapping holding it.
type step struct{ key, val, parent *yaml.Node }

// trace follows path from mapping m as far as it exists.
func trace(m *yaml.Node, path []string) []step {
	var out []step
	cur := m
	for _, seg := range path {
		if cur == nil || cur.Kind != yaml.MappingNode {
			break
		}
		var s *step
		for j := 0; j+1 < len(cur.Content); j += 2 {
			if cur.Content[j].Value == seg {
				s = &step{cur.Content[j], cur.Content[j+1], cur}
			}
		}
		if s == nil {
			break
		}
		out = append(out, *s)
		cur = s.val
	}
	return out
}

// flowAncestor is the outermost flow collection the edit at path goes
// inside, and the rest of path from it.
func flowAncestor(steps []step, path []string) (*yaml.Node, []string) {
	for i, s := range steps {
		if s.parent.Style&yaml.FlowStyle != 0 {
			return s.parent, path[i:]
		}
		if s.val.Style&yaml.FlowStyle != 0 && s.val.Kind == yaml.MappingNode && i < len(path)-1 {
			return s.val, path[i+1:]
		}
	}
	// Adding under a mapping written {} (or {a: 1}).
	if n := len(steps); n > 0 && n < len(path) && steps[n-1].val.Kind == yaml.MappingNode && steps[n-1].val.Style&yaml.FlowStyle != 0 {
		return steps[n-1].val, path[n:]
	}
	return nil, nil
}

// replacePair writes s's key and value again, with value v, at the key's
// column.
func replacePair(data []byte, m *yaml.Node, s step, key string, v *yaml.Node) ([]byte, error) {
	start, err := offset(data, s.key.Line, s.key.Column)
	if err != nil {
		return nil, err
	}
	end, err := lineEnd(data, nodeEnd(data, s.val))
	if err != nil {
		return nil, err
	}
	text, err := pair(key, v, s.key.Column-1, indentStep(m))
	if err != nil {
		return nil, err
	}
	return splice(data, start, end, strings.TrimRight(text, "\n")), nil
}

// insert adds key with value v to block mapping parent, a node of data,
// after its last key.
func insert(data []byte, m, parent *yaml.Node, key string, v *yaml.Node) ([]byte, error) {
	indent := 0
	if len(parent.Content) > 0 {
		indent = parent.Content[0].Column - 1
	}
	text, err := pair(key, v, indent, indentStep(m))
	if err != nil {
		return nil, err
	}
	at, err := after(data, m, parent)
	if err != nil {
		return nil, err
	}
	text = strings.Repeat(" ", indent) + text
	if at > 0 && data[at-1] != '\n' {
		text = "\n" + text
	}
	return splice(data, at, at, text), nil
}

// pair renders key: v, its first line unindented and the rest indented by
// indent, ending in a newline. A collection that isn't flow goes on the
// lines below, step further in.
func pair(key string, v *yaml.Node, indent, step int) (string, error) {
	if v.Kind == yaml.ScalarNode || v.Style&yaml.FlowStyle != 0 || len(v.Content) == 0 {
		if v.Kind != yaml.ScalarNode {
			v = clone(v)
			flow(v)
		}
		text, err := encode(v, step)
		if err != nil {
			return "", err
		}
		if strings.Contains(strings.TrimRight(text, "\n"), "\n") && v.Kind == yaml.ScalarNode {
			return "", errors.New("the value spans lines")
		}
		return quoteKey(key) + ": " + indentLines(strings.TrimRight(text, "\n"), indent) + "\n", nil
	}
	text, err := encode(v, step)
	if err != nil {
		return "", err
	}
	return quoteKey(key) + ":\n" + indentLines(strings.Repeat(" ", indent+step)+strings.TrimRight(text, "\n"), indent+step) + "\n", nil
}

// indentLines indents every line of s but the first by n spaces.
func indentLines(s string, n int) string {
	return strings.ReplaceAll(s, "\n", "\n"+strings.Repeat(" ", n))
}

func encode(v *yaml.Node, step int) (string, error) {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(step)
	if err := e.Encode(v); err != nil {
		return "", err
	}
	return b.String(), e.Close()
}

// rewrite replaces node old, a scalar or flow collection of data, with v
// written inline.
func rewrite(data []byte, old, v *yaml.Node) ([]byte, error) {
	start, err := offset(data, old.Line, old.Column)
	if err != nil {
		return nil, err
	}
	end := nodeEnd(data, old)
	if end < 0 {
		return nil, fmt.Errorf("line %d: can't find the end of the value", old.Line)
	}
	if v.Kind != yaml.ScalarNode {
		v = clone(v)
		flow(v)
	}
	text, err := encode(v, 2)
	if err != nil {
		return nil, err
	}
	return splice(data, start, end, strings.TrimRight(text, "\n")), nil
}

// nodeEnd is the offset after n's text in data, or -1.
func nodeEnd(data []byte, n *yaml.Node) int {
	start, err := offset(data, n.Line, n.Column)
	if err != nil {
		return -1
	}
	switch {
	case n.Kind == yaml.AliasNode:
		return start + 1 + len(n.Value)
	case n.Kind == yaml.ScalarNode && n.Style&yaml.DoubleQuotedStyle != 0:
		return closeDouble(data, start)
	case n.Kind == yaml.ScalarNode && n.Style&yaml.SingleQuotedStyle != 0:
		return closeSingle(data, start)
	case n.Kind == yaml.ScalarNode && n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		return blockEnd(data, start)
	case n.Kind == yaml.ScalarNode:
		if n.Tag == "!!null" && n.Value == "" {
			// An empty value: the key's line ends it.
			return start
		}
		if i := bytes.IndexByte(data[start:], '\n'); i >= 0 && i < len(n.Value) {
			return blockEnd(data, start)
		}
		return start + len(n.Value)
	case n.Style&yaml.FlowStyle != 0:
		return flowEnd(data, start)
	case len(n.Content) == 0:
		return start
	}
	return nodeEnd(data, n.Content[len(n.Content)-1])
}

// flowEnd is the offset after the flow collection opening at start.
func flowEnd(data []byte, start int) int {
	depth := 0
	for i := start; i < len(data); i++ {
		switch data[i] {
		case '[', '{':
			depth++
		case ']', '}':
			if depth--; depth == 0 {
				return i + 1
			}
		case '"':
			if i = closeDouble(data, i) - 1; i < 0 {
				return -1
			}
		case '\'':
			if i = closeSingle(data, i) - 1; i < 0 {
				return -1
			}
		case '#':
			if i > 0 && (data[i-1] == ' ' || data[i-1] == '\t') {
				for i < len(data) && data[i] != '\n' {
					i++
				}
			}
		}
	}
	return -1
}

// blockEnd is the end of the last line of a block scalar (or multi-line
// plain scalar) starting at start: the lines indented further than the line
// it starts on, and blank lines among them.
func blockEnd(data []byte, start int) int {
	lineStart := bytes.LastIndexByte(data[:start], '\n') + 1
	base := indentOf(data[lineStart:])
	end, _ := lineEnd(data, start)
	for i := end + 1; i < len(data); {
		next := bytes.IndexByte(data[i:], '\n')
		line := data[i:]
		if next >= 0 {
			line = data[i : i+next]
		}
		if strings.TrimSpace(string(line)) != "" {
			if indentOf(line) <= base {
				break
			}
			end = i + len(line)
		}
		if next < 0 {
			break
		}
		i += next + 1
	}
	return end
}

// plainString is true when s written plain reads back as the string s.
func plainString(s string) bool {
	var n yaml.Node
	if s == "" || yaml.Unmarshal([]byte(s), &n) != nil || len(n.Content) != 1 {
		return false
	}
	v := n.Content[0]
	return v.Kind == yaml.ScalarNode && v.Tag == "!!str" && v.Value == s && v.Style == 0 && plainSafe(s)
}

func indentOf(line []byte) int {
	return len(line) - len(bytes.TrimLeft(line, " "))
}

// lineEnd is the offset of the end of the line at, before its newline,
// after any comment.
func lineEnd(data []byte, at int) (int, error) {
	if at < 0 {
		return 0, errors.New("can't find the end of the value")
	}
	if i := bytes.IndexByte(data[at:], '\n'); i >= 0 {
		return at + i, nil
	}
	return len(data), nil
}

func mustOffset(data []byte, line, col int) int {
	at, _ := offset(data, line, col)
	return at
}

func splice(data []byte, start, end int, text string) []byte {
	out := make([]byte, 0, len(data)-(end-start)+len(text))
	return append(append(append(out, data[:start]...), text...), data[end:]...)
}

func indentStep(m *yaml.Node) int {
	if s := firstNested(m); s > 0 {
		return s
	}
	return 2
}

// nest is the mapping {path[0]: {path[1]: ... v}} as a key-value node pair
// list: its Content are path[0] and the rest.
func nest(path []string, v *yaml.Node) *yaml.Node {
	for i := len(path) - 1; i > 0; i-- {
		v = &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: path[i]}, v}}
	}
	return &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: path[0]}, v}}
}

// setIn sets path in mapping n, a detached tree, to v.
func setIn(n *yaml.Node, path []string, v *yaml.Node) error {
	for i, seg := range path {
		if n.Kind != yaml.MappingNode {
			*n = yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
		}
		var next *yaml.Node
		for j := 0; j+1 < len(n.Content); j += 2 {
			if n.Content[j].Value == seg {
				next = n.Content[j+1]
			}
		}
		if i == len(path)-1 {
			if next != nil {
				*next = *v
			} else {
				n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: seg}, v)
			}
			return nil
		}
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: seg}, next)
		}
		n = next
	}
	return nil
}

// deleteIn removes path from n, a detached tree, leaving emptied mappings
// as {}.
func deleteIn(n *yaml.Node, path []string) {
	for i, seg := range path {
		if n.Kind != yaml.MappingNode {
			return
		}
		for j := 0; j+1 < len(n.Content); j += 2 {
			if n.Content[j].Value != seg {
				continue
			}
			if i == len(path)-1 {
				n.Content = append(n.Content[:j], n.Content[j+2:]...)
				return
			}
			n = n.Content[j+1]
			break
		}
	}
}

func clone(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		c.Content[i] = clone(child)
	}
	return &c
}

// flow marks n and every collection under it flow style, and drops
// comments, which a flow collection on one line can't hold.
func flow(n *yaml.Node) {
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		n.Style |= yaml.FlowStyle
	}
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		flow(c)
	}
}

// Document is where one document of a file is: from the line after the
// separator before it (or the start) to the next separator (or the end).
type Document struct{ Start, End int }

// DocumentAt finds the document whose top node starts at line.
func DocumentAt(data []byte, top *yaml.Node) Document {
	d := Document{End: len(data)}
	lines := bytes.SplitAfter(data, []byte("\n"))
	at := 0
	for i, l := range lines {
		n := i + 1
		if separator(l) {
			if n < top.Line {
				d.Start = at + len(l)
			} else if n > maxLine(top) {
				d.End = at
				break
			}
		}
		at += len(l)
	}
	return d
}

func separator(line []byte) bool {
	l := strings.TrimRight(string(line), "\r\n")
	return l == "---" || l == "..." || strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "---\t")
}

// ReplaceDocument replaces document d of data with text.
func ReplaceDocument(data []byte, d Document, text string) []byte {
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return splice(data, d.Start, d.End, text)
}

// RemoveDocument removes document d of data, and a separator next to it.
func RemoveDocument(data []byte, d Document) []byte {
	start, end := d.Start, d.End
	if end < len(data) {
		// The separator after it.
		if i := bytes.IndexByte(data[end:], '\n'); i >= 0 {
			end += i + 1
		} else {
			end = len(data)
		}
	} else if start > 0 {
		// The separator before it.
		start = bytes.LastIndexByte(data[:start-1], '\n') + 1
	}
	out := splice(data, start, end, "")
	if strings.TrimSpace(string(out)) == "" {
		return nil
	}
	return out
}

// AppendDocument adds text to data as a document of its own.
func AppendDocument(data []byte, text string) []byte {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if strings.TrimSpace(string(data)) == "" {
		return []byte(text)
	}
	out := append([]byte(nil), data...)
	if !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	return append(append(out, "---\n"...), text...)
}
