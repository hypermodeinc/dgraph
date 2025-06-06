/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/pkg/errors"

	"github.com/hypermodeinc/dgraph/v25/algo"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
)

// rdfBuilder is used to generate RDF from subgraph.
type rdfBuilder struct {
	buf *bytes.Buffer
}

// ToRDF converts the given subgraph list into rdf format.
func ToRDF(l *Latency, sgl []*SubGraph) ([]byte, error) {
	b := &rdfBuilder{
		buf: &bytes.Buffer{},
	}
	for _, sg := range sgl {
		if err := validateSubGraphForRDF(sg); err != nil {
			return nil, err
		}
		// Skip parent graph. we don't want parent values.
		for _, child := range sg.Children {
			if err := b.castToRDF(child); err != nil {
				return nil, err
			}
		}
	}
	return b.buf.Bytes(), nil
}

// castToRDF converts the given subgraph to RDF and appends to the
// output string.
func (b *rdfBuilder) castToRDF(sg *SubGraph) error {
	if err := validateSubGraphForRDF(sg); err != nil {
		return err
	}
	if sg.SrcUIDs != nil {
		// Get RDF for the given subgraph.
		if err := b.rdfForSubgraph(sg); err != nil {
			return err
		}
	}
	// Recursively cnvert RDF for the children graph.
	for _, child := range sg.Children {
		if err := b.castToRDF(child); err != nil {
			return err
		}
	}
	return nil
}

// rdfForSubgraph generates RDF and appends to the output parameter.
func (b *rdfBuilder) rdfForSubgraph(sg *SubGraph) error {
	// handle the case of recurse queries
	// Do not generate RDF if all the children of sg null uidMatrix
	nonNullChild := false
	for _, ch := range sg.Children {
		if len(ch.uidMatrix) != 0 {
			nonNullChild = true
		}
	}

	if len(sg.Children) > 0 && !nonNullChild {
		return nil
	}

	for i, uid := range sg.SrcUIDs.Uids {
		if sg.Params.IgnoreResult {
			// Skip ignored values.
			continue
		}
		if sg.IsInternal() {
			if sg.Params.Expand != "" {
				continue
			}
			// Check if we have val for the given uid. If you got uid then populate
			// the rdf.
			val, ok := sg.Params.UidToVal.Get(uid)
			if !ok && val.Value == nil {
				continue
			}
			outputval, err := getObjectVal(val)
			if err != nil {
				continue
			}
			b.writeRDF(uid, []byte(sg.aggWithVarFieldName()), outputval)
			continue
		}
		switch {
		case len(sg.counts) > 0:
			// Add count rdf.
			b.rdfForCount(uid, sg.counts[i], sg)
		case i < len(sg.uidMatrix) && len(sg.uidMatrix[i].Uids) != 0 && len(sg.Children) > 0:
			// Add posting list relation.
			b.rdfForUIDList(uid, sg.uidMatrix[i], sg)
		case i < len(sg.valueMatrix):
			b.rdfForValueList(uid, sg.valueMatrix[i], sg.fieldName())
		}
	}
	return nil
}

func (b *rdfBuilder) writeRDF(subject uint64, predicate []byte, object []byte) {
	// add subject
	x.Check2(b.buf.Write(x.ToHex(subject, true)))
	x.Check(b.buf.WriteByte(' '))
	// add predicate
	b.writeTriple(predicate)
	x.Check(b.buf.WriteByte(' '))
	// add object
	x.Check2(b.buf.Write(object))
	x.Check(b.buf.WriteByte(' '))
	x.Check(b.buf.WriteByte('.'))
	x.Check(b.buf.WriteByte('\n'))
}

func (b *rdfBuilder) writeTriple(val []byte) {
	x.Check(b.buf.WriteByte('<'))
	x.Check2(b.buf.Write(val))
	x.Check(b.buf.WriteByte('>'))
}

// rdfForCount returns rdf for count fucntion.
func (b *rdfBuilder) rdfForCount(subject uint64, count uint32, sg *SubGraph) {
	fieldName := sg.Params.Alias
	if fieldName == "" {
		fieldName = fmt.Sprintf("count(%s)", sg.Attr)
	}
	b.writeRDF(subject, []byte(fieldName),
		quotedNumber([]byte(strconv.FormatUint(uint64(count), 10))))
}

// rdfForUIDList returns rdf for uid list.
func (b *rdfBuilder) rdfForUIDList(subject uint64, list *pb.List, sg *SubGraph) {
	for _, destUID := range list.Uids {
		if algo.IndexOf(sg.DestUIDs, destUID) < 0 {
			// This uid is filtered.
			continue
		}
		// Build object.
		b.writeRDF(subject, []byte(sg.fieldName()), x.ToHex(destUID, true))
	}
}

// rdfForValueList returns rdf for the value list.
// Ignore RDF's for the attirbute `uid`.
func (b *rdfBuilder) rdfForValueList(subject uint64, valueList *pb.ValueList, attr string) {
	for _, destValue := range valueList.Values {
		val, err := convertWithBestEffort(destValue, attr)
		if err != nil {
			continue
		}
		outputval, err := getObjectVal(val)
		if err != nil {
			continue
		}
		b.writeRDF(subject, []byte(attr), outputval)
	}
}

func getObjectVal(v types.Val) ([]byte, error) {
	outputval, err := valToBytes(v)
	if err != nil {
		return nil, err
	}
	switch v.Tid {
	case types.UidID:
		return buildTriple(outputval), nil
	case types.IntID:
		return quotedNumber(outputval), nil
	case types.FloatID:
		return quotedNumber(outputval), nil
	case types.GeoID:
		return nil, errors.New("Geo id is not supported in rdf output")
	default:
		return outputval, nil
	}
}

func buildTriple(val []byte) []byte {
	buf := make([]byte, 0, 2+len(val))
	buf = append(buf, '<')
	buf = append(buf, val...)
	buf = append(buf, '>')
	return buf
}

func validateSubGraphForRDF(sg *SubGraph) error {
	if sg.IsGroupBy() {
		return errors.New("groupby is not supported in rdf output format")
	}
	uidCount := sg.Attr == "uid" && sg.Params.DoCount && sg.IsInternal()
	if uidCount {
		return errors.New("uid count is not supported in the rdf output format")
	}
	if sg.Params.Normalize {
		return errors.New("normalize directive is not supported in the rdf output format")
	}
	if sg.Params.IgnoreReflex {
		return errors.New("ignorereflex directive is not supported in the rdf output format")
	}
	if sg.SrcFunc != nil && sg.SrcFunc.Name == "checkpwd" {
		return errors.New("chkpwd function is not supported in the rdf output format")
	}
	if sg.Params.Facet != nil && !sg.Params.ExpandAll {
		return errors.New("facets are not supported in the rdf output format")
	}
	return nil
}

func quotedNumber(val []byte) []byte {
	tmpVal := make([]byte, 0, len(val)+2)
	tmpVal = append(tmpVal, '"')
	tmpVal = append(tmpVal, val...)
	tmpVal = append(tmpVal, '"')
	return tmpVal
}
