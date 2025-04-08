/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package chunker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/geojson"

	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/dgraph-io/simdjson-go"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/types"
	"github.com/hypermodeinc/dgraph/v24/types/facets"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var errEmptyUID = errors.New("UID must be present and non-zero while deleting edges")

func stripSpaces(str string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}

		return r
	}, str)
}

// handleBasicFacetsType parses a facetVal to string/float64/bool/datetime type.
func handleBasicFacetsType(key string, facetVal interface{}) (*api.Facet, error) {
	var jsonValue interface{}
	var valueType api.Facet_ValType
	switch v := facetVal.(type) {
	case string:
		if t, err := types.ParseTime(v); err == nil {
			valueType = api.Facet_DATETIME
			jsonValue = t
		} else {
			facet, err := facets.FacetFor(key, strconv.Quote(v))
			if err != nil {
				return nil, err
			}

			// FacetFor function already converts the value to binary so there is no need
			// for the conversion again after the switch block.
			return facet, nil
		}
	case json.Number:
		number := facetVal.(json.Number)
		if strings.Contains(number.String(), ".") {
			jsonFloat, err := number.Float64()
			if err != nil {
				return nil, err
			}
			jsonValue = jsonFloat
			valueType = api.Facet_FLOAT
		} else {
			jsonInt, err := number.Int64()
			if err != nil {
				return nil, err
			}
			jsonValue = jsonInt
			valueType = api.Facet_INT
		}
	// these int64/float64 cases are needed for the FastParseJSON simdjson
	// parser, which doesn't use json.Number
	case int64:
		jsonValue = v
		valueType = api.Facet_INT
	case float64:
		jsonValue = v
		valueType = api.Facet_FLOAT
	case bool:
		jsonValue = v
		valueType = api.Facet_BOOL
	default:
		return nil, errors.New("facet value can only be string/number/bool")
	}

	// Convert facet val interface{} to binary.
	binaryValueFacet, err := facets.ToBinary(key, jsonValue, valueType)
	if err != nil {
		return nil, err
	}

	return binaryValueFacet, nil
}

// parseMapFacets parses facets which are of map type. Facets for scalar list predicates are
// specified in map format. For example below predicate nickname and kind facet associated with it.
// Here nickname "bob" doesn't have any facet associated with it.
//
//	{
//		"nickname": ["alice", "bob", "josh"],
//		"nickname|kind": {
//			"0": "friends",
//			"2": "official"
//		}
//	}
//
// Parsed response would a slice of maps[int]*api.Facet, one map for each facet.
// Map key would be the index of scalar value for respective facets.
func parseMapFacets(m map[string]interface{}, prefix string) ([]map[int]*api.Facet, error) {
	// This happens at root.
	if prefix == "" {
		return nil, nil
	}

	var mapSlice []map[int]*api.Facet
	for fname, facetVal := range m {
		if facetVal == nil {
			continue
		}
		if !strings.HasPrefix(fname, prefix) {
			continue
		}

		fm, ok := facetVal.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("facets format should be of type map for "+
				"scalarlist predicates, found: %v for facet: %v", facetVal, fname)
		}

		idxMap := make(map[int]*api.Facet, len(fm))
		for sidx, val := range fm {
			key := fname[len(prefix):]
			facet, err := handleBasicFacetsType(key, val)
			if err != nil {
				return nil, fmt.Errorf("facet: %s, index: %s: %w", fname, sidx, err)
			}
			idx, err := strconv.Atoi(sidx)
			if err != nil {
				return nil, fmt.Errorf("facet: %s, index: %s: %w", fname, sidx, err)
			}
			idxMap[idx] = facet
		}
		mapSlice = append(mapSlice, idxMap)
	}

	return mapSlice, nil
}

// parseScalarFacets parses facets which should be of type string/json.Number/bool.
// It returns []*api.Facet, one *api.Facet for each facet.
func parseScalarFacets(m map[string]interface{}, prefix string) ([]*api.Facet, error) {
	// This happens at root.
	if prefix == "" {
		return nil, nil
	}

	var facetsForPred []*api.Facet
	for fname, facetVal := range m {
		if facetVal == nil {
			continue
		}
		if !strings.HasPrefix(fname, prefix) {
			continue
		}

		key := fname[len(prefix):]
		facet, err := handleBasicFacetsType(key, facetVal)
		if err != nil {
			return nil, fmt.Errorf("facet: %s: %w", fname, err)
		}
		facetsForPred = append(facetsForPred, facet)
	}

	return facetsForPred, nil
}

// This is the response for a map[string]interface{} i.e. a struct.
type mapResponse struct {
	uid       string       // uid retrieved or allocated for the node.
	namespace uint64       // namespace to which the node belongs.
	fcts      []*api.Facet // facets on the edge connecting this node to the source if any.
	rawFacets map[string]interface{}
}

func handleBasicType(k string, v interface{}, op int, nq *api.NQuad) error {
	switch v := v.(type) {
	case json.Number:
		if strings.ContainsAny(v.String(), ".Ee") {
			f, err := v.Float64()
			if err != nil {
				return err
			}
			nq.ObjectValue = &api.Value{Val: &api.Value_DoubleVal{DoubleVal: f}}
			return nil
		}
		i, err := v.Int64()
		if err != nil {
			return err
		}
		nq.ObjectValue = &api.Value{Val: &api.Value_IntVal{IntVal: i}}

	// this int64 case is needed for FastParseJSON, which doesn't use json.Number
	case int64:
		if v == 0 && op == DeleteNquads {
			nq.ObjectValue = &api.Value{Val: &api.Value_IntVal{IntVal: v}}
			return nil
		}
		nq.ObjectValue = &api.Value{Val: &api.Value_IntVal{IntVal: v}}

	case string:
		// Default value is considered as S P * deletion.
		if v == "" && op == DeleteNquads {
			nq.ObjectValue = &api.Value{Val: &api.Value_DefaultVal{DefaultVal: x.Star}}
			return nil
		}

		if vf, err := types.ParseVFloat(v); err == nil && len(vf) != 0 {
			nq.ObjectValue = &api.Value{Val: &api.Value_Vfloat32Val{Vfloat32Val: types.FloatArrayAsBytes(vf)}}
			return nil
		}

		// Handle the uid function in upsert block
		s := stripSpaces(v)
		if strings.HasPrefix(s, "uid(") || strings.HasPrefix(s, "val(") {
			if !strings.HasSuffix(s, ")") {
				return fmt.Errorf("while processing '%s', brackets are not closed properly", s)
			}
			nq.ObjectId = s
			return nil
		}

		// In RDF, we assume everything is default (types.DefaultID), but in JSON we assume string
		// (StringID). But this value will be checked against the schema so we don't overshadow a
		// password value (types.PasswordID) - Issue#2623
		nq.ObjectValue = &api.Value{Val: &api.Value_StrVal{StrVal: v}}

	case float64:
		if v == 0 && op == DeleteNquads {
			nq.ObjectValue = &api.Value{Val: &api.Value_DefaultVal{DefaultVal: x.Star}}
			return nil
		}
		nq.ObjectValue = &api.Value{Val: &api.Value_DoubleVal{DoubleVal: v}}

	case bool:
		if !v && op == DeleteNquads {
			nq.ObjectValue = &api.Value{Val: &api.Value_DefaultVal{DefaultVal: x.Star}}
			return nil
		}
		nq.ObjectValue = &api.Value{Val: &api.Value_BoolVal{BoolVal: v}}

	default:
		return fmt.Errorf("unexpected type for val for attr: %s while converting to nquad", k)
	}
	return nil

}

func (buf *NQuadBuffer) checkForDeletion(mr mapResponse, m map[string]interface{}, op int) {
	// Since uid is the only key, this must be S * * deletion.
	if op == DeleteNquads && len(mr.uid) > 0 && len(m) == 1 && len(mr.rawFacets) == 0 {
		buf.Push(&api.NQuad{
			Subject:     mr.uid,
			Predicate:   x.Star,
			Namespace:   mr.namespace,
			ObjectValue: &api.Value{Val: &api.Value_DefaultVal{DefaultVal: x.Star}},
		})
	}
}

func handleGeoType(val map[string]interface{}, nq *api.NQuad) (bool, error) {
	_, hasType := val["type"]
	_, hasCoordinates := val["coordinates"]
	if len(val) == 2 && hasType && hasCoordinates {
		b, err := json.Marshal(val)
		if err != nil {
			return false, fmt.Errorf("error while trying to parse value: %+v as geo val", val)
		}
		ok, err := tryParseAsGeo(b, nq)
		if err != nil && ok {
			return true, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func tryParseAsGeo(b []byte, nq *api.NQuad) (bool, error) {
	var g geom.T
	err := geojson.Unmarshal(b, &g)
	if err != nil {
		return false, nil
	}

	geo, err := types.ObjectValue(types.GeoID, g)
	if err != nil {
		return false, fmt.Errorf("couldn't convert value: %s to geo type", string(b))
	}

	nq.ObjectValue = geo
	return true, nil
}

// NQuadBuffer batches up batchSize NQuads per push to channel, accessible via Ch(). If batchSize is
// negative, it only does one push to Ch() during Flush.
type NQuadBuffer struct {
	batchSize int
	nquads    []*api.NQuad
	nqCh      chan []*api.NQuad
	predHints map[string]pb.Metadata_HintType
}

// NewNQuadBuffer returns a new NQuadBuffer instance with the specified batch size.
func NewNQuadBuffer(batchSize int) *NQuadBuffer {
	buf := &NQuadBuffer{
		batchSize: batchSize,
		nqCh:      make(chan []*api.NQuad, 10),
	}
	if buf.batchSize > 0 {
		buf.nquads = make([]*api.NQuad, 0, batchSize)
	}
	buf.predHints = make(map[string]pb.Metadata_HintType)
	return buf
}

// Ch returns a channel containing slices of NQuads which can be consumed by the caller.
func (buf *NQuadBuffer) Ch() <-chan []*api.NQuad {
	return buf.nqCh
}

// Push can be passed one or more NQuad pointers, which get pushed to the buffer.
func (buf *NQuadBuffer) Push(nqs ...*api.NQuad) {
	for _, nq := range nqs {
		buf.nquads = append(buf.nquads, nq)
		if buf.batchSize > 0 && len(buf.nquads) >= buf.batchSize {
			buf.nqCh <- buf.nquads
			buf.nquads = make([]*api.NQuad, 0, buf.batchSize)
		}
	}
}

// Metadata returns the parse metadata that has been aggregated so far..
func (buf *NQuadBuffer) Metadata() *pb.Metadata {
	return &pb.Metadata{
		PredHints: buf.predHints,
	}
}

// PushPredHint pushes and aggregates hints about the type of the predicate derived
// during the parsing. This  metadata is expected to be a lot smaller than the set of
// NQuads so it's not  necessary to send them in batches.
func (buf *NQuadBuffer) PushPredHint(pred string, hint pb.Metadata_HintType) {
	if oldHint, ok := buf.predHints[pred]; ok && hint != oldHint {
		hint = pb.Metadata_LIST
	}
	buf.predHints[pred] = hint
}

// Flush must be called at the end to push out all the buffered NQuads to the channel. Once Flush is
// called, this instance of NQuadBuffer should no longer be used.
func (buf *NQuadBuffer) Flush() {
	if len(buf.nquads) > 0 {
		buf.nqCh <- buf.nquads
		buf.nquads = nil
	}
	close(buf.nqCh)
}

// nextIdx is the index that is used to generate blank node ids for a json map object
// when the map object does not have a "uid" field.
// It should only be accessed through the atomic APIs.
var nextIdx uint64

// randomID will be used to generate blank node ids.
// We use a random number to avoid collision with user specified uids.
var randomID uint32

func init() {
	//nolint:gosec // random node id generator does not require cryptographic precision
	randomID = rand.Uint32()
}

func getNextBlank() string {
	id := atomic.AddUint64(&nextIdx, 1)
	return fmt.Sprintf("_:dg.%d.%d", randomID, id)
}

// TODO - Abstract these parameters to a struct.
func (buf *NQuadBuffer) mapToNquads(m map[string]interface{}, op int, parentPred string) (mapResponse, error) {
	var mr mapResponse

	// move all facets from global map to smaller mr.rawFacets map
	mr.rawFacets = make(map[string]interface{})
	for k, v := range m {
		if strings.Contains(k, x.FacetDelimiter) {
			mr.rawFacets[k] = v
			delete(m, k)
		}
	}

	// Check field in map.
	if uidVal, ok := m["uid"]; ok {
		var uid uint64

		switch uidVal := uidVal.(type) {
		case json.Number:
			ui, err := uidVal.Int64()
			if err != nil {
				return mr, err
			}
			uid = uint64(ui)

		// this int64 case is needed for FastParseJSON, which doesn't use json.Number
		case int64:
			uid = uint64(uidVal)

		case string:
			s := stripSpaces(uidVal)
			if uidVal == "" {
				uid = 0
			} else if ok := strings.HasPrefix(uidVal, "_:"); ok {
				mr.uid = uidVal
			} else if ok := strings.HasPrefix(s, "uid("); ok {
				mr.uid = s
			} else if u, err := strconv.ParseUint(uidVal, 0, 64); err == nil {
				uid = u
			} else {
				return mr, err
			}
		}
		if uid > 0 {
			mr.uid = fmt.Sprintf("%d", uid)
		}
	}

	if mr.uid == "" {
		if op == DeleteNquads {
			// Delete operations with a non-nil value must have an uid specified.
			return mr, errEmptyUID
		}
		mr.uid = getNextBlank()
	}

	namespace := x.GalaxyNamespace
	if ns, ok := m["namespace"]; ok {
		switch nsVal := ns.(type) {
		case json.Number:
			nsi, err := nsVal.Int64()
			if err != nil {
				return mr, err
			}
			namespace = uint64(nsi)

		// this int64 case is needed for FastParseJSON, which doesn't use json.Number
		case int64:
			namespace = uint64(nsVal)
		case string:
			s := stripSpaces(nsVal)
			if s == "" {
				namespace = 0
			} else if n, err := strconv.ParseUint(s, 0, 64); err == nil {
				namespace = n
			} else {
				return mr, err
			}
		}
	}
	mr.namespace = namespace

	for pred, v := range m {
		// We have already extracted the uid above so we skip that edge.
		// v can be nil if user didn't set a value and if omitEmpty was not supplied as JSON
		// option.
		// We also skip facets here because we parse them with the corresponding predicate.
		if pred == "uid" || pred == "namespace" {
			continue
		}

		if v == nil {
			if op == DeleteNquads {
				// This corresponds to edge deletion.
				nq := &api.NQuad{
					Subject:     mr.uid,
					Predicate:   pred,
					Namespace:   namespace,
					ObjectValue: &api.Value{Val: &api.Value_DefaultVal{DefaultVal: x.Star}},
				}
				// Here we split predicate and lang directive (ex: "name@en"), if needed. With JSON
				// mutations that's the only way to send language for a value.
				nq.Predicate, nq.Lang = x.PredicateLang(nq.Predicate)
				buf.Push(nq)
				continue
			}

			// If op is SetNquads, ignore this triplet and continue.
			continue
		}

		nq := api.NQuad{
			Subject:   mr.uid,
			Predicate: pred,
			Namespace: namespace,
		}

		prefix := pred + x.FacetDelimiter
		if _, ok := v.([]interface{}); !ok {
			fts, err := parseScalarFacets(mr.rawFacets, prefix)
			if err != nil {
				return mr, err
			}
			nq.Facets = fts
		}

		// Here we split predicate and lang directive (ex: "name@en"), if needed. With JSON
		// mutations that's the only way to send language for a value.
		nq.Predicate, nq.Lang = x.PredicateLang(nq.Predicate)

		switch v := v.(type) {
		// these int64/float64 cases are needed for FastParseJSON, which doesn't use json.Number
		case int64, float64:
			if err := handleBasicType(pred, v, op, &nq); err != nil {
				return mr, err
			}
			buf.Push(&nq)
			buf.PushPredHint(pred, pb.Metadata_SINGLE)
		case string, json.Number, bool:
			if err := handleBasicType(pred, v, op, &nq); err != nil {
				return mr, err
			}
			buf.Push(&nq)
			buf.PushPredHint(pred, pb.Metadata_SINGLE)
		case map[string]interface{}:
			if len(v) == 0 {
				continue
			}

			ok, err := handleGeoType(v, &nq)
			if err != nil {
				return mr, err
			}
			if ok {
				buf.Push(&nq)
				buf.PushPredHint(pred, pb.Metadata_SINGLE)
				continue
			}

			cr, err := buf.mapToNquads(v, op, pred)
			if err != nil {
				return mr, err
			}

			// Add the connecting edge between the entities.
			nq.ObjectId = cr.uid
			nq.Facets = cr.fcts
			buf.Push(&nq)
			buf.PushPredHint(pred, pb.Metadata_SINGLE)
		case []interface{}:
			buf.PushPredHint(pred, pb.Metadata_LIST)

			// NOTE: facetsMapSlice should be empty unless this is a scalar list
			var facetsMapSlice []map[int]*api.Facet
			for idx, item := range v {
				if idx == 0 {
					// determine if this is a scalar list
					switch item.(type) {
					case string, float64, json.Number, int64:
						var err error
						facetsMapSlice, err = parseMapFacets(mr.rawFacets, prefix)
						if err != nil {
							return mr, err
						}
					default:
						// not a scalar list, continue
					}
				}

				nq := api.NQuad{
					Subject:   mr.uid,
					Predicate: pred,
					Namespace: namespace,
				}

				switch iv := item.(type) {
				case string, float64, json.Number, int64:
					if err := handleBasicType(pred, iv, op, &nq); err != nil {
						return mr, err
					}
					// Here populate facets from facetsMapSlice. Each map has mapping for single
					// facet from item(one of predicate value) idx to *api.Facet.
					// {
					// 	"friend": ["Joshua", "David", "Josh"],
					// 	"friend|from": {
					// 		"0": "school"
					// 	},
					// 	"friend|age": {
					// 		"1": 20
					// 	}
					// }
					// facetMapSlice looks like below. First map is for friend|from facet and second
					// map is for friend|age facet.
					// [
					// 		map[int]*api.Facet{
					//			0: *api.Facet
					// 		},
					// 		map[int]*api.Facet{
					//			1: *api.Facet
					// 		}
					// ]
					var fts []*api.Facet
					for _, fm := range facetsMapSlice {
						if ft, ok := fm[idx]; ok {
							fts = append(fts, ft)
						}
					}
					nq.Facets = fts
					buf.Push(&nq)
				case map[string]interface{}:
					// map[string]interface{} can mean geojson or a connecting entity.
					ok, err := handleGeoType(item.(map[string]interface{}), &nq)
					if err != nil {
						return mr, err
					}
					if ok {
						buf.Push(&nq)
						continue
					}

					cr, err := buf.mapToNquads(iv, op, pred)
					if err != nil {
						return mr, err
					}
					nq.ObjectId = cr.uid
					nq.Facets = cr.fcts
					buf.Push(&nq)
				default:
					return mr,
						fmt.Errorf("got unsupported type for list: %s", pred)
				}
			}
		default:
			return mr, fmt.Errorf("unexpected type for val for attr: %s while converting to nquad", pred)
		}
	}

	fts, err := parseScalarFacets(mr.rawFacets, parentPred+x.FacetDelimiter)
	mr.fcts = fts

	return mr, err
}

const (
	// SetNquads is the constant used to indicate that the parsed NQuads are meant to be added.
	SetNquads = iota
	// DeleteNquads is the constant used to indicate that the parsed NQuads are meant to be
	// deleted.
	DeleteNquads
)

// FastParseJSON currently parses NQuads about 30% faster than ParseJSON.
//
// This function is very similar to buf.ParseJSON, but we just replace encoding/json with
// simdjson-go.
func (buf *NQuadBuffer) FastParseJSON(b []byte, op int) error {
	if !simdjson.SupportedCPU() {
		// default to slower / old parser
		return buf.ParseJSON(b, op)
	}
	// parse the json into tape format
	tape, err := simdjson.Parse(b, nil)
	if err != nil {
		return err
	}

	// we only need the iter to get the first element, either an array or object
	iter := tape.Iter()

	tmp := &simdjson.Iter{}

	// if root object, this will be filled
	obj := &simdjson.Object{}
	// if root array, this will be filled
	arr := &simdjson.Array{}

	// grab the first element
	typ := iter.Advance()
	switch typ {
	case simdjson.TypeRoot:
		if typ, tmp, err = iter.Root(tmp); err != nil {
			return err
		}
		if typ == simdjson.TypeObject {
			// the root element is an object, so parse the object
			if obj, err = tmp.Object(obj); err != nil {
				return err
			}
			// attempt to convert to map[string]interface{}
			m, err := obj.Map(nil)
			if err != nil {
				return err
			}
			// pass to next parsing stage
			mr, err := buf.mapToNquads(m, op, "")
			if err != nil {
				return err
			}
			buf.checkForDeletion(mr, m, op)
		} else if typ == simdjson.TypeArray {
			// the root element is an array, so parse the array
			if arr, err = tmp.Array(arr); err != nil {
				return err
			}
			// attempt to convert to []interface{}
			a, err := arr.Interface()
			if err != nil {
				return err
			}
			if len(a) > 0 {
				// attempt to convert each array element to a
				// map[string]interface{} for further parsing
				var o interface{}
				for _, o = range a {
					if _, ok := o.(map[string]interface{}); !ok {
						return errors.New("only array of map allowed at root")
					}
					// pass to next parsing stage
					mr, err := buf.mapToNquads(o.(map[string]interface{}), op, "")
					if err != nil {
						return err
					}
					buf.checkForDeletion(mr, o.(map[string]interface{}), op)
				}
			}
		}
	default:
		return errors.New("initial element not found in json")
	}
	return nil
}

// ParseJSON parses the given byte slice and pushes the parsed NQuads into the buffer.
func (buf *NQuadBuffer) ParseJSON(b []byte, op int) error {
	buffer := bytes.NewBuffer(b)
	dec := json.NewDecoder(buffer)
	dec.UseNumber()
	ms := make(map[string]interface{})
	var list []interface{}
	if err := dec.Decode(&ms); err != nil {
		// Couldn't parse as map, lets try to parse it as a list.
		buffer.Reset() // The previous contents are used. Reset here.
		// Rewrite b into buffer, so it can be consumed.
		if _, err := buffer.Write(b); err != nil {
			return err
		}
		if err = dec.Decode(&list); err != nil {
			return err
		}
	}
	if len(list) == 0 && len(ms) == 0 {
		return nil
	}
	if len(list) > 0 {
		for _, obj := range list {
			if _, ok := obj.(map[string]interface{}); !ok {
				return errors.New("only array of map allowed at root")
			}
			mr, err := buf.mapToNquads(obj.(map[string]interface{}), op, "")
			if err != nil {
				return err
			}
			buf.checkForDeletion(mr, obj.(map[string]interface{}), op)
		}
		return nil
	}
	mr, err := buf.mapToNquads(ms, op, "")
	if err != nil {
		return err
	}
	buf.checkForDeletion(mr, ms, op)
	return nil
}

// ParseJSON is a convenience wrapper function to get all NQuads in one call. This can however, lead
// to high memory usage. So be careful using this.
func ParseJSON(b []byte, op int) ([]*api.NQuad, *pb.Metadata, error) {
	buf := NewNQuadBuffer(-1)
	err := buf.FastParseJSON(b, op)
	if err != nil {
		return nil, nil, err
	}
	buf.Flush()
	nqs := <-buf.Ch()
	metadata := buf.Metadata()
	return nqs, metadata, nil
}
