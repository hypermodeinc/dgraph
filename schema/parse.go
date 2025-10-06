/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"math"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"github.com/pkg/errors"

	"github.com/hypermodeinc/dgraph/v25/lex"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/tok"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
)

// ParseBytes parses the byte array which holds the schema. We will reset
// all the globals.
// Overwrites schema blindly - called only during initilization in testing
func ParseBytes(s []byte, gid uint32) (rerr error) {
	if pstate == nil {
		reset()
	}
	pstate.DeleteAll()
	result, err := Parse(string(s))
	if err != nil {
		return err
	}

	for _, update := range result.Preds {
		State().Set(update.Predicate, update)
	}
	return nil
}

func parseDirective(it *lex.ItemIterator, schema *pb.SchemaUpdate, t types.TypeID) error {
	it.Next()
	next := it.Item()
	if next.Typ != itemText {
		return next.Errorf("Missing directive name")
	}
	switch next.Val {
	case "reverse":
		if t != types.UidID {
			return next.Errorf("Cannot reverse for non-UID type")
		}
		schema.Directive = pb.SchemaUpdate_REVERSE
	case "index":
		tokenizer, vectorSpecs, err := parseIndexDirective(it, schema.Predicate, t)
		if err != nil {
			return err
		}
		schema.Directive = pb.SchemaUpdate_INDEX
		schema.Tokenizer = tokenizer
		schema.IndexSpecs = vectorSpecs
	case "count":
		schema.Count = true
	case "upsert":
		schema.Upsert = true
	case "unique":
		schema.Unique = true
	case "noconflict":
		schema.NoConflict = true
	case "lang":
		if t != types.StringID || schema.List {
			return next.Errorf("@lang directive can only be specified for string type."+
				" Got: [%v] for attr: [%v]", t.Name(), schema.Predicate)
		}
		schema.Lang = true
	default:
		return next.Errorf("Invalid index specification")
	}
	it.Next()

	return nil
}

func parseScalarPair(it *lex.ItemIterator, predicate string, ns uint64) (*pb.SchemaUpdate, error) {
	it.Next()
	next := it.Item()
	switch {
	// This check might seem redundant but it's necessary. We have two possibilities,
	//   1) that the schema is of form: name@en: string .
	//
	//   2) or this alternate form: <name@en>: string .
	//
	// The itemAt test invalidates 1) and string.Contains() tests for 2). We don't allow
	// '@' in predicate names, so both forms are disallowed. Handling them here avoids
	// messing with the lexer and IRI values.
	case next.Typ == itemAt || strings.Contains(predicate, "@"):
		return nil, next.Errorf("Invalid '@' in name")
	case next.Typ != itemColon:
		return nil, next.Errorf("Missing colon")
	case !it.Next():
		return nil, next.Errorf("Invalid ending while trying to parse schema.")
	}
	next = it.Item()
	schema := &pb.SchemaUpdate{Predicate: x.NamespaceAttr(ns, predicate)}
	// Could be list type.
	if next.Typ == itemLeftSquare {
		schema.List = true
		if !it.Next() {
			return nil, next.Errorf("Invalid ending while trying to parse schema.")
		}
		next = it.Item()
	}

	if next.Typ != itemText {
		return nil, next.Errorf("Missing Type")
	}
	typ := strings.ToLower(next.Val)
	// We ignore the case for types.
	t, ok := types.TypeForName(typ)
	if !ok {
		return nil, next.Errorf("Undefined Type")
	}
	if schema.List {
		if uint32(t) == uint32(types.PasswordID) || uint32(t) == uint32(types.BoolID) {
			return nil, next.Errorf("Unsupported type for list: [%s].", t.Name())
		}
	}
	schema.ValueType = t.Enum()

	// Check for index / reverse.
	it.Next()
	next = it.Item()
	if schema.List {
		if next.Typ != itemRightSquare {
			return nil, next.Errorf("Unclosed [ while parsing schema for: %s", predicate)
		}
		if !it.Next() {
			return nil, next.Errorf("Invalid ending")
		}
		next = it.Item()
	}

	for {
		if next.Typ != itemAt {
			break
		}
		if err := parseDirective(it, schema, t); err != nil {
			return nil, err
		}
		next = it.Item()
	}

	if next.Typ != itemDot {
		return nil, next.Errorf("Invalid ending")
	}
	it.Next()
	next = it.Item()
	if next.Typ == lex.ItemEOF {
		it.Prev()
		return schema, nil
	}
	if next.Typ != itemNewLine {
		return nil, next.Errorf("Invalid ending")
	}
	return schema, nil
}

// parseIndexDirective works on "@index" or "@index(customtokenizer)"
// or @index(tok1(opt1:"opt1val",opt2:"opt2val"), tok2, tok3)
// We assume that the "@index" has already been found, so we just need
// to parse the rest.
// Syntax EBNF (after '@index' has been found):
//
//	Tokens ::= '(' TokenList ')'
//	TokenList ::= Token [',' TokenList]*
//
// This function will specifically handle this as:
//
//	Tokens ::= '(' TokenList ')'
//	TokenList ::= Token [',' TokeniList]
//
// It then defers to parseTokenOrVectorIndexSpec to parse Token.
func parseIndexDirective(it *lex.ItemIterator, predicate string,
	typ types.TypeID) ([]string, []*pb.VectorIndexSpec, error) {
	tokenizers := []string{}
	var vectorSpecs []*pb.VectorIndexSpec
	var seen = make(map[string]bool)
	var seenSortableTok bool

	if typ == types.UidID || typ == types.DefaultID || typ == types.PasswordID {
		return tokenizers, vectorSpecs,
			it.Item().Errorf("Indexing not allowed on predicate %s of type %s",
				predicate, typ.Name())
	}
	if !it.Next() {
		// Nothing to read.
		return tokenizers, vectorSpecs, it.Item().Errorf("Invalid ending.")
	}
	next := it.Item()
	if next.Typ != itemLeftRound {
		it.Prev() // Backup.
		return tokenizers, vectorSpecs,
			it.Item().Errorf("Require type of tokenizer for pred: %s for indexing.",
				predicate)
	}

	// Look for tokenizers and IndexFactories (vectorSpecs).
	for {
		tokenText, vectorSpec, sortable, err := parseTokenOrVectorIndexSpec(it, predicate, typ)
		if err != nil {
			return tokenizers, vectorSpecs, err
		}
		if sortable && seenSortableTok {
			return tokenizers, vectorSpecs,
				next.Errorf("Only one index tokenizer can be sortable for %s",
					predicate)
		}
		seenSortableTok = sortable
		if tokenText != "" {
			if _, found := seen[tokenText]; found {
				return tokenizers, vectorSpecs,
					next.Errorf("Duplicate tokenizers defined for predicate %v",
						predicate)
			}
			tokenizers = append(tokenizers, tokenText)
			seen[tokenText] = true
		} else {
			// parseTokenOrVectorIndexSpec should have returned either
			// non-empty tokenText or non-nil vectorsSpec or an error.
			x.AssertTrue(vectorSpec != nil)
			// At the moment, we cannot accept two VectorIndexSpecs of
			// the same name. Later, we may reconsider this as we
			// develop a simple means to distinguish how their keys
			// are formed based on the specified options. The notion
			// of "seen" still applies, but we just use the tokenizer name.
			seen[vectorSpec.Name] = true
			vectorSpecs = append(vectorSpecs, vectorSpec)
		}

		it.Next()
		next = it.Item()
		if next.Typ == itemRightRound {
			break
		}
		if next.Typ != itemComma {
			return tokenizers, vectorSpecs, next.Errorf(
				"Expected ',' or ')' but found '%s' for predicate '%s'",
				next.Val, predicate)
		}
	}
	return tokenizers, vectorSpecs, nil
}

// parseTokenOrVectorIndexSpec(it, predicate, typ) will parse a "Token" according to the
// grammar specification below.
//
//	Token ::= TokenName [ TokenOptions ]
//	TokenName ::= {itemText from Lexer}
//
// For TokenOptions, it defers to parseTokenOptions parsing.
// We expect either to find the name of a Tokenizer or else the name of an IndexFactory
// along with its options. We also return a boolean value indicating whether or
// not the found index is Sortable.
func parseTokenOrVectorIndexSpec(
	it *lex.ItemIterator,
	predicate string,
	typ types.TypeID) (string, *pb.VectorIndexSpec, bool, error) {
	it.Next()
	next := it.Item()
	if next.Typ != itemText {
		return "", nil, false, next.Errorf(
			"Expected token or VectorFactory name, but found '%s'",
			next.Val)
	}
	tokenOrFactoryName := strings.ToLower(next.Val)
	factory, found := tok.GetIndexFactory(tokenOrFactoryName)
	if found {
		// TODO: Consider allowing IndexFactory types not related to
		//       VectorIndex objects.
		if typ != types.VFloatID {
			return "", nil, false,
				next.Errorf("IndexFactory: %s isn't valid for predicate: %s of type: %s",
					factory.Name(), x.ParseAttr(predicate), typ.Name())
		}
		tokenOpts, err := parseTokenOptions(it, factory)
		if err != nil {
			return "", nil, false, err
		}
		allowedOpts := factory.AllowedOptions()
		for _, pair := range tokenOpts {
			_, err := allowedOpts.GetParsedOption(pair.Key, pair.Value)
			if err != nil {
				return "", nil, false,
					next.Errorf("IndexFactory: %s issues this error: '%s'",
						factory.Name(), err)
			}
		}
		vs := &pb.VectorIndexSpec{
			Name:    tokenOrFactoryName,
			Options: tokenOpts,
		}
		return "", vs, factory.IsSortable(), err
	}

	// Look for custom tokenizer, and validate its type.
	tokenizer, has := tok.GetTokenizer(tokenOrFactoryName)
	if !has {
		return tokenOrFactoryName, nil, false,
			next.Errorf("Invalid tokenizer 1 %s", next.Val)
	}
	tokenizerType, ok := types.TypeForName(tokenizer.Type())
	x.AssertTrue(ok) // Type is validated during tokenizer loading.
	if tokenizerType != typ {
		return tokenOrFactoryName, nil, false,
			next.Errorf("Tokenizer: %s isn't valid for predicate: %s of type: %s",
				tokenizer.Name(), x.ParseAttr(predicate), typ.Name())
	}
	return tokenOrFactoryName, nil, tokenizer.IsSortable(), nil
}

// parseTokenOptions(it, factory) will parse "TokenOptions" according to the
// following grammar:
//
//	TokenOptions ::= ['(' TokenOptionList ')']
//	TokenOptionList ::= TokenOption [',' TokenOptionList ]
//
// TODO: TokenOptionList could be made optional so that "hnsw()" is treated as
// an hnsw index with default search options
//
// For Parsing TokenOption, it defers to parseTokenOption
// Note that specifying TokenOptions is optional! The result is considered
// valid even if no token options are found as long as the first character
// discovered by it is a comma or end-parenthesis. (In the context where we
// invoke this, a comma indicates another tokenizer, and an end-parenthesis
// indicates the end of a list of tokenizers.
// TokenOptions provide the OptionKey-OptionValue pairs needed for building
// a VectorIndex. The factory is used to validate that any option name given
// is specified as an AllowedOption.
func parseTokenOptions(it *lex.ItemIterator, factory tok.IndexFactory) ([]*pb.OptionPair, error) {
	retVal := []*pb.OptionPair{}
	nextItem, found := it.PeekOne()
	if !found {
		return nil, nextItem.Errorf(
			"unexpected end of stream when looking for IndexFactory options")
	}
	if nextItem.Typ == itemComma || nextItem.Typ == itemRightRound {
		return []*pb.OptionPair{}, nil
	}
	if nextItem.Typ != itemLeftRound {
		return nil, nextItem.Errorf(
			"unexpected '%s' found when expecting '('", nextItem.Val)
	}
	it.Next() // Reads initial '('
	for {
		optPair, err := parseTokenOption(it, factory)
		if err != nil {
			return retVal, err
		}
		retVal = append(retVal, optPair)
		it.Next()
		nextItem = it.Item()
		if nextItem.Typ == itemRightRound {
			return retVal, nil
		}
		if nextItem.Typ != itemComma {
			return nil, nextItem.Errorf(
				"unexpected '%s' found when expecting ',' or ')'",
				nextItem.Val)
		}
	}
}

// parseTokenOption(it, factory) constructs OptionPair instances
// and validates that the options are okay via the factory.
//
//	TokenOption ::= OptionName ':' OptionValue
//	OptionName ::= {itemText from Lexer}
//	OptionValue ::= {itemQuotedText from Lexer}
func parseTokenOption(it *lex.ItemIterator, factory tok.IndexFactory) (*pb.OptionPair, error) {
	it.Next()
	nextItem := it.Item()
	if nextItem.Typ != itemText {
		return nil, nextItem.Errorf(
			"unexpected '%s' found when expecting option name",
			nextItem.Val)
	}
	optName := nextItem.Val
	it.Next()
	nextItem = it.Item()
	if nextItem.Typ != itemColon {
		return nil, nextItem.Errorf(
			"unexpected '%s' found when expecting ':'",
			nextItem.Val)
	}
	it.Next()
	nextItem = it.Item()
	if nextItem.Typ != itemQuotedText {
		return nil, nextItem.Errorf(
			"unexpected '%s' found when expecting quoted text",
			nextItem.Val)
	}
	optVal := nextItem.Val[1 : len(nextItem.Val)-1]
	return &pb.OptionPair{Key: optName, Value: optVal}, nil
}

func HasTokenizerOrVectorIndexSpec(update *pb.SchemaUpdate) bool {
	if update == nil {
		return false
	}
	return len(update.Tokenizer) > 0 || len(update.IndexSpecs) > 0
}

// resolveTokenizers resolves default tokenizers and verifies tokenizers definitions.
func resolveTokenizers(updates []*pb.SchemaUpdate) error {
	for _, schema := range updates {
		typ := types.TypeID(schema.ValueType)

		if (typ == types.UidID || typ == types.DefaultID || typ == types.PasswordID) &&
			schema.Directive == pb.SchemaUpdate_INDEX {
			return errors.Errorf("Indexing not allowed on predicate %s of type %s",
				x.ParseAttr(schema.Predicate), typ.Name())
		}

		if typ == types.UidID {
			continue
		}

		if !HasTokenizerOrVectorIndexSpec(schema) &&
			schema.Directive == pb.SchemaUpdate_INDEX {
			return errors.Errorf(
				"Require type of tokenizer for pred: %s of type: %s for indexing.",
				schema.Predicate, typ.Name())
		} else if HasTokenizerOrVectorIndexSpec(schema) &&
			schema.Directive != pb.SchemaUpdate_INDEX {
			return errors.Errorf("Tokenizers present without indexing on attr %s",
				x.ParseAttr(schema.Predicate))
		}
		// check for valid tokenizer types and duplicates
		var seen = make(map[string]bool)
		var seenSortableTok bool
		for _, t := range schema.Tokenizer {
			tokenizer, has := tok.GetTokenizer(t)
			if !has {
				return errors.Errorf("Invalid tokenizer %s", t)
			}
			tokenizerType, ok := types.TypeForName(tokenizer.Type())
			x.AssertTrue(ok) // Type is validated during tokenizer loading.
			if tokenizerType != typ {
				return errors.Errorf("Tokenizer: %s isn't valid for predicate: %s of type: %s",
					tokenizer.Name(), x.ParseAttr(schema.Predicate), typ.Name())
			}
			if _, ok := seen[tokenizer.Name()]; !ok {
				seen[tokenizer.Name()] = true
			} else {
				return errors.Errorf("Duplicate tokenizers present for attr %s",
					x.ParseAttr(schema.Predicate))
			}
			if tokenizer.IsSortable() {
				if seenSortableTok {
					return errors.Errorf("More than one sortable index encountered for: %v",
						schema.Predicate)
				}
				seenSortableTok = true
			}
		}
	}
	return nil
}

func parseTypeDeclaration(it *lex.ItemIterator, ns uint64) (*pb.TypeUpdate, error) {
	// Iterator is currently on the token corresponding to the keyword type.
	if it.Item().Typ != itemText || it.Item().Val != "type" {
		return nil, it.Item().Errorf("Expected type keyword. Got %v", it.Item().Val)
	}

	it.Next()
	if it.Item().Typ != itemText {
		return nil, it.Item().Errorf("Expected type name. Got %v", it.Item().Val)
	}
	typeUpdate := &pb.TypeUpdate{TypeName: x.NamespaceAttr(ns, it.Item().Val)}

	it.Next()
	if it.Item().Typ != itemLeftCurl {
		return nil, it.Item().Errorf("Expected {. Got %v", it.Item().Val)
	}

	var fields []*pb.SchemaUpdate
	for it.Next() {
		item := it.Item()

		switch item.Typ {
		case itemRightCurl:
			it.Next()
			if it.Item().Typ != itemNewLine && it.Item().Typ != lex.ItemEOF {
				return nil, it.Item().Errorf(
					"Expected new line or EOF after type declaration. Got %v", it.Item())
			}
			it.Prev()

			fieldSet := make(map[string]struct{})
			for _, field := range fields {
				if _, ok := fieldSet[field.GetPredicate()]; ok {
					return nil, it.Item().Errorf("Duplicate fields with name: %s",
						x.ParseAttr(field.GetPredicate()))
				}

				fieldSet[field.GetPredicate()] = struct{}{}
			}

			typeUpdate.Fields = fields
			return typeUpdate, nil
		case itemText:
			field, err := parseTypeField(it, typeUpdate.TypeName, ns)
			if err != nil {
				return nil, err
			}
			fields = append(fields, field)
		case itemNewLine:
			// Ignore empty lines.
		default:
			return nil, it.Item().Errorf("Unexpected token. Got %v", it.Item().Val)
		}
	}
	return nil, errors.Errorf("Shouldn't reach here.")
}

func parseTypeField(it *lex.ItemIterator, typeName string, ns uint64) (*pb.SchemaUpdate, error) {
	field := &pb.SchemaUpdate{Predicate: x.NamespaceAttr(ns, it.Item().Val)}
	var list bool
	it.Next()

	// Simplified type definitions only require the field name. If a new line is found,
	// proceed to the next field in the type.
	if it.Item().Typ == itemNewLine {
		return field, nil
	}

	// For the sake of backwards-compatibility, process type definitions in the old format,
	// but ignore the information after the colon.
	if it.Item().Typ != itemColon {
		return nil, it.Item().Errorf("Missing colon in type declaration. Got %v", it.Item().Val)
	}

	it.Next()
	if it.Item().Typ == itemLeftSquare {
		list = true
		it.Next()
	}

	if it.Item().Typ != itemText {
		return nil, it.Item().Errorf("Missing field type in type declaration. Got %v",
			it.Item().Val)
	}

	it.Next()
	if it.Item().Typ == itemExclamationMark {
		it.Next()
	}

	if list {
		if it.Item().Typ != itemRightSquare {
			return nil, it.Item().Errorf("Expected matching square bracket. Got %v", it.Item().Val)
		}
		it.Next()

		if it.Item().Typ == itemExclamationMark {
			it.Next()
		}
	}

	if it.Item().Typ != itemNewLine {
		return nil, it.Item().Errorf("Expected new line after field declaration. Got %v",
			it.Item().Val)
	}

	glog.Warningf("Type declaration for type %s includes deprecated information about field type "+
		"for field %s which will be ignored.", typeName, x.ParseAttr(field.Predicate))
	return field, nil
}

func parseNamespace(it *lex.ItemIterator) (uint64, error) {
	nextItems, err := it.Peek(2)
	if err != nil {
		return 0, errors.Errorf("Unable to peek: %v", err)
	}
	if nextItems[0].Typ != itemNumber || nextItems[1].Typ != itemRightSquare {
		return 0, errors.Errorf("Typed oes not match the expected")
	}
	ns, err := strconv.ParseUint(nextItems[0].Val, 0, 64)
	if err != nil {
		return 0, err
	}
	it.Next()
	it.Next()
	// We have parsed the namespace. Now move to the next item.
	if !it.Next() {
		return 0, errors.Errorf("No schema found after namespace. Got: %v", nextItems[0])
	}
	return ns, nil
}

// ParsedSchema represents the parsed schema and type updates.
type ParsedSchema struct {
	Preds []*pb.SchemaUpdate
	Types []*pb.TypeUpdate
}

func isTypeDeclaration(item lex.Item, it *lex.ItemIterator) bool {
	if item.Val != "type" {
		return false
	}

	nextItems, err := it.Peek(2)
	switch {
	case err != nil || len(nextItems) != 2:
		return false

	case nextItems[0].Typ != itemText:
		return false

	case nextItems[1].Typ != itemLeftCurl:
		return false
	}

	return true
}

// parse parses a schema string and returns the schema representation for it.
// If namespace == math.MaxUint64, then it preserves the namespace. Else it forces the passed
// namespace on schema/types.

// Example schema:
// [ns1] name: string .
// [ns2] age: string .
// parse(schema, 0) --> All the schema fields go to namespace 0.
// parse(schema, x) --> All the schema fields go to namespace x.
// parse(schema, math.MaxUint64) --> name (ns1), age(ns2) // Preserve the namespace
func parse(s string, namespace uint64) (*ParsedSchema, error) {
	var result ParsedSchema

	var l lex.Lexer
	l.Reset(s)
	l.Run(lexText)
	if err := l.ValidateResult(); err != nil {
		return nil, err
	}

	parseTypeOrSchema := func(item lex.Item, it *lex.ItemIterator, ns uint64) error {
		if isTypeDeclaration(item, it) {
			typeUpdate, err := parseTypeDeclaration(it, ns)
			if err != nil {
				return err
			}
			result.Types = append(result.Types, typeUpdate)
			return nil
		}

		schema, err := parseScalarPair(it, item.Val, ns)
		if err != nil {
			return err
		}
		result.Preds = append(result.Preds, schema)
		return nil
	}

	it := l.NewIterator()
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case lex.ItemEOF:
			if err := resolveTokenizers(result.Preds); err != nil {
				return nil, errors.Wrapf(err, "failed to enrich schema")
			}
			return &result, nil

		case itemText:
			// For schema which does not contain the namespace information, use the default
			// namespace, if namespace has to be preserved. Else, use the passed namespace.
			ns := x.RootNamespace
			if namespace != math.MaxUint64 {
				ns = namespace
			}
			if err := parseTypeOrSchema(item, it, ns); err != nil {
				return nil, err
			}

		case itemLeftSquare:
			// We expect a namespace.
			ns, err := parseNamespace(it)
			if err != nil {
				return nil, errors.Wrapf(err, "While parsing namespace:")
			}
			if namespace != math.MaxUint64 {
				// Use the passed namespace, if we don't want to preserve the namespace.
				ns = namespace
			}
			// We have already called next in parseNamespace.
			item := it.Item()
			if err := parseTypeOrSchema(item, it, ns); err != nil {
				return nil, err
			}

		case itemNewLine:
			// pass empty line

		default:
			return nil, it.Item().Errorf("Unexpected token: %v while parsing schema", item)
		}
	}
	return nil, errors.Errorf("Shouldn't reach here")
}

// Parse parses the schema with namespace preserved. For the types/predicates for which the
// namespace is not specified, it uses default.
func Parse(s string) (*ParsedSchema, error) {
	return parse(s, math.MaxUint64)
}

// ParseWithNamespace parses the schema and forces the given namespace on each of the
// type/predicate.
func ParseWithNamespace(s string, namespace uint64) (*ParsedSchema, error) {
	return parse(s, namespace)
}
