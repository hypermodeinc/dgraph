/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package chunker

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/lex"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/types"
	"github.com/hypermodeinc/dgraph/v24/types/facets"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var (
	// ErrEmpty indicates that the parser encountered a harmless error (e.g empty line or comment).
	ErrEmpty = errors.New("RDF: harmless error, e.g. comment line")
)

// Function to do sanity check for subject, predicate and object strings.
func sane(s string) bool {
	// ObjectId can be "", we already check that subject and predicate
	// shouldn't be empty.
	if len(s) == 0 {
		return true
	}

	// s should have atleast one alphanumeric character.
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// ParseRDFs is a convenience wrapper function to get all NQuads in one call. This can however, lead
// to high memory usage. So, be careful using this.
func ParseRDFs(b []byte) ([]*api.NQuad, *pb.Metadata, error) {
	var nqs []*api.NQuad
	var l lex.Lexer
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		nq, err := ParseRDF(string(line), &l)
		if errors.Is(err, ErrEmpty) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		nqs = append(nqs, &nq)
	}

	return nqs, calculateTypeHints(nqs), nil
}

func isSpaceRune(r rune) bool {
	return r == ' '
}

// ParseRDF parses a mutation string and returns the N-Quad representation for it.
// It parses N-Quad statements based on http://www.w3.org/TR/n-quads/.
func ParseRDF(line string, l *lex.Lexer) (api.NQuad, error) {
	var rnq api.NQuad
	line = strings.TrimSpace(line)
	if len(line) == 0 {
		return rnq, ErrEmpty
	}

	l.Reset(line)
	l.Run(lexText)
	if err := l.ValidateResult(); err != nil {
		return rnq, err
	}
	it := l.NewIterator()
	var oval string
	var seenOval bool
	var vend bool
	isCommentLine := false
	// We read items from the l.Items channel to which the lexer sends items.
L:
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case itemSubject:
			rnq.Subject = strings.TrimFunc(item.Val, isSpaceRune)

		case itemSubjectFunc:
			var err error
			if rnq.Subject, err = parseFunction(it); err != nil {
				return rnq, err
			}

		case itemObjectFunc:
			var err error
			if rnq.ObjectId, err = parseFunction(it); err != nil {
				return rnq, err
			}

		case itemPredicate:
			// Here we split predicate and lang directive (ex: "name@en"), if needed.
			rnq.Predicate, rnq.Lang = x.PredicateLang(strings.TrimFunc(item.Val, isSpaceRune))

		case itemObject:
			rnq.ObjectId = strings.TrimFunc(item.Val, isSpaceRune)

		case itemStar:
			switch {
			case rnq.Subject == "":
				rnq.Subject = x.Star
			case rnq.Predicate == "":
				rnq.Predicate = x.Star
			default:
				rnq.ObjectValue = &api.Value{Val: &api.Value_DefaultVal{DefaultVal: x.Star}}
			}

		case itemLiteral:
			var err error
			oval, err = strconv.Unquote(item.Val)
			if err != nil {
				return rnq, fmt.Errorf("while unquoting: %w", err)
			}
			seenOval = true

		case itemLanguage:
			rnq.Lang = item.Val

		case itemObjectType:
			if rnq.Predicate == x.Star || rnq.Subject == x.Star {
				return rnq, errors.New("if predicate/subject is *, value should be * as well")
			}

			val := strings.TrimFunc(item.Val, isSpaceRune)
			// TODO: Check if this condition is required.
			if val == "*" {
				return rnq, errors.New("itemObject can't be *")
			}
			// Lets find out the storage type from the type map.
			t, ok := typeMap[val]
			if !ok {
				return rnq, fmt.Errorf("unrecognized rdf type %s", val)
			}
			if oval == "" && t != types.StringID {
				return rnq, errors.New("invalid ObjectValue")
			}
			src := types.ValueForType(types.StringID)
			src.Value = []byte(oval)
			// if this is a password value dont re-encrypt. issue#2765
			if t == types.PasswordID {
				src.Tid = t
			}
			p, err := types.Convert(src, t)
			if err != nil {
				return rnq, err
			}

			if rnq.ObjectValue, err = types.ObjectValue(t, p.Value); err != nil {
				return rnq, err
			}
		case itemComment:
			isCommentLine = true
			vend = true

		case itemValidEnd:
			vend = true
			if !it.Next() {
				return rnq, fmt.Errorf("invalid end of input. Input: [%s]", line)
			}
			// RDF spec says N-Quads should be terminated with a newline. Since we break the input
			// by newline already. We should get EOF or # after dot(.)
			item = it.Item()
			if !(item.Typ == lex.ItemEOF || item.Typ == itemComment) {
				return rnq, fmt.Errorf("invalid end of input. Expected newline or # after ."+
					" Input: [%s]", line)
			}
			break L

		case itemLabel:
			s := strings.TrimFunc(item.Val, isSpaceRune)
			namespace, err := strconv.ParseUint(s, 0, 64)
			if err != nil {
				return rnq, fmt.Errorf("invalid namespace ID. Input: [%s]", line)
			}
			rnq.Namespace = namespace

		case itemLeftRound:
			it.Prev() // backup '('
			if err := parseFacetsRDF(it, &rnq); err != nil {
				return rnq, fmt.Errorf("could not parse facet: %w", err)
			}
		}
	}

	if !vend {
		return rnq, fmt.Errorf("invalid end of input. Input: [%s]", line)
	}
	if isCommentLine {
		return rnq, ErrEmpty
	}
	// We only want to set default value if we have seen ObjectValue within "" and if we didn't
	// already set it.
	if seenOval && rnq.ObjectValue == nil {
		rnq.ObjectValue = &api.Value{Val: &api.Value_DefaultVal{DefaultVal: oval}}
	}
	if len(rnq.Subject) == 0 || len(rnq.Predicate) == 0 {
		return rnq, fmt.Errorf("empty required fields in NQuad. Input: [%s]", line)
	}
	if len(rnq.ObjectId) == 0 && rnq.ObjectValue == nil {
		return rnq, fmt.Errorf("no Object in NQuad. Input: [%s]", line)
	}
	if !sane(rnq.Subject) || !sane(rnq.Predicate) || !sane(rnq.ObjectId) {
		return rnq, fmt.Errorf("NQuad failed sanity check:%+v", rnq)
	}

	return rnq, nil
}

// parseFunction parses uid(<var name>) and returns
// uid(<var name>) after striping whitespace if any
func parseFunction(it *lex.ItemIterator) (string, error) {
	item := it.Item()
	s := item.Val

	it.Next()
	if item = it.Item(); item.Typ != itemLeftRound {
		return "", fmt.Errorf("expected '(', found: %s", item.Val)
	}

	it.Next()
	if item = it.Item(); item.Typ != itemVarName {
		return "", fmt.Errorf("expected variable name, found: %s", item.Val)
	}
	if strings.TrimSpace(item.Val) == "" {
		return "", errors.New("empty variable name in function call")
	}
	s += "(" + item.Val + ")"

	it.Next()
	if item = it.Item(); item.Typ != itemRightRound {
		return "", fmt.Errorf("expected ')', found: %s", item.Val)
	}

	return s, nil
}

func parseFacetsRDF(it *lex.ItemIterator, rnq *api.NQuad) error {
	if !it.Next() {
		return errors.New("unexpected end of facets")
	}
	item := it.Item()
	if item.Typ != itemLeftRound {
		return fmt.Errorf("expected '(' but found %v at facet", item.Val)
	}

	for it.Next() { // parse one key value pair
		// parse key
		item = it.Item()
		if item.Typ != itemText {
			return fmt.Errorf("expected key but found %v", item.Val)
		}
		facetKey := strings.TrimSpace(item.Val)
		if len(facetKey) == 0 {
			return errors.New("empty facetKeys not allowed")
		}
		// parse =
		if !it.Next() {
			return errors.New("unexpected end of facets")
		}
		item = it.Item()
		if item.Typ != itemEqual {
			return fmt.Errorf("expected = after facetKey. Found %v", item.Val)
		}
		// parse value or empty value
		if !it.Next() {
			return errors.New("unexpected end of facets")
		}
		item = it.Item()
		facetVal := ""
		if item.Typ == itemText {
			facetVal = item.Val
		}
		facet, err := facets.FacetFor(facetKey, facetVal)
		if err != nil {
			return err
		}
		rnq.Facets = append(rnq.Facets, facet)

		// empty value case..
		if item.Typ == itemRightRound {
			break
		}
		if item.Typ == itemComma {
			continue
		}
		if item.Typ != itemText {
			return fmt.Errorf("expected , or ) or text but found %s", item.Val)
		}
		// value was present...
		if !it.Next() { // get either ')' or ','
			return errors.New("unexpected end of facets")
		}
		item = it.Item()
		if item.Typ == itemRightRound {
			break
		}
		if item.Typ == itemComma {
			continue
		}
		return fmt.Errorf("expected , or ) after facet. Received %s", item.Val)
	}

	return nil
}

// subjectPred is a type to store the count for each <subject, pred> in the  mutations.
type subjectPred struct {
	subject string
	pred    string
}

func calculateTypeHints(nqs []*api.NQuad) *pb.Metadata {
	// Stores the count of <subject, pred> pairs to help figure out whether
	// schemas should be created as scalars or lists of scalars.
	schemaCountMap := make(map[subjectPred]int)
	predHints := make(map[string]pb.Metadata_HintType)

	for _, nq := range nqs {
		subPredPair := subjectPred{subject: nq.Subject, pred: nq.Predicate}
		schemaCountMap[subPredPair]++
		if count := schemaCountMap[subPredPair]; count > 1 {
			predHints[nq.Predicate] = pb.Metadata_LIST
		}
	}
	return &pb.Metadata{PredHints: predHints}
}

var typeMap = map[string]types.TypeID{
	"xs:password":        types.PasswordID,
	"xs:string":          types.StringID,
	"xs:date":            types.DateTimeID,
	"xs:dateTime":        types.DateTimeID,
	"xs:int":             types.IntID,
	"xs:integer":         types.IntID,
	"xs:positiveInteger": types.IntID,
	"xs:boolean":         types.BoolID,
	"xs:double":          types.FloatID,
	"xs:float":           types.FloatID,
	"xs:base64Binary":    types.BinaryID,
	"xs:decimal":         types.BigFloatID,
	"geo:geojson":        types.GeoID,
	"xs:[]float32":       types.VFloatID,
	"http://www.w3.org/2001/XMLSchema#string":          types.StringID,
	"http://www.w3.org/2001/XMLSchema#dateTime":        types.DateTimeID,
	"http://www.w3.org/2001/XMLSchema#date":            types.DateTimeID,
	"http://www.w3.org/2001/XMLSchema#int":             types.IntID,
	"http://www.w3.org/2001/XMLSchema#positiveInteger": types.IntID,
	"http://www.w3.org/2001/XMLSchema#integer":         types.IntID,
	"http://www.w3.org/2001/XMLSchema#boolean":         types.BoolID,
	"http://www.w3.org/2001/XMLSchema#double":          types.FloatID,
	"http://www.w3.org/2001/XMLSchema#float":           types.FloatID,
	"http://www.w3.org/2001/XMLSchema#gYear":           types.DateTimeID,
	"http://www.w3.org/2001/XMLSchema#gYearMonth":      types.DateTimeID,
}
