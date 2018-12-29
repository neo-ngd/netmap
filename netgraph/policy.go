package netgraph

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"sort"
	"strings"

	"github.com/pkg/errors"
)

const (
	Separator   = "/"
	NodesBucket = ""
)

type (
	Policy struct {
		Size       int64
		ReplFactor int
		NodeCount  int
	}

	Bucket struct {
		Key      string
		Value    string
		nodes    Int32Slice
		children []Bucket
	}

	Int32Slice []int32
	FilterFunc func([]int32) []int32
)

func (p Int32Slice) Len() int           { return len(p) }
func (p Int32Slice) Less(i, j int) bool { return p[i] < p[j] }
func (p Int32Slice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

func (b *Bucket) FindGraph(rnd *rand.Rand, ss ...Selector) (c *Bucket) {
	var g *Bucket

	c = &Bucket{Key: b.Key, Value: b.Value}
	for _, s := range ss {
		if g = b.findGraph(rnd, s.Selectors, s.Filters); g == nil {
			return nil
		}
		c.Merge(*g)
	}
	return
}

func (b *Bucket) findGraph(rnd *rand.Rand, ss []Select, fs []Filter) (c *Bucket) {
	if c = b.GetMaxSelection(ss, fs); c != nil {
		return c.GetSelection(ss, rnd)
	}
	return
}

func (b *Bucket) FindNodes(rnd *rand.Rand, ss ...Selector) (nodes []int32) {
	for _, s := range ss {
		nodes = merge(nodes, b.findNodes(rnd, s.Selectors, s.Filters))
	}
	return
}

func (b *Bucket) findNodes(rnd *rand.Rand, ss []Select, fs []Filter) []int32 {
	var c *Bucket

	if c = b.GetMaxSelection(ss, fs); c != nil {
		if c = c.GetSelection(ss, rnd); c != nil {
			return c.Nodelist()
		}
	}
	return nil
}

func (b Bucket) Copy() Bucket {
	var bc = Bucket{
		Key:   b.Key,
		Value: b.Value,
	}
	if b.nodes != nil {
		bc.nodes = make(Int32Slice, len(b.nodes))
		copy(bc.nodes, b.nodes)
	}
	if b.children != nil {
		bc.children = make([]Bucket, 0, len(b.children))
		for i := 0; i < len(b.children); i++ {
			bc.children = append(bc.children, b.children[i].Copy())
		}
	}

	return bc
}

// IsValid checks if bucket is well-formed:
// - all nodes contained in sub-bucket must belong to this
// - there must be no nodes belonging to 2 buckets
func (b Bucket) IsValid() bool {
	var (
		ns    Int32Slice
		nodes = make(Int32Slice, 0, len(b.nodes))
	)

	if len(b.children) == 0 {
		return true
	}

	for _, c := range b.children {
		if !c.IsValid() {
			return false
		}
		nodes = append(nodes, c.nodes...)
	}

	sort.Sort(nodes)
	ns = intersect(nodes, b.nodes)
	return len(nodes) == len(ns)
}

func (b Bucket) findForbidden(fs []Filter) (forbidden []int32) {
	// if root does not satisfy any filter it must be forbidden
	for _, f := range fs {
		if b.Key == f.Key && !f.Check(b) {
			return b.nodes
		}
	}

	for _, c := range b.children {
		forbidden = union(forbidden, c.findForbidden(fs))
	}
	return
}

// filterSubtree returns Bucket which contains only nodes,
// satisfying specified filter.
// If Bucket contains 0 nodes, nil is returned.
func (b Bucket) filterSubtree(filter FilterFunc) *Bucket {
	var (
		root Bucket
		r    *Bucket
	)

	root.Key = b.Key
	root.Value = b.Value
	if len(b.children) == 0 {
		if filter != nil {
			root.nodes = filter(b.nodes)
		} else {
			root.nodes = b.nodes
		}
		if len(root.nodes) != 0 {
			return &root
		}
		return nil
	}

	for _, c := range b.children {
		if r = c.filterSubtree(filter); r != nil {
			root.nodes = append(root.nodes, r.nodes...)
			root.children = append(root.children, *r)
		}
	}
	if len(root.nodes) > 0 {
		sort.Sort(root.nodes)
		return &root
	}
	return nil
}


func (b Bucket) getMaxSelection(ss []Select, filter FilterFunc) (*Bucket, int32) {
	return b.getMaxSelectionC(ss, filter, true)
}

func (b Bucket) getMaxSelectionC(ss []Select, filter FilterFunc, cut bool) (*Bucket, int32) {
	var (
		root     Bucket
		r        *Bucket
		sel      []Select
		count, n int32
		cutc     bool
	)

	if len(ss) == 0 || ss[0].Key == NodesBucket {
		if r = b.filterSubtree(filter); r != nil {
			if count = int32(len(r.nodes)); len(ss) == 0 || ss[0].Count <= count {
				return r, count
			}
		}
		return nil, 0
	}

	root.Key = b.Key
	root.Value = b.Value
	for _, c := range b.children {
		sel = ss
		if cutc = c.Key == ss[0].Key; cutc {
			sel = ss[1:]
		}
		if r, n = c.getMaxSelectionC(sel, filter, cutc); r != nil {
			root.children = append(root.children, *r)
			root.nodes = append(root.nodes, r.Nodelist()...)
			if cutc {
				count++
			} else {
				count += n
			}
		}
	}

	if (!cut && count != 0) || count >= ss[0].Count {
		sort.Sort(root.nodes)
		return &root, count

	}
	return nil, 0
}

// GetMaxSelection returns 'maximal container' -- subgraph which contains
// any other subgraph satisfying specified selects and filters.
func (b Bucket) GetMaxSelection(ss []Select, fs []Filter) (r *Bucket) {
	var (
		forbidden = b.findForbidden(fs)
		forbidMap = make(map[int32]struct{}, len(forbidden))
	)

	for _, c := range forbidden {
		forbidMap[c] = struct{}{}
	}

	r, _ = b.getMaxSelection(ss, func(nodes []int32) []int32 {
		return diff(nodes, forbidMap)
	})
	return
}

func (b Bucket) GetSelection(ss []Select, rnd *rand.Rand) *Bucket {
	var (
		root     = Bucket{Key: b.Key, Value: b.Value}
		r        *Bucket
		count, c int
		cs       []Bucket
	)

	if len(ss) == 0 {
		root.nodes = b.nodes
		root.children = b.children
		return &root
	}

	count = int(ss[0].Count)
	if ss[0].Key == NodesBucket {
		root.nodes = make(Int32Slice, len(b.nodes))
		copy(root.nodes, b.nodes)

		if rnd != nil {
			rnd.Shuffle(len(root.nodes), func(i, j int) {
				root.nodes[i], root.nodes[j] = root.nodes[j], root.nodes[i]
			})
		}
		root.nodes = root.nodes[:count]
		return &root
	}

	cs = getChildrenByKey(b, ss[0])
	if rnd != nil {
		rnd.Shuffle(len(cs), func(i, j int) {
			cs[i], cs[j] = cs[j], cs[i]
		})
	}
	for i := 0; i < len(cs); i++ {
		if r = cs[i].GetSelection(ss[1:], rnd); r != nil {
			root.Merge(*b.combine(r))
			if c++; c == count {
				return &root
			}
		}
	}
	return nil
}

func (b Bucket) combine(b1 *Bucket) *Bucket {
	if b.Equals(*b1) {
		return b1
	}

	var r *Bucket
	for _, c := range b.children {
		if r = c.combine(b1); r != nil {
			return &Bucket{
				Key:      b.Key,
				Value:    b.Value,
				nodes:    r.nodes,
				children: []Bucket{*r},
			}
		}
	}
	return nil
}

func (b Bucket) checkConflicts(b1 Bucket) bool {
	for _, n := range b1.nodes {
		if !contains(b.nodes, n) {
			continue
		}
		for _, c := range b.children {
			check := false
			if contains(c.nodes, n) {
				for _, c1 := range b1.children {
					if contains(c1.nodes, n) && (c.Key != c1.Key || c.Value != c1.Value) {
						return true
					}
					if c.Key == c1.Key && c.Value == c1.Value && !check && c.checkConflicts(c1) {
						return true
					}
					check = true
				}
			}
		}
	}
	return false
}

func (b *Bucket) Merge(b1 Bucket) {
	b.nodes = merge(b.nodes, b1.nodes)

loop:
	for _, c1 := range b1.children {
		for i := range b.children {
			if b.children[i].Equals(c1) {
				b.children[i].Merge(c1)
				continue loop
			}
		}
		b.children = append(b.children, c1)
	}
	sort.Sort(b.nodes)
}

func (b *Bucket) updateIndices(tr map[int32]int32) Bucket {
	var (
		children = make([]Bucket, 0, len(b.children))
		nodes    = make(Int32Slice, 0, len(b.nodes))
	)

	for i := range b.children {
		children = append(children, b.children[i].updateIndices(tr))
	}
	for i := range b.nodes {
		nodes = append(nodes, tr[b.nodes[i]])
	}
	sort.Sort(nodes)

	return Bucket{
		Key:      b.Key,
		Value:    b.Value,
		children: children,
		nodes:    nodes,
	}
}

func getChildrenByKey(b Bucket, s Select) []Bucket {
	buckets := make([]Bucket, 0, 10)
	for _, c := range b.children {
		if s.Key == c.Key {
			buckets = append(buckets, c)
		} else {
			buckets = append(buckets, getChildrenByKey(c, s)...)
		}
	}
	return buckets
}

// Writes Bucket with this byte structure
// [lnName][Name][lnNodes][Node1]...[NodeN][lnSubprops][sub1]...[subN]
func (b Bucket) Write(w io.Writer) error {
	var err error

	// writing name
	if err = binary.Write(w, binary.BigEndian, int32(len(b.Key)+len(b.Value)+1)); err != nil {
		return err
	}
	if err = binary.Write(w, binary.BigEndian, []byte(b.Name())); err != nil {
		return err
	}

	// writing nodes
	if err = binary.Write(w, binary.BigEndian, int32(len(b.nodes))); err != nil {
		return err
	}
	if err = binary.Write(w, binary.BigEndian, b.nodes); err != nil {
		return err
	}

	if err = binary.Write(w, binary.BigEndian, int32(len(b.children))); err != nil {
		return err
	}
	for i := range b.children {
		if err = b.children[i].Write(w); err != nil {
			return err
		}
	}

	return nil
}

func (b *Bucket) Read(r io.Reader) error {
	var ln int32
	var err error
	if err = binary.Read(r, binary.BigEndian, &ln); err != nil {
		return err
	}
	name := make([]byte, ln)
	lnE, err := r.Read(name)
	if err != nil {
		return err
	}
	if int32(lnE) != ln {
		return errors.New("unmarshaller error: cannot read name")
	}

	b.Key, b.Value, _ = splitKV(string(name))

	// reading node list
	if err = binary.Read(r, binary.BigEndian, &ln); err != nil {
		return err
	}
	if ln > 0 {
		b.nodes = make([]int32, ln)
		if err = binary.Read(r, binary.BigEndian, &b.nodes); err != nil {
			return err
		}
	}

	if err = binary.Read(r, binary.BigEndian, &ln); err != nil {
		return err
	}
	if ln > 0 {
		b.children = make([]Bucket, ln)
		for i := range b.children {
			if err = b.children[i].Read(r); err != nil {
				return err
			}
		}
	}

	return nil
}

func (b Bucket) MarshalBinary() ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := b.Write(buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (b *Bucket) UnmarshalBinary(data []byte) (err error) {
	buf := bytes.NewBuffer(data)
	if err = b.Read(buf); err == io.EOF {
		return nil
	}
	return
}

func (b Bucket) Name() string {
	return b.Key + ":" + b.Value
}

func (b *Bucket) fillNodes() {
	r := b.nodes
	for _, c := range b.children {
		c.fillNodes()
		r = merge(r, c.Nodelist())
	}
	b.nodes = r
}

func (b Bucket) Nodelist() (r []int32) {
	if b.nodes != nil || len(b.children) == 0 {
		return b.nodes
	}

	for _, c := range b.children {
		r = merge(r, c.Nodelist())
	}
	return
}

func (b Bucket) Children() []Bucket {
	return b.children
}

func (b *Bucket) AddNode(n int32, opts ...string) error {
	for _, o := range opts {
		if err := b.AddBucket(o, []int32{n}); err != nil {
			return err
		}
	}
	return nil
}

func splitKV(s string) (string, string, error) {
	kv := strings.SplitN(s, ":", 2)
	if len(kv) != 2 {
		return "", "", errors.New("wrong format")
	}
	return kv[0], kv[1], nil
}

func (b Bucket) GetNodesByOption(opts ...string) []int32 {
	var nodes []int32
	for _, opt := range opts {
		nodes = intersect(nodes, getNodes(b, splitProps(opt[1:])))
	}
	return nodes
}

func (b *Bucket) addNodes(bs []Bucket, n []int32) error {
	b.nodes = merge(b.nodes, n)
	if len(bs) == 0 {
		return nil
	}

	for i := range b.children {
		if bs[0].Equals(b.children[i]) {
			return b.children[i].addNodes(bs[1:], n)
		}
	}
	b.children = append(b.children, makeTreeProps(bs, n))
	return nil
}

func (b *Bucket) AddBucket(o string, n []int32) error {
	if o != Separator && (!strings.HasPrefix(o, Separator) || strings.HasSuffix(o, Separator)) {
		return errors.Errorf("must start and not end with '%s'", Separator)
	}

	return b.addNodes(splitProps(o[1:]), n)
}

func (b *Bucket) AddChild(c Bucket) {
	b.nodes = merge(b.nodes, c.nodes)
	b.children = append(b.children, c)
}

func splitProps(o string) []Bucket {
	ss := strings.Split(o, Separator)
	props := make([]Bucket, 0, 10)
	for _, s := range ss {
		k, v, _ := splitKV(s)
		props = append(props, Bucket{Key: k, Value: v})
	}
	return props
}

func merge(a, b []int32) []int32 {
	if a == nil {
		return b
	}

	la, lb := len(a), len(b)
	c := make([]int32, 0, la+lb)
loop:
	for i, j := 0, 0; i < la || j < lb; {
		switch true {
		case i == la:
			c = append(c, b[j:]...)
			break loop
		case j == lb:
			c = append(c, a[i:]...)
			break loop
		case a[i] < b[j]:
			c = append(c, a[i])
			i++
		case a[i] > b[j]:
			c = append(c, b[j])
			j++
		default:
			c = append(c, a[i])
			i++
			j++
		}
	}

	return c
}

func makeTreeProps(bs []Bucket, n []int32) Bucket {
	bs[0].nodes = n
	for i := len(bs) - 1; i > 0; i-- {
		bs[i].nodes = n
		bs[i-1].children = []Bucket{bs[i]}
	}
	return bs[0]
}

func (b Bucket) Equals(b1 Bucket) bool {
	return b.Key == b1.Key && b.Value == b1.Value
}