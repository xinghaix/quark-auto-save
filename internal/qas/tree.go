package qas

import (
	"fmt"
	"strings"
)

type saveNode struct {
	ID, Tag, Parent string
	Data            map[string]any
}

type saveTree struct {
	Root  string
	Order []string
	Nodes map[string]*saveNode
}

func newSaveTree() *saveTree { return &saveTree{Nodes: map[string]*saveNode{}} }

func (t *saveTree) create(tag, id, parent string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	t.Nodes[id] = &saveNode{ID: id, Tag: tag, Parent: parent, Data: data}
	t.Order = append(t.Order, id)
	if parent == "" {
		t.Root = id
	}
}

func (t *saveTree) depth(id string) int {
	n := 0
	for id != "" && id != t.Root {
		node := t.Nodes[id]
		if node == nil {
			break
		}
		id = node.Parent
		n++
	}
	return n
}

func (t *saveTree) sizeAt(level int) int {
	n := 0
	for id := range t.Nodes {
		if t.depth(id) == level {
			n++
		}
	}
	return n
}

func (t *saveTree) children(id string) []*saveNode {
	var out []*saveNode
	for _, nid := range t.Order {
		if t.Nodes[nid].Parent == id {
			out = append(out, t.Nodes[nid])
		}
	}
	return out
}

func (t *saveTree) all() []*saveNode {
	out := make([]*saveNode, 0, len(t.Order))
	for _, id := range t.Order {
		out = append(out, t.Nodes[id])
	}
	return out
}

func (t *saveTree) merge(parent string, other *saveTree) {
	if other == nil {
		return
	}
	for _, id := range other.Order {
		n := other.Nodes[id]
		if id == other.Root {
			continue
		}
		p := n.Parent
		if p == other.Root {
			p = parent
		}
		t.create(n.Tag, n.ID, p, n.Data)
	}
}

func (t *saveTree) String() string {
	if t == nil || t.Root == "" {
		return ""
	}
	var b strings.Builder
	t.write(&b, t.Root, "", true)
	return strings.TrimRight(b.String(), "\n")
}

func (t *saveTree) write(b *strings.Builder, id, prefix string, last bool) {
	node := t.Nodes[id]
	if node == nil {
		return
	}
	if id == t.Root {
		fmt.Fprintf(b, "%s\n", node.Tag)
	} else {
		branch := "├── "
		next := "│   "
		if last {
			branch = "└── "
			next = "    "
		}
		fmt.Fprintf(b, "%s%s%s\n", prefix, branch, node.Tag)
		prefix += next
	}
	kids := t.children(id)
	for i, child := range kids {
		t.write(b, child.ID, prefix, i == len(kids)-1)
	}
}
