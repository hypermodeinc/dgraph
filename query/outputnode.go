/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/geojson"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/algo"
	gqlSchema "github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/task"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/types/facets"
	"github.com/hypermodeinc/dgraph/v25/x"
)

// ToJson converts the list of subgraph into a JSON response by calling toFastJSON.
func ToJson(ctx context.Context, l *Latency, sgl []*SubGraph, field gqlSchema.Field) ([]byte,
	error) {
	sgr := &SubGraph{}
	for _, sg := range sgl {
		if sg.Params.Alias == "var" || sg.Params.Alias == "shortest" {
			continue
		}
		if sg.Params.GetUid {
			sgr.Params.GetUid = true
		}
		sgr.Children = append(sgr.Children, sg)
	}
	data, err := sgr.toFastJSON(ctx, l, field)

	// don't log or wrap GraphQL errors
	if x.IsGqlErrorList(err) {
		return data, err
	}
	if err != nil {
		glog.Errorf("while running ToJson: %v\n", err)
	}
	return data, errors.Wrapf(err, "while running ToJson")
}

// We are capping maxEncoded size to 4GB, as grpc encoding fails
// for a response size > math.MaxUint32.
const maxEncodedSize = uint64(4 << 30)

type encoder struct {
	// attrMap has mapping of string predicates to uint16 ids.
	// For each predicate one unique id is assigned to save space.
	attrMap map[string]uint16
	// idSlice contains mapping from predicate id to predicate.
	idSlice []string
	// arena is used to store scalarVal for fastJsonNodes. Offset of scalarVal inside arena buffer
	// is stored in fastJsonNode meta.
	arena *arena
	// curSize is current estimated size of the encoded response. It should be less than actual
	// response size after encoding. If curSize exceeds a threshold size(maxEncodedSize), we return
	// query response with error saying response is too big. Currently curSize tracking has been
	// kept very simple. curSize is crossing threshold value or not is only checked at leaf(scalar)
	// nodes as of now. curSize is updated in following cases:
	// 1. By adding predicate len, while expanding it for an uid in preTraverse().
	// 2. By adding scalarVal len in setScalarVal function for a leaf(scalar) node.
	// TODO(Ashish): currently we are not including facets/groupby/aggregations fields in curSize
	// for simplicity. curSize can be made more accurate by adding these fields.
	curSize uint64

	// Allocator for nodes.
	alloc *z.Allocator

	// Cache uid attribute, which is very commonly used.
	uidAttr uint16

	// buf is the buffer which stores the JSON encoded response
	buf *bytes.Buffer
}

type node struct {
	// meta stores meta information for a fastJsonNode in an uint64. Layout is as follows.
	// Bytes 4-1 contains offset(uint32) for Arena.
	// Bytes 7-6 contains attr.
	// Bit MSB(first bit in Byte-8) contains list field value.
	// Bit SecondMSB(second bit in Byte-8) contains facetsParent field value.
	// Bit ThirdMSB(third bit in Byte-8) stores if the node contains uid value
	// Bit FourthMSB(fourth bit in Byte-8) stores if the order of node's children has been fixed.
	// Bit FifthMSB(fifth bit in Byte-8) stores if node contains value for a @custom GraphQL field.
	// Byte-5 is not getting used as of now.
	// |-----------------------------------------------------------------------------|
	// |             8              |    7   |    6   |    5   |  4  |  3  |  2 |  1 |
	// |-----------------------------------------------------------------------------|
	// | MSB - list                 |                 | Unused |                     |
	// | SecondMSB - facetsParent   |     Attr ID     | For    | Offset inside Arena |
	// | ThirdMSB - uid             |                 | Now    |                     |
	// | FourthMSB - Order Info     |                 |        |                     |
	// | FifthMSB - @custom GraphQL |                 |        |                     |
	// |-----------------------------------------------------------------------------|
	meta uint64

	next  *node
	child *node
}

var nodeSize = int(unsafe.Sizeof(node{}))

func newEncoder() *encoder {
	idSlice := make([]string, 1)

	a := (arenaPool.Get()).(*arena)
	a.reset()

	e := &encoder{
		attrMap: make(map[string]uint16),
		idSlice: idSlice,
		arena:   a,
		alloc:   z.NewAllocator(4<<10, "OutputNode.Encoder"),
		buf:     &bytes.Buffer{},
	}
	e.uidAttr = e.idForAttr("uid")
	return e
}

// Sort the given fastJson list
func (enc *encoder) MergeSort(headRef *fastJsonNode) {
	if headRef == nil || (*headRef).next == nil {
		return
	}

	var a, b fastJsonNode
	frontBackSplit(*headRef, &a, &b)
	enc.MergeSort(&a)
	enc.MergeSort(&b)
	*headRef = enc.mergeSortedLists(a, b)
}

func (enc *encoder) mergeSortedLists(a fastJsonNode, b fastJsonNode) fastJsonNode {
	var result fastJsonNode

	if a == nil {
		return b
	} else if b == nil {
		return a
	}

	if enc.less(a, b) {
		result = a
		result.next = enc.mergeSortedLists(a.next, b)
	} else {
		result = b
		result.next = enc.mergeSortedLists(a, b.next)
	}
	return result
}

func (enc *encoder) less(i fastJsonNode, j fastJsonNode) bool {
	attri := enc.getAttr(i)
	attrj := enc.getAttr(j)
	return strings.Compare(enc.attrForID(attri), enc.attrForID(attrj)) <= 0
}

func frontBackSplit(source fastJsonNode,
	frontRef *fastJsonNode, backRef *fastJsonNode) {
	slow := source
	fast := source.next

	for fast != nil {
		fast = fast.next
		if fast != nil {
			slow = slow.next
			fast = fast.next
		}
	}

	*frontRef = source
	*backRef = slow.next
	slow.next = nil
}

func (enc *encoder) idForAttr(attr string) uint16 {
	if attr == "uid" && enc.uidAttr > 0 {
		return enc.uidAttr
	}
	if id, ok := enc.attrMap[attr]; ok {
		return id
	}

	enc.idSlice = append(enc.idSlice, attr)
	enc.attrMap[attr] = uint16(len(enc.idSlice) - 1) // TODO(Ashish): check for overflow.
	return uint16(len(enc.idSlice) - 1)
}

func (enc *encoder) attrForID(id uint16) string {
	// For now we are not returning error from here.
	if id == 0 || id >= uint16(len(enc.idSlice)) {
		return ""
	}

	return enc.idSlice[id]
}

// makeScalarNode returns a fastJsonNode with all of its meta data, scalarVal populated.
func (enc *encoder) makeScalarNode(attr uint16, val []byte, list bool) (fastJsonNode, error) {
	fj := enc.newNode(attr)
	if err := enc.setScalarVal(fj, val); err != nil {
		return nil, err
	}
	enc.setList(fj, list)

	return fj, nil
}

func (enc *encoder) makeUidNode(attr uint16, uid uint64) (*node, error) {
	fj := enc.newNode(attr)
	fj.meta |= uidNodeBit

	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], uid)

	if err := enc.setScalarVal(fj, tmp[:]); err != nil {
		return nil, err
	}
	return fj, nil
}

// makeCustomNode returns a fastJsonNode that stores the given val for a @custom GraphQL field.
func (enc *encoder) makeCustomNode(attr uint16, val []byte) (fastJsonNode, error) {
	fj := enc.newNode(attr)
	if err := enc.setScalarVal(fj, val); err != nil {
		return nil, err
	}
	enc.setCustom(fj)

	return fj, nil
}

const (
	// Value with most significant bit set to 1.
	listBit = 1 << 63
	// Value with second most significant bit set to 1.
	facetsBit = 1 << 62
	// Value with third most significant bit set to 1.
	uidNodeBit = 1 << 61
	// Node has been visited for fixing the children order.
	visitedBit = 1 << 60
	// customBit is a value with fifth most significant bit set to 1. If a node has customBit set
	// in its meta, it means that node stores the value for a @custom GraphQL field.
	customBit = 1 << 59

	// Value with all bits set to 1 for bytes 7 and 6.
	setBytes76 = uint64(0x00FFFF0000000000)
	// Compliment value of setBytes76.
	unsetBytes76 = ^setBytes76
	// Value with all bits set to 1 for bytes 4 to 1.
	setBytes4321 = 0x00000000FFFFFFFF
)

// fastJsonNode represents node of a tree, which is formed to convert a subgraph into json response
// for a query. A fastJsonNode has following meta data:
//  1. Attr => predicate associated with this node.
//  2. ScalarVal => Any value associated with node, if it is a leaf node.
//  3. List => Stores boolean value, true if this node is part of list.
//  4. FacetsParent => Stores boolean value, true if this node is a facetsParent. facetsParent is
//     node which is parent for facets values for a scalar list predicate. Eg: node "city|country"
//     will have FacetsParent value as true.
//     {
//     "city": ["Bengaluru", "San Francisco"],
//     "city|country": {
//     "0": "india",
//     "1": "US"
//     }
//     }
//  5. Children(Attrs) => List of all children.
//  6. Visited => Stores boolen values, true if node has been visited for fixing children's order.
//
// All of the data for fastJsonNode tree is stored in encoder to optimise memory usage. fastJsonNode
// struct is pointer to node object. node object stores below information.
// 1. meta information.
// 2. Pointer to its first child.
// 3. Pointer to its sibling.
type fastJsonNode *node

// newNode returns a fastJsonNode with its attr set to attr,
// and all other meta set to their default value.
func (enc *encoder) newNode(attr uint16) fastJsonNode {
	b := enc.alloc.AllocateAligned(nodeSize)
	n := (*node)(unsafe.Pointer(&b[0]))
	enc.setAttr(n, attr)
	return n
}

func (enc *encoder) setAttr(fj fastJsonNode, attr uint16) {
	// There can be some cases where we change name of attr for fastJsoNode and
	// hence first clear the existing attr, then store new one.
	fj.meta &= unsetBytes76
	fj.meta |= (uint64(attr) << 40)
}

func (enc *encoder) setScalarVal(fj fastJsonNode, sv []byte) error {
	offset, err := enc.arena.put(sv)
	if err != nil {
		return err
	}
	fj.meta |= uint64(offset)

	// Also increase curSize.
	enc.curSize += uint64(len(sv))
	if size := uint64(enc.alloc.Size()) + enc.curSize; size > maxEncodedSize {
		return fmt.Errorf("estimated response size: %d is bigger than threshold: %d",
			size, maxEncodedSize)
	}
	return nil
}

func (enc *encoder) setList(fj fastJsonNode, list bool) {
	if list {
		fj.meta |= listBit
	} else {
		fj.meta &^= listBit
	}
}

func (enc *encoder) setVisited(fj fastJsonNode, visited bool) {
	if visited {
		fj.meta |= visitedBit
	} else {
		fj.meta &^= visitedBit
	}
}

func (enc *encoder) setFacetsParent(fj fastJsonNode) {
	fj.meta |= facetsBit
}

func (enc *encoder) setCustom(fj fastJsonNode) {
	fj.meta |= customBit
}

//nolint:unused // appendAttrs is used in outputnode_test.go as a helper function
func (enc *encoder) appendAttrs(fj, child fastJsonNode) {
	enc.addChildren(fj, child)
}

// addChildren appends attrs to existing fj's attrs.
func (enc *encoder) addChildren(fj, head fastJsonNode) {
	if fj.child == nil {
		fj.child = head
		return
	}

	tail := head
	for tail.next != nil {
		tail = tail.next
	}

	// We're inserting the node in between. This would need to be fixed later via fixOrder.
	// Single child additions:
	// Child 1
	// Child 2 -> 1
	// Child 3 -> 2 -> 1
	// Child 4 -> 3 -> 2 -> 1
	// Child 5 -> 4 -> 3 -> 2 -> 1
	//
	// If child has siblings, then it could look like this.
	// addChildren(13 -> 12 -> 11)
	// Child 5 -> 4 -> 3 -> 2 -> 1
	//
	// What we want:
	// 13 -> 12 -> 11 -> 5 -> 4 -> 3 -> 2 -> 1
	fj.child, tail.next = head, fj.child
}

// fixOrder would recursively fix the ordering issue caused by addChildren, across the entire
// tree.
// fixOrder would fix the order from
// 5 -> 4 -> 3 -> 2 -> 1 to
// 1 -> 2 -> 3 -> 4 -> 5
func (enc *encoder) fixOrder(fj fastJsonNode) {
	// If you call this again on the same fastJsonNode, then this would become wrong.  Due to
	// children being copied over, the same node can be referenced by multiple nodes. Thus, the node
	// would be visited again, it would be fixed multiple times, causing ordering issue.
	// To avoid this, we keep track of the node by marking it.
	if (fj.meta & visitedBit) > 0 {
		return
	}
	enc.setVisited(fj, true)

	tail := fj.child // This is node 5 in the chain mentioned above.
	// Edge cases: Child is nil, or only child.
	if tail == nil {
		return
	}

	if tail.next == nil {
		enc.fixOrder(tail)
		return
	}

	left, right := tail, tail.next // Left is 5, right is 4.
	left.next = nil                // Make left the last child.
	for right != nil {
		next := right.next        // right of ptr2 (points to 3)
		right.next = left         // ptr2 now points left to ptr1 (4 -> 5)
		left, right = right, next // Advance both pointers (left = 4, right = 3 and so on)
	}
	// left is now pointing to 1.
	fj.child = left // Child is now pointed to 1.

	// Now recurse to fix up all children.
	child := fj.child
	for child != nil {
		enc.fixOrder(child)
		child = child.next
	}
}

func (enc *encoder) getAttr(fj fastJsonNode) uint16 {
	return uint16((fj.meta & setBytes76) >> 40)
}

func (enc *encoder) getScalarVal(fj fastJsonNode) ([]byte, error) {
	offset := uint32(fj.meta & setBytes4321)
	data, err := enc.arena.get(offset)
	if err != nil {
		return nil, err
	}
	if (fj.meta & uidNodeBit) > 0 {
		uid := binary.BigEndian.Uint64(data)
		return x.ToHex(uid, false), nil
	}
	return data, nil
}

func (enc *encoder) getList(fj fastJsonNode) bool {
	return (fj.meta & listBit) > 0
}

func (enc *encoder) getFacetsParent(fj fastJsonNode) bool {
	return (fj.meta & facetsBit) > 0
}

func (enc *encoder) getCustom(fj fastJsonNode) bool {
	return (fj.meta & customBit) > 0
}

func (enc *encoder) children(fj fastJsonNode) fastJsonNode {
	// Return nil if no attrs are found.
	return fj.child
}

func (enc *encoder) AddValue(fj fastJsonNode, attr uint16, v types.Val) error {
	return enc.AddListValue(fj, attr, v, false)
}

func (enc *encoder) AddListValue(fj fastJsonNode, attr uint16, v types.Val, list bool) error {
	if v.Tid == types.VFloatID {
		for _, f := range v.Value.([]float32) {
			bs := []byte(strconv.FormatFloat(float64(f), 'E', -1, 32))
			sn, err := enc.makeScalarNode(attr, bs, true)
			if err != nil {
				return err
			}

			enc.addChildren(fj, sn)
		}
		return nil
	}
	bs, err := valToBytes(v)
	if err != nil {
		return nil // Ignore this.
	}
	sn, err := enc.makeScalarNode(attr, bs, list)
	if err != nil {
		return err
	}

	enc.addChildren(fj, sn)
	return nil
}

func (enc *encoder) AddMapChild(fj, val fastJsonNode) {
	var childNode fastJsonNode
	child := enc.children(fj)
	for child != nil {
		if enc.getAttr(child) == enc.getAttr(val) {
			childNode = child
			break
		}
		child = child.next
	}

	if childNode == nil {
		enc.addChildren(fj, val)
	} else {
		enc.addChildren(childNode, enc.children(val))
	}
}

func (enc *encoder) AddListChild(fj, child fastJsonNode) {
	enc.setList(child, true)
	enc.addChildren(fj, child)
}

func (enc *encoder) SetUID(fj fastJsonNode, uid uint64, attr uint16) error {
	// if we're in debug mode, uid may be added second time, skip this
	if attr == enc.uidAttr {
		fjAttrs := enc.children(fj)
		for fjAttrs != nil {
			if enc.getAttr(fjAttrs) == attr {
				return nil
			}
			fjAttrs = fjAttrs.next
		}
	}

	un, err := enc.makeUidNode(attr, uid)
	if err != nil {
		return err
	}
	enc.addChildren(fj, un)
	return nil
}

func (enc *encoder) IsEmpty(fj fastJsonNode) bool {
	return fj.child == nil
}

var (
	boolTrue  = []byte("true")
	boolFalse = []byte("false")

	// Below variables are used in stringJsonMarshal function.
	bufferPool = sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}

	hex        = "0123456789abcdef"
	escapeHTML = true
)

// stringJsonMarshal is replacement for json.Marshal() function only for string type.
// This function is encodeState.string(string, escapeHTML) in "encoding/json/encode.go".
// It should be in sync with encodeState.string function.
func stringJsonMarshal(s string) []byte {
	e := bufferPool.Get().(*bytes.Buffer)
	e.Reset()

	e.WriteByte('"')
	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			if htmlSafeSet[b] || (!escapeHTML && safeSet[b]) {
				i++
				continue
			}
			if start < i {
				e.WriteString(s[start:i])
			}
			e.WriteByte('\\')
			switch b {
			case '\\', '"':
				e.WriteByte(b)
			case '\n':
				e.WriteByte('n')
			case '\r':
				e.WriteByte('r')
			case '\t':
				e.WriteByte('t')
			default:
				// This encodes bytes < 0x20 except for \t, \n and \r.
				// If escapeHTML is set, it also escapes <, >, and &
				// because they can lead to security holes when
				// user-controlled strings are rendered into JSON
				// and served to some browsers.
				e.WriteString(`u00`)
				e.WriteByte(hex[b>>4])
				e.WriteByte(hex[b&0xF])
			}
			i++
			start = i
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		if c == utf8.RuneError && size == 1 {
			if start < i {
				e.WriteString(s[start:i])
			}
			e.WriteString(`\ufffd`)
			i += size
			start = i
			continue
		}
		// U+2028 is LINE SEPARATOR.
		// U+2029 is PARAGRAPH SEPARATOR.
		// They are both technically valid characters in JSON strings,
		// but don't work in JSONP, which has to be evaluated as JavaScript,
		// and can lead to security holes there. It is valid JSON to
		// escape them, so we do so unconditionally.
		// See http://timelessrepo.com/json-isnt-a-javascript-subset for discussion.
		if c == '\u2028' || c == '\u2029' {
			if start < i {
				e.WriteString(s[start:i])
			}
			e.WriteString(`\u202`)
			e.WriteByte(hex[c&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	if start < len(s) {
		e.WriteString(s[start:])
	}
	e.WriteByte('"')
	buf := append([]byte(nil), e.Bytes()...)
	bufferPool.Put(e)
	return buf
}

func valToBytes(v types.Val) ([]byte, error) {
	switch v.Tid {
	case types.StringID, types.DefaultID:
		switch str := v.Value.(type) {
		case string:
			return stringJsonMarshal(str), nil
		default:
			return json.Marshal(str)
		}
	case types.BinaryID:
		return []byte(fmt.Sprintf("%q", v.Value)), nil
	case types.IntID:
		// In types.Convert(), we always convert to int64 for IntID type. fmt.Sprintf is slow
		// and hence we are using strconv.FormatInt() here. Since int64 and int are most common int
		// types we are using FormatInt for those.
		switch num := v.Value.(type) {
		case int64:
			return []byte(strconv.FormatInt(num, 10)), nil
		case int:
			return []byte(strconv.FormatInt(int64(num), 10)), nil
		default:
			return []byte(fmt.Sprintf("%d", v.Value)), nil
		}
	case types.FloatID:
		f, fOk := v.Value.(float64)

		// +Inf, -Inf and NaN are not representable in JSON.
		// Please see https://golang.org/src/encoding/json/encode.go?s=6458:6501#L573
		if !fOk || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, errors.New("Unsupported floating point number in float field")
		}

		return []byte(fmt.Sprintf("%f", f)), nil
	case types.BoolID:
		if v.Value.(bool) {
			return boolTrue, nil
		}
		return boolFalse, nil
	case types.DateTimeID:
		t := v.Value.(time.Time)
		return marshalTimeJson(t)
	case types.GeoID:
		return geojson.Marshal(v.Value.(geom.T))
	case types.BigFloatID:
		b := v.Value.(big.Float)
		return b.MarshalText()
	case types.UidID:
		return []byte(fmt.Sprintf("\"%#x\"", v.Value)), nil
	case types.PasswordID:
		return []byte(fmt.Sprintf("%q", v.Value.(string))), nil
	case types.VFloatID:
		return json.Marshal(v.Value.([]float32))
	default:
		return nil, errors.New("Unsupported types.Val.Tid")
	}
}

// marshalTimeJson does what time.MarshalJson does along with supporting RFC3339 non compliant
// time zones in a timestamp. While go 1.20 changes the behaviour of time.MarshalJSON, we do
// not want to throw error suddenly because we can't marshal the stored data correctly any more.
func marshalTimeJson(t time.Time) ([]byte, error) {
	_, offset := t.Zone()
	// normal case
	if types.GoodTimeZone(offset) {
		return t.MarshalJSON()
	}

	// If zone >23 or <-23, we need to handle this case ourselves.
	// This is because, in go1.20, MarshalJSON fails for invalid zones.
	// We, for now, call MarshalJSON for timestamp without the zone (or making it UTC zone).
	b, err := t.Add(time.Duration(offset) * time.Second).UTC().MarshalJSON()
	if err != nil {
		return nil, err
	}

	// we will get a byte slice that has Z appended at the end along with a quote (")
	// e.g.: []byte("2018-05-28T14:41:57Z"). We replace Z with -/+.
	zone := offset / 60
	if zone < 0 {
		b[len(b)-2] = '-'
		zone = -zone
	} else {
		b[len(b)-2] = '+'
	}
	return append(b[:len(b)-1], []byte(fmt.Sprintf("%02d:%02d\"", zone/60, zone%60))...), nil
}

func (enc *encoder) writeKey(fj fastJsonNode) error {
	if _, err := enc.buf.WriteRune('"'); err != nil {
		return err
	}
	attrID := enc.getAttr(fj)
	if _, err := enc.buf.WriteString(enc.attrForID(attrID)); err != nil {
		return err
	}
	if _, err := enc.buf.WriteRune('"'); err != nil {
		return err
	}
	if _, err := enc.buf.WriteRune(':'); err != nil {
		return err
	}
	return nil
}

func (enc *encoder) attachFacets(fj fastJsonNode, fieldName string, isList bool,
	fList []*api.Facet, facetIdx int) error {

	idxFieldID := enc.idForAttr(strconv.Itoa(facetIdx))
	for _, f := range fList {
		fName := facetName(fieldName, f)
		fVal, err := facets.ValFor(f)
		if err != nil {
			return err
		}

		if !isList {
			if err := enc.AddValue(fj, enc.idForAttr(fName), fVal); err != nil {
				return err
			}
		} else {
			facetNode := enc.newNode(enc.idForAttr(fName))
			err := enc.AddValue(facetNode, idxFieldID, fVal)
			if err != nil {
				return err
			}
			// Mark this node as facetsParent.
			enc.setFacetsParent(facetNode)
			enc.AddMapChild(fj, facetNode)
		}
	}

	return nil
}

func (enc *encoder) encode(fj fastJsonNode) error {
	child := enc.children(fj)
	// This is a scalar value.
	if child == nil {
		val, err := enc.getScalarVal(fj)
		if err != nil {
			return err
		}
		_, err = enc.buf.Write(val)
		return err
	}

	// This is an internal node.
	if _, err := enc.buf.WriteRune('{'); err != nil {
		return err
	}
	cnt := 0
	var cur, next fastJsonNode
	for child != nil {
		cnt++
		validNext := false
		cur = child
		if cur.next != nil {
			next = cur.next
			validNext = true
		}

		if validNext && enc.getAttr(cur) == enc.getAttr(next) {
			if cnt == 1 {
				if err := enc.writeKey(cur); err != nil {
					return err
				}
				if _, err := enc.buf.WriteRune('['); err != nil {
					return err
				}
			}
			if err := enc.encode(cur); err != nil {
				return err
			}
		} else {
			if cnt == 1 {
				if err := enc.writeKey(cur); err != nil {
					return err
				}
				if enc.getList(cur) {
					if _, err := enc.buf.WriteRune('['); err != nil {
						return err
					}
				}
			}
			if err := enc.encode(cur); err != nil {
				return err
			}
			if cnt > 1 || enc.getList(cur) {
				if _, err := enc.buf.WriteRune(']'); err != nil {
					return err
				}
			}
			cnt = 0 // Reset the count.
		}
		// We need to print comma except for the last attribute.
		if child.next != nil {
			if _, err := enc.buf.WriteRune(','); err != nil {
				return err
			}
		}

		child = child.next
	}
	if _, err := enc.buf.WriteRune('}'); err != nil {
		return err
	}

	return nil
}

func (enc *encoder) copyFastJsonList(fj fastJsonNode) (fastJsonNode, int) {
	if fj == nil {
		return fj, 0
	}

	var head, tail fastJsonNode
	nodeCount := 0

	for fj != nil {
		nodeCount++
		nn := enc.copySingleNode(fj)
		if tail == nil {
			head, tail = nn, nn
			fj = fj.next
			continue
		}
		tail.next = nn
		fj, tail = fj.next, tail.next
	}

	return head, nodeCount
}

func (enc *encoder) copySingleNode(fj fastJsonNode) fastJsonNode {
	if fj == nil {
		return nil
	}

	nn := enc.newNode(enc.getAttr(fj))
	nn.meta = fj.meta
	nn.child = fj.child
	nn.next = nil
	return nn
}

func (enc *encoder) merge(parent, child []fastJsonNode) ([]fastJsonNode, error) {
	if len(parent) == 0 {
		return child, nil
	}

	// Here we merge two slices of maps.
	mergedList := make([]fastJsonNode, 0)
	cnt := 0
	for _, pa := range parent {
		for _, ca := range child {
			paCopy, paNodeCount := enc.copyFastJsonList(pa)
			caCopy, caNodeCount := enc.copyFastJsonList(ca)

			cnt += paNodeCount + caNodeCount
			if cnt > x.Config.LimitNormalizeNode {
				return nil, errors.Errorf(
					"Couldn't evaluate @normalize directive - too many results")
			}

			if paCopy == nil {
				paCopy = caCopy
			} else {
				temp := paCopy
				for temp.next != nil {
					temp = temp.next
				}
				temp.next = caCopy
			}
			mergedList = append(mergedList, paCopy)
		}
	}
	return mergedList, nil
}

// normalize returns all attributes of fj and its children (if any).
func (enc *encoder) normalize(fj fastJsonNode) ([]fastJsonNode, error) {
	cnt := 0
	chead := enc.children(fj)
	for chead != nil {
		// Here we are counting all non-scalar children of fj. If there are any such
		// children, we will flatten them, otherwise we will return all children.
		// We should only consider those children(of fj) for flattening which have
		// children and are not facetsParent.
		if enc.children(chead) != nil && !enc.getFacetsParent(chead) {
			cnt++
		}
		chead = chead.next
	}

	if cnt == 0 {
		// Recursion base case
		// There are no children, we can just return slice with fj.child.
		return []fastJsonNode{enc.children(fj)}, nil
	}

	parentSlice := make([]fastJsonNode, 0, 5)

	// First separate children of fj which are scalar.
	var shead, curScalar fastJsonNode
	chead = enc.children(fj)
	for chead != nil {
		if enc.children(chead) != nil && !enc.getFacetsParent(chead) {
			chead = chead.next
			continue
		}

		// Here, add all nodes which have either no children or they are facetsParent.
		copyNode := enc.copySingleNode(chead)
		if curScalar == nil {
			shead, curScalar = copyNode, copyNode
		} else {
			curScalar.next = copyNode
			curScalar = copyNode
		}

		chead = chead.next
	}

	parentSlice = append(parentSlice, shead)
	chead = enc.children(fj)
	for chead != nil {
		childNode := chead
		// Here, exclude all nodes which have either no children or they are facetsParent.
		if enc.children(childNode) == nil || enc.getFacetsParent(childNode) {
			chead = chead.next
			continue
		}

		childSlice := make([]fastJsonNode, 0, 5)
		for chead != nil && enc.getAttr(childNode) == enc.getAttr(chead) {
			childSlice = append(childSlice, enc.children(chead))
			chead = chead.next
		}

		var err error
		parentSlice, err = enc.merge(parentSlice, childSlice)
		if err != nil {
			return nil, err
		}
	}

	for i, slice := range parentSlice {
		if x.Config.NormalizeCompatibilityMode == "" {
			// sort the fastJson list. This will ensure that nodes
			// with same attribute name comes together in response
			enc.MergeSort(&parentSlice[i])
		}

		// From every list we need to remove node with attribute "uid".
		var prev, cur fastJsonNode
		cur = slice
		for cur != nil {
			if enc.getAttr(cur) == enc.uidAttr {
				if prev == nil {
					cur = cur.next
					continue
				} else {
					prev.next = cur.next
				}
			}
			prev = cur
			cur = cur.next
		}
	}

	return parentSlice, nil
}

func (sg *SubGraph) addGroupby(enc *encoder, fj fastJsonNode,
	res *groupResults, fname string) error {

	// Don't add empty groupby
	if len(res.group) == 0 {
		return nil
	}
	g := enc.newNode(enc.idForAttr(fname))
	for _, grp := range res.group {
		uc := enc.newNode(enc.idForAttr("@groupby"))
		for _, it := range grp.keys {
			if err := enc.AddValue(uc, enc.idForAttr(it.attr), it.key); err != nil {
				return err
			}
		}
		for _, it := range grp.aggregates {
			if err := enc.AddValue(uc, enc.idForAttr(it.attr), it.key); err != nil {
				return err
			}
		}
		enc.AddListChild(g, uc)
	}
	enc.AddListChild(fj, g)
	return nil
}

func (sg *SubGraph) addAggregations(enc *encoder, fj fastJsonNode) error {
	for _, child := range sg.Children {
		aggVal, ok := child.Params.UidToVal.Get(0)
		if !ok {
			if len(child.Params.NeedsVar) == 0 {
				return errors.Errorf("Only aggregated variables allowed within empty block.")
			}
			// the aggregation didn't happen, most likely was called with unset vars.
			// See: query.go:fillVars
			// In this case we do nothing. The aggregate value in response will be returned as NULL.
		}
		if child.Params.Normalize && child.Params.Alias == "" {
			continue
		}
		fieldName := child.aggWithVarFieldName()
		n1 := enc.newNode(enc.idForAttr(sg.Params.Alias))
		if err := enc.AddValue(n1, enc.idForAttr(fieldName), aggVal); err != nil {
			return err
		}
		enc.AddListChild(fj, n1)
	}
	if enc.IsEmpty(fj) {
		enc.AddListChild(fj, enc.newNode(enc.idForAttr(sg.Params.Alias)))
	}
	return nil
}

func (sg *SubGraph) handleCountUIDNodes(enc *encoder, n fastJsonNode, count int) (bool, error) {
	addedNewChild := false
	fieldName := sg.fieldName()
	sgFieldID := enc.idForAttr(fieldName)
	for _, child := range sg.Children {
		uidCount := child.Attr == "uid" && child.Params.DoCount && child.IsInternal()
		normWithoutAlias := child.Params.Alias == "" && child.Params.Normalize
		if uidCount && !normWithoutAlias {
			addedNewChild = true

			c := types.ValueForType(types.IntID)
			c.Value = int64(count)

			field := child.Params.Alias
			if field == "" {
				field = "count"
			}

			fjChild := enc.newNode(sgFieldID)
			if err := enc.AddValue(fjChild, enc.idForAttr(field), c); err != nil {
				return false, err
			}
			enc.AddListChild(n, fjChild)
		}
	}

	return addedNewChild, nil
}

func processNodeUids(fj fastJsonNode, enc *encoder, sg *SubGraph) error {
	if sg.Params.IsEmpty {
		return sg.addAggregations(enc, fj)
	}

	enc.curSize += uint64(len(sg.Params.Alias))

	attrID := enc.idForAttr(sg.Params.Alias)
	if sg.uidMatrix == nil {
		enc.AddListChild(fj, enc.newNode(attrID))
		return nil
	}

	hasChild, err := sg.handleCountUIDNodes(enc, fj, len(sg.DestUIDs.Uids))
	if err != nil {
		return err
	}
	if sg.Params.IsGroupBy {
		if len(sg.GroupbyRes) == 0 {
			return errors.Errorf("Expected GroupbyRes to have length > 0.")
		}
		return sg.addGroupby(enc, fj, sg.GroupbyRes[0], sg.Params.Alias)
	}

	lenList := len(sg.uidMatrix[0].Uids)
	for i := range lenList {
		uid := sg.uidMatrix[0].Uids[i]
		if algo.IndexOf(sg.DestUIDs, uid) < 0 {
			// This UID was filtered. So Ignore it.
			continue
		}

		n1 := enc.newNode(attrID)
		enc.setAttr(n1, enc.idForAttr(sg.Params.Alias))
		if err := sg.preTraverse(enc, uid, n1); err != nil {
			if err.Error() == "_INV_" {
				continue
			}
			return err
		}

		if enc.IsEmpty(n1) {
			continue
		}

		hasChild = true
		if !sg.Params.Normalize {
			enc.AddListChild(fj, n1)
			continue
		}

		// With the new changes we store children in reverse order(check addChildren method). This
		// leads to change of order of field responses for existing Normalize test cases. To
		// minimize the changes of existing tests case we are fixing order of node children before
		// calling normalize() on it. Also once we have fixed order for children, we don't need to
		// fix its order again. Hence mark the newly created node visited immediately.
		enc.fixOrder(n1)
		// Lets normalize the response now.
		normalized, err := enc.normalize(n1)
		if err != nil {
			return err
		}
		for _, c := range normalized {
			node := enc.newNode(attrID)
			enc.setVisited(node, true)
			enc.addChildren(node, c)
			enc.AddListChild(fj, node)
		}
	}

	if !hasChild {
		// So that we return an empty key if the root didn't have any children.
		enc.AddListChild(fj, enc.newNode(attrID))
	}
	return nil
}

// Extensions represents the extra information appended to query results.
type Extensions struct {
	Latency *api.Latency    `json:"server_latency,omitempty"`
	Txn     *api.TxnContext `json:"txn,omitempty"`
	Metrics *api.Metrics    `json:"metrics,omitempty"`
}

func (sg *SubGraph) toFastJSON(ctx context.Context, l *Latency, field gqlSchema.Field) ([]byte,
	error) {
	encodingStart := time.Now()
	defer func() {
		l.Json = time.Since(encodingStart)
	}()

	enc := newEncoder()
	defer func() {
		// Put encoder's arena back to arena pool.
		arenaPool.Put(enc.arena)
		enc.alloc.Release()
	}()

	var err error
	n := enc.newNode(enc.idForAttr("_root_"))
	for _, sg := range sg.Children {
		err = processNodeUids(n, enc, sg)
		if err != nil {
			return nil, err
		}
	}
	enc.fixOrder(n)

	// According to GraphQL spec response should only contain data, errors and extensions as top
	// level keys. Hence we send server_latency under extensions key.
	// https://facebook.github.io/graphql/#sec-Response-Format

	// if there is a GraphQL field that means we need to encode the response in GraphQL form,
	// otherwise encode it in DQL form.
	if field != nil {
		// if there were any GraphQL errors, we need to propagate them back to GraphQL layer along
		// with the data. So, don't return here if we get an error.
		err = sg.toGraphqlJSON(newGraphQLEncoder(ctx, enc), n, field)
	} else if err = sg.toDqlJSON(enc, n); err != nil {
		return nil, err
	}

	// Return error if encoded buffer size exceeds than a threshold size.
	if uint64(enc.buf.Len()) > maxEncodedSize {
		return nil, fmt.Errorf("while writing to buffer. Encoded response size: %d"+
			" is bigger than threshold: %d", enc.buf.Len(), maxEncodedSize)
	}

	return enc.buf.Bytes(), err
}

func (sg *SubGraph) toDqlJSON(enc *encoder, n fastJsonNode) error {
	if enc.children(n) == nil {
		x.Check2(enc.buf.WriteString(`{}`))
		return nil
	}
	return enc.encode(n)
}

func (sg *SubGraph) toGraphqlJSON(genc *graphQLEncoder, n fastJsonNode, f gqlSchema.Field) error {
	// GraphQL queries will always have at least one query whose results are visible to users,
	// implying that the root fastJson node will always have at least one child. So, no need
	// to check for the case where there are no children for the root fastJson node.

	// if this field has any @custom(http: {...}) children,
	// then need to resolve them first before encoding the final GraphQL result.
	genc.processCustomFields(f, n)
	// now encode the GraphQL results.
	if !genc.encode(encodeInput{
		parentField: nil,
		parentPath:  f.PreAllocatePathSlice(),
		fj:          n,
		fjIsRoot:    true,
		childSelSet: []gqlSchema.Field{f},
	}) {
		// if genc.encode() didn't finish successfully here, that means we need to send
		// data as null in the GraphQL response like this:
		// 		{
		// 			"errors": [...],
		// 			"data": null
		// 		}
		// and not just null for a single query in data.
		// So, reset the buffer contents here, so that GraphQL layer may know that if it gets
		// error of type x.GqlErrorList along with nil JSON response, then it needs to set whole
		// data as null.
		genc.buf.Reset()
	}

	if len(genc.errs) > 0 {
		return genc.errs
	}
	return nil
}

func (sg *SubGraph) fieldName() string {
	fieldName := sg.Attr
	if sg.Params.Alias != "" {
		fieldName = sg.Params.Alias
	}
	return fieldName
}

func (sg *SubGraph) addCount(enc *encoder, count uint64, dst fastJsonNode) error {
	if sg.Params.Normalize && sg.Params.Alias == "" {
		return nil
	}
	c := types.ValueForType(types.IntID)
	c.Value = int64(count)
	fieldName := sg.Params.Alias
	if fieldName == "" {
		fieldName = fmt.Sprintf("count(%s)", sg.Attr)
	}
	return enc.AddValue(dst, enc.idForAttr(fieldName), c)
}

func (sg *SubGraph) aggWithVarFieldName() string {
	if sg.Params.Alias != "" {
		return sg.Params.Alias
	}
	fieldName := fmt.Sprintf("val(%v)", sg.Params.Var)
	if len(sg.Params.NeedsVar) > 0 {
		fieldName = fmt.Sprintf("val(%v)", sg.Params.NeedsVar[0].Name)
		if sg.SrcFunc != nil {
			fieldName = fmt.Sprintf("%s(%v)", sg.SrcFunc.Name, fieldName)
		}
	}
	return fieldName
}

func (sg *SubGraph) addInternalNode(enc *encoder, uid uint64, dst fastJsonNode) error {
	sv, ok := sg.Params.UidToVal.Get(uid)
	if !ok || sv.Value == nil {
		return nil
	}
	fieldName := sg.aggWithVarFieldName()
	return enc.AddValue(dst, enc.idForAttr(fieldName), sv)
}

func (sg *SubGraph) addCheckPwd(enc *encoder, vals []*pb.TaskValue, dst fastJsonNode) error {
	c := types.ValueForType(types.BoolID)
	if len(vals) == 0 {
		c.Value = false
	} else {
		c.Value = task.ToBool(vals[0])
	}

	fieldName := sg.Params.Alias
	if fieldName == "" {
		fieldName = fmt.Sprintf("checkpwd(%s)", sg.Attr)
	}
	return enc.AddValue(dst, enc.idForAttr(fieldName), c)
}

func alreadySeen(parentIds []uint64, uid uint64) bool {
	for _, id := range parentIds {
		if id == uid {
			return true
		}
	}
	return false
}

func facetName(fieldName string, f *api.Facet) string {
	if f.Alias != "" {
		return f.Alias
	}
	return fieldName + x.FacetDelimiter + f.Key
}

// This method gets the values and children for a subprotos.
func (sg *SubGraph) preTraverse(enc *encoder, uid uint64, dst fastJsonNode) error {
	if sg.Params.IgnoreReflex {
		if alreadySeen(sg.Params.ParentIds, uid) {
			// A node can't have itself as the child at any level.
			return nil
		}
		// Push myself to stack before sending this to children.
		sg.Params.ParentIds = append(sg.Params.ParentIds, uid)
	}

	var invalidUids map[uint64]bool
	// We go through all predicate children of the subprotos.
	for _, pc := range sg.Children {
		if pc.Params.IgnoreResult {
			continue
		}
		if pc.IsInternal() {
			if pc.Params.Expand != "" {
				continue
			}
			if pc.Params.Normalize && pc.Params.Alias == "" {
				continue
			}
			if err := pc.addInternalNode(enc, uid, dst); err != nil {
				return err
			}
			continue
		}

		if len(pc.uidMatrix) == 0 {
			// Can happen in recurse query.
			continue
		}
		if len(pc.facetsMatrix) > 0 && len(pc.facetsMatrix) != len(pc.uidMatrix) {
			return errors.Errorf("Length of facetsMatrix and uidMatrix mismatch: %d vs %d",
				len(pc.facetsMatrix), len(pc.uidMatrix))
		}

		idx := algo.IndexOf(pc.SrcUIDs, uid)
		if idx < 0 {
			continue
		}
		if pc.Params.IsGroupBy {
			if len(pc.GroupbyRes) <= idx {
				return errors.Errorf("Unexpected length while adding Groupby. Idx: [%v], len: [%v]",
					idx, len(pc.GroupbyRes))
			}
			if err := pc.addGroupby(enc, dst, pc.GroupbyRes[idx], pc.fieldName()); err != nil {
				return err
			}
			continue
		}

		fieldName := pc.fieldName()
		switch {
		case len(pc.counts) > 0:
			if err := pc.addCount(enc, uint64(pc.counts[idx]), dst); err != nil {
				return err
			}

		case pc.SrcFunc != nil && pc.SrcFunc.Name == "checkpwd":
			if err := pc.addCheckPwd(enc, pc.valueMatrix[idx].Values, dst); err != nil {
				return err
			}

		case idx < len(pc.uidMatrix) && len(pc.uidMatrix[idx].Uids) > 0:
			var fcsList []*pb.Facets
			if pc.Params.Facet != nil {
				fcsList = pc.facetsMatrix[idx].FacetsList
			}

			if sg.Params.IgnoreReflex {
				pc.Params.ParentIds = sg.Params.ParentIds
			}

			// calculate it once to avoid multiple call to idToAttr()
			fieldID := enc.idForAttr(fieldName)
			// Add len of fieldName to enc.curSize.
			enc.curSize += uint64(len(fieldName))

			// We create as many predicate entity children as the length of uids for
			// this predicate.
			ul := pc.uidMatrix[idx]
			for childIdx, childUID := range ul.Uids {
				if fieldName == "" || (invalidUids != nil && invalidUids[childUID]) {
					continue
				}
				uc := enc.newNode(fieldID)
				if rerr := pc.preTraverse(enc, childUID, uc); rerr != nil {
					if rerr.Error() == "_INV_" {
						if invalidUids == nil {
							invalidUids = make(map[uint64]bool)
						}

						invalidUids[childUID] = true
						continue // next UID.
					}
					return rerr
				}

				if !enc.IsEmpty(uc) {
					if sg.Params.GetUid {
						if err := enc.SetUID(uc, childUID, enc.uidAttr); err != nil {
							return err
						}
					}

					// Add facets nodes.
					if pc.Params.Facet != nil && len(fcsList) > childIdx {
						fs := fcsList[childIdx].Facets
						if err := enc.attachFacets(uc, fieldName, false, fs, childIdx); err != nil {
							return err
						}
					}

					if pc.Params.Normalize {
						// We will normalize at each level instead of
						// calling normalize after pretraverse.
						// Now normalize() only flattens one level,
						// the expectation is that its children have
						// already been normalized.

						// TODO(ashish): Check reason for calling fixOrder() here in
						// processNodeUids(), just before calling normalize().
						enc.fixOrder(uc)
						normAttrs, err := enc.normalize(uc)
						if err != nil {
							return err
						}

						for _, c := range normAttrs {
							// Adding as list child irrespective of the type of pc
							// (list or non-list), otherwise result might be inconsistent or might
							// depend on children and grandchildren of pc. Consider the case:
							// 	boss: uid .
							// 	friend: [uid] .
							// 	name: string .
							// For query like:
							// {
							// 	me(func: uid(0x1)) {
							// 		boss @normalize {
							// 			name
							// 		}
							// 	}
							// }
							// boss will be non list type in response, but for query like:
							// {
							// 	me(func: uid(0x1)) {
							// 		boss @normalize {
							// 			friend {
							// 				name
							// 			}
							// 		}
							// 	}
							// }
							// boss should be of list type because there can be multiple friends of
							// boss.
							node := enc.newNode(fieldID)
							enc.setVisited(node, true)
							enc.addChildren(node, c)
							enc.AddListChild(dst, node)
						}
						continue
					}
					if pc.List {
						enc.AddListChild(dst, uc)
					} else {
						enc.AddMapChild(dst, uc)
					}
				}
			}

			// add value for count(uid) nodes if any.
			if _, err := pc.handleCountUIDNodes(enc, dst, len(ul.Uids)); err != nil {
				return err
			}
		default:
			if pc.Params.Alias == "" && len(pc.Params.Langs) > 0 && pc.Params.Langs[0] != "*" {
				fieldName += "@"
				fieldName += strings.Join(pc.Params.Langs, ":")
			}

			// calculate it once to avoid multiple call to idToAttr()
			fieldID := enc.idForAttr(fieldName)
			// Add len of fieldName to enc.curSize.
			enc.curSize += uint64(len(fieldName))

			if pc.Attr == "uid" {
				if err := enc.SetUID(dst, uid, fieldID); err != nil {
					return err
				}
				continue
			}

			if len(pc.facetsMatrix) > idx && len(pc.facetsMatrix[idx].FacetsList) > 0 {
				// In case of Value we have only one Facets.
				for i, fcts := range pc.facetsMatrix[idx].FacetsList {
					if err := enc.attachFacets(dst, fieldName, pc.List, fcts.Facets, i); err != nil {
						return err
					}
				}
			}

			if len(pc.valueMatrix) <= idx {
				continue
			}

			for i, tv := range pc.valueMatrix[idx].Values {
				// if conversion not possible, we ignore it in the result.
				sv, convErr := convertWithBestEffort(tv, pc.Attr)
				if convErr != nil {
					return convErr
				}

				if pc.Params.ExpandAll && len(pc.LangTags[idx].Lang) != 0 {
					if i >= len(pc.LangTags[idx].Lang) {
						return errors.Errorf(
							"pb.error: all lang tags should be either present or absent")
					}
					fieldNameWithTag := fieldName
					lang := pc.LangTags[idx].Lang[i]
					if lang != "" && lang != "*" {
						fieldNameWithTag += "@" + lang
					}
					encodeAsList := pc.List && lang == ""
					if err := enc.AddListValue(dst, enc.idForAttr(fieldNameWithTag),
						sv, encodeAsList); err != nil {
						return err
					}
					continue
				}

				encodeAsList := pc.List && len(pc.Params.Langs) == 0
				if !pc.Params.Normalize {
					err := enc.AddListValue(dst, fieldID, sv, encodeAsList)
					if err != nil {
						return err
					}
					continue
				}
				// If the query had the normalize directive, then we only add nodes
				// with an Alias.
				if pc.Params.Alias != "" {
					err := enc.AddListValue(dst, fieldID, sv, encodeAsList)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	if sg.Params.IgnoreReflex && len(sg.Params.ParentIds) > 0 {
		// Lets pop the stack.
		sg.Params.ParentIds = (sg.Params.ParentIds)[:len(sg.Params.ParentIds)-1]
	}

	// Only for shortest path query we want to return uid always if there is
	// nothing else at that level.
	if (sg.Params.GetUid && !enc.IsEmpty(dst)) || sg.Params.Shortest {
		if err := enc.SetUID(dst, uid, enc.uidAttr); err != nil {
			return err
		}
	}

	if sg.pathMeta != nil {
		totalWeight := types.Val{
			Tid:   types.FloatID,
			Value: sg.pathMeta.weight,
		}
		if err := enc.AddValue(dst, enc.idForAttr("_weight_"), totalWeight); err != nil {
			return err
		}
	}

	return nil
}
