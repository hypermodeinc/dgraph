/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dql

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"github.com/pkg/errors"

	"github.com/hypermodeinc/dgraph/v25/lex"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
)

const (
	uidFunc     = "uid"
	valueFunc   = "val"
	typFunc     = "type"
	lenFunc     = "len"
	countFunc   = "count"
	uidInFunc   = "uid_in"
	similarToFn = "similar_to"
)

var (
	errExpandType = "expand is only compatible with type filters"
)

// GraphQuery stores the parsed Query in a tree format. This gets converted to
// pb.y used query.SubGraph before processing the query.
type GraphQuery struct {
	UID        []uint64
	Attr       string
	Langs      []string
	Alias      string
	IsCount    bool
	IsInternal bool
	IsGroupby  bool
	Var        string
	NeedsVar   []VarContext
	Func       *Function
	Expand     string // Which variable to expand with.

	Args map[string]string
	// Query can have multiple sort parameters.
	Order            []*pb.Order
	Children         []*GraphQuery
	Filter           *FilterTree
	MathExp          *MathTree
	Normalize        bool
	Recurse          bool
	RecurseArgs      RecurseArgs
	ShortestPathArgs ShortestPathArgs
	Cascade          []string
	IgnoreReflex     bool
	Facets           *pb.FacetParams
	FacetsFilter     *FilterTree
	GroupbyAttrs     []GroupByAttr
	FacetVar         map[string]string
	FacetsOrder      []*FacetOrder

	// Used for ACL enabled queries to curtail results to only accessible params
	AllowedPreds []string

	// Internal fields below.
	// If gq.fragment is nonempty, then it is a fragment reference / spread.
	fragment string

	// True for blocks that don't have a starting function and hence no starting nodes. They are
	// used to aggregate and get variables defined in another block.
	IsEmpty bool
}

// RecurseArgs stores the arguments needed to process the @recurse directive.
type RecurseArgs struct {
	Depth     uint64
	AllowLoop bool
	varMap    map[string]string //varMap holds the variable args name. So, that we can substitute the
	// argument in the substitution part.
}

// ShortestPathArgs stores the arguments needed to process the shortest path query.
type ShortestPathArgs struct {
	// From, To can have a uid or a uid function as the argument.
	// 1. from: 0x01
	// 2. from: uid(0x01)
	// 3. from: uid(p) // a variable
	From *Function
	To   *Function
}

// GroupByAttr stores the arguments needed to process the @groupby directive.
type GroupByAttr struct {
	Attr  string
	Alias string
	Langs []string
}

// FacetOrder stores ordering for single facet key.
type FacetOrder struct {
	Key  string
	Desc bool // true if ordering should be decending by this facet.
}

// pair denotes the key value pair that is part of the DQL query root in parenthesis.
type pair struct {
	Key string
	Val string
}

// fragmentNode is an internal structure for doing dfs on fragments.
type fragmentNode struct {
	Name    string
	Gq      *GraphQuery
	Entered bool // Entered in dfs.
	Exited  bool // Exited in dfs.
}

// fragmentMap is used to associate fragment names to their corresponding fragmentNode.
type fragmentMap map[string]*fragmentNode

const (
	AnyVar   = 0
	UidVar   = 1
	ValueVar = 2
	ListVar  = 3
)

// VarContext stores information about the vars needed to complete a query.
type VarContext struct {
	Name string
	Typ  int //  1 for UID vars, 2 for value vars
}

// varInfo holds information on DQL variables.
type varInfo struct {
	Value string
	Type  string
}

// varMap is a map with key as DQL variable name.
type varMap map[string]varInfo

// FilterTree is the result of parsing the filter directive.
// Either you can have `Op and Children` on non-leaf nodes
// Or Func at leaf nodes.
type FilterTree struct {
	Op    string
	Child []*FilterTree
	Func  *Function
}

// Arg stores an argument to a function.
type Arg struct {
	Value      string
	IsValueVar bool // If argument is val(a), e.g. eq(name, val(a))
	IsDQLVar   bool
}

// Function holds the information about dql functions.
type Function struct {
	Attr       string
	Lang       string // language of the attribute value
	Name       string // Specifies the name of the function.
	Args       []Arg  // Contains the arguments of the function.
	UID        []uint64
	NeedsVar   []VarContext // If the function requires some variable
	IsCount    bool         // gt(count(friends),0)
	IsValueVar bool         // eq(val(s), 5)
	IsLenVar   bool         // eq(len(s), 5)
}

// filterOpPrecedence is a map from filterOp (a string) to its precedence.
var filterOpPrecedence = map[string]int{
	"not": 3,
	"and": 2,
	"or":  1,
}
var mathOpPrecedence = map[string]int{
	"u-":      500,
	"floor":   105,
	"ceil":    104,
	"since":   103,
	"exp":     100,
	"ln":      99,
	"sqrt":    98,
	"cond":    90,
	"pow":     89,
	"logbase": 88,
	"max":     85,
	"min":     84,

	// NOTE: Previously, we had "/" at precedence 50 and "*" at precedence 49.
	//       This is problematic because it would evaluate:
	//              5 * 10 / 50 as: 5 * (10/50). This is fine for floating point, but breaks
	//       for integer arithmetic! The result in integer arithmetic is 0, but the precedence
	//       should actually be evaluated as (5*10)/50 = 1.
	"/": 50,
	"*": 50,
	// We add dot as lower priority than "/" and "*" so that the expression:
	//   c1 * v1 dot c2 * v2 gets evaluated as (c1 *  v1) dot (c2 * v2).
	// Note that v dot c where v is a vector and c is a float (or int) is not legal,
	// so we must evaluate this with this precedence! This also implies that we need
	// support for v / c where v is a vector and c is a float, and that this should
	// be interpreted the same as v * (1/c).
	"dot": 49,
	"%":   48,
	"-":   47,
	"+":   46,

	"<":  10,
	">":  9,
	"<=": 8,
	">=": 7,
	"==": 6,
	"!=": 5,
}

// IsAggregator returns true if the function name is an aggregation function.
func (f *Function) IsAggregator() bool {
	return isAggregator(f.Name)
}

// IsPasswordVerifier returns true if the function name is "checkpwd".
func (f *Function) IsPasswordVerifier() bool {
	return f.Name == "checkpwd"
}

// DebugPrint is useful for debugging.
func (gq *GraphQuery) DebugPrint(prefix string) {
	glog.Infof("%s[%x %q %q]\n", prefix, gq.UID, gq.Attr, gq.Alias)
	for _, c := range gq.Children {
		c.DebugPrint(prefix + "|->")
	}
}

func (gq *GraphQuery) isFragment() bool {
	return gq.fragment != ""
}

func (fn *fragmentNode) expand(fmap fragmentMap) error {
	if fn.Exited {
		// This fragment node has already been expanded.
		return nil
	}
	if fn.Entered {
		return errors.Errorf("Cycle detected: %s", fn.Name)
	}
	fn.Entered = true
	if err := fn.Gq.expandFragments(fmap); err != nil {
		return err
	}
	fn.Exited = true
	return nil
}

func (gq *GraphQuery) expandFragments(fmap fragmentMap) error {
	// We have to make a copy of children to preserve order and replace
	// fragment references with fragment content. The copy is newChildren.
	var newChildren []*GraphQuery
	// Expand non-fragments. Do not append to gq.Children.
	for _, child := range gq.Children {
		if child.isFragment() {
			fname := child.fragment // Name of fragment being referenced.
			fchild := fmap[fname]
			if fchild == nil {
				return errors.Errorf("Missing fragment: %s", fname)
			}
			if err := fchild.expand(fmap); err != nil {
				return err
			}
			newChildren = append(newChildren, fchild.Gq.Children...)
		} else {
			if err := child.expandFragments(fmap); err != nil {
				return err
			}
			newChildren = append(newChildren, child)
		}
	}
	gq.Children = newChildren
	return nil
}

func convertToVarMap(variables map[string]string) (vm varMap) {
	vm = make(map[string]varInfo)
	for k, v := range variables {
		vm[k] = varInfo{
			Value: v,
		}
	}
	return vm
}

// Request stores the query text and the variable mapping.
type Request struct {
	Str       string
	Variables map[string]string
}

func parseValue(v varInfo) (types.Val, error) {
	typ := v.Type
	if v.Value == "" {
		return types.Val{}, errors.Errorf("No value found")
	}

	switch typ {
	case "int":
		{
			if i, err := strconv.ParseInt(v.Value, 0, 64); err != nil {
				return types.Val{}, errors.Wrapf(err, "Expected an int but got %v", v.Value)
			} else {
				return types.Val{
					Tid:   types.IntID,
					Value: i,
				}, nil
			}
		}
	case "float":
		{
			if i, err := strconv.ParseFloat(v.Value, 64); err != nil {
				return types.Val{}, errors.Wrapf(err, "Expected a float but got %v", v.Value)
			} else {
				return types.Val{
					Tid:   types.FloatID,
					Value: i,
				}, nil
			}
		}
	case "bool":
		{
			if i, err := strconv.ParseBool(v.Value); err != nil {
				return types.Val{}, errors.Wrapf(err, "Expected a bool but got %v", v.Value)
			} else {
				return types.Val{
					Tid:   types.BoolID,
					Value: i,
				}, nil
			}
		}
	case "float32vector":
		{
			if i, err := types.ParseVFloat(v.Value); err != nil {
				return types.Val{}, errors.Wrapf(err, "Expected a float32vector but got %v", v.Value)
			} else {
				return types.Val{
					Tid:   types.VFloatID,
					Value: i,
				}, nil
			}
		}
	case "string": // Value is a valid string. No checks required.
		return types.Val{
			Tid:   types.StringID,
			Value: v.Value,
		}, nil
	default:
		return types.Val{}, errors.Errorf("Type %q not supported", typ)
	}
}

func checkValueType(vm varMap) error {
	for k, v := range vm {
		typ := v.Type

		if len(typ) == 0 {
			return errors.Errorf("Type of variable %v not specified", k)
		}

		// Ensure value is not nil if the variable is required.
		if typ[len(typ)-1] == '!' {
			if v.Value == "" {
				return errors.Errorf("Variable %v should be initialised", k)
			}
			typ = typ[:len(typ)-1]
		}

		// Type check the values.
		if v.Value != "" {
			switch typ {
			case "int":
				{
					if _, err := strconv.ParseInt(v.Value, 0, 64); err != nil {
						return errors.Wrapf(err, "Expected an int but got %v", v.Value)
					}
				}
			case "float":
				{
					if _, err := strconv.ParseFloat(v.Value, 64); err != nil {
						return errors.Wrapf(err, "Expected a float but got %v", v.Value)
					}
				}
			case "bool":
				{
					if _, err := strconv.ParseBool(v.Value); err != nil {
						return errors.Wrapf(err, "Expected a bool but got %v", v.Value)
					}
				}
			case "float32vector":
				{
					if _, err := types.ParseVFloat(v.Value); err != nil {
						return errors.Wrapf(err, "Expected a vector32float but got %v", v.Value)
					}
				}
			case "string": // Value is a valid string. No checks required.
			default:
				return errors.Errorf("Type %q not supported", typ)
			}
		}
	}

	return nil
}

func substituteVar(f string, res *string, vmap varMap) error {
	if len(f) > 0 && f[0] == '$' {
		va, ok := vmap[f]
		if !ok || va.Type == "" {
			return errors.Errorf("Variable not defined %v", f)
		}
		*res = va.Value
	}
	return nil
}

func substituteVarInMath(gq *GraphQuery, vmap varMap) error {
	return gq.MathExp.subs(vmap)
}

func substituteVariables(gq *GraphQuery, vmap varMap) error {
	for k, v := range gq.Args {
		// v won't be empty as its handled in parseDqlVariables.
		val := gq.Args[k]
		if err := substituteVar(v, &val, vmap); err != nil {
			return err
		}
		gq.Args[k] = val
	}

	idVal, ok := gq.Args["id"]
	if ok && len(gq.UID) == 0 {
		uids, err := parseID(idVal)
		if err != nil {
			return err
		}
		gq.UID = append(gq.UID, uids...)
		// Deleting it here because we don't need to fill it in query.go.
		delete(gq.Args, "id")
	}

	if gq.MathExp != nil {
		if err := substituteVarInMath(gq, vmap); err != nil {
			return err
		}
	}

	if gq.Func != nil {
		if err := substituteVar(gq.Func.Attr, &gq.Func.Attr, vmap); err != nil {
			return err
		}

		for idx, v := range gq.Func.Args {
			if !v.IsDQLVar {
				continue
			}
			if err := substituteVar(v.Value, &gq.Func.Args[idx].Value, vmap); err != nil {
				return err
			}
			if gq.Func.Name == "regexp" {
				if err := regExpVariableFilter(gq.Func, idx); err != nil {
					return err
				}
			}
		}
	}

	for _, child := range gq.Children {
		if err := substituteVariables(child, vmap); err != nil {
			return err
		}
	}
	if gq.Filter != nil {
		if err := substituteVariablesFilter(gq.Filter, vmap); err != nil {
			return err
		}
	}
	if gq.FacetsFilter != nil {
		if err := substituteVariablesFilter(gq.FacetsFilter, vmap); err != nil {
			return err
		}
	}
	if gq.RecurseArgs.varMap != nil {
		// Update the depth if the get the depth as a variable in the query.
		varName, ok := gq.RecurseArgs.varMap["depth"]
		if ok {
			val, ok := vmap[varName]
			if !ok {
				return errors.Errorf("variable %s not defined", varName)
			}
			depth, err := strconv.ParseUint(val.Value, 0, 64)
			if err != nil {
				return errors.Wrapf(err, "%v should be type of integer", varName)
			}
			gq.RecurseArgs.Depth = depth
		}

		// Update the loop if the get the loop as a variable in the query.
		varName, ok = gq.RecurseArgs.varMap["loop"]
		if ok {
			val, ok := vmap[varName]
			if !ok {
				return errors.Errorf("variable %s not defined", varName)
			}
			allowLoop, err := strconv.ParseBool(val.Value)
			if err != nil {
				return errors.Wrapf(err, "%v should be type of boolean", varName)
			}
			gq.RecurseArgs.AllowLoop = allowLoop
		}

	}
	return nil
}

func regExpVariableFilter(f *Function, idx int) error {
	// Value should have been populated from the map that the user gave us in the
	// DQL variable map. Let's parse the expression and flags from the variable
	// string.
	ra, err := parseRegexArgs(f.Args[idx].Value)
	if err != nil {
		return err
	}
	// We modify the value of this arg and add a new arg for the flags. Regex functions
	// should have two args.
	f.Args[idx].Value = ra.expr
	f.Args = append(f.Args, Arg{Value: ra.flags})
	return nil
}

func substituteVariablesFilter(f *FilterTree, vmap varMap) error {
	if f == nil {
		return nil
	}

	if f.Func != nil {
		if err := substituteVar(f.Func.Attr, &f.Func.Attr, vmap); err != nil {
			return err
		}

		for idx, v := range f.Func.Args {
			if !v.IsDQLVar {
				continue
			}
			if f.Func.Name == uidFunc {
				// This is to support DQL variables in uid functions.
				idVal, ok := vmap[v.Value]
				if !ok {
					return errors.Errorf("Couldn't find value for DQL variable: [%s]", v.Value)
				}
				uids, err := parseID(idVal.Value)
				if err != nil {
					return err
				}
				f.Func.UID = append(f.Func.UID, uids...)
				continue
			}

			if err := substituteVar(v.Value, &f.Func.Args[idx].Value, vmap); err != nil {
				return err
			}

			// We need to parse the regexp after substituting it from a DQL Variable.
			_, ok := vmap[v.Value]
			if f.Func.Name == "regexp" && ok {
				if err := regExpVariableFilter(f.Func, idx); err != nil {
					return err
				}
			}
		}
	}

	for _, fChild := range f.Child {
		if err := substituteVariablesFilter(fChild, vmap); err != nil {
			return err
		}
	}
	return nil
}

// Vars struct contains the list of variables defined and used by a
// query block.
type Vars struct {
	Defines []string
	Needs   []string
}

// Result struct contains the Query list, its corresponding variable use list
// and the mutation block.
type Result struct {
	Query     []*GraphQuery
	QueryVars []*Vars
	Schema    *pb.SchemaRequest
}

// Parse initializes and runs the lexer. It also constructs the GraphQuery subgraph
// from the lexed items.
func Parse(r Request) (Result, error) {
	return ParseWithNeedVars(r, nil)
}

// ParseWithNeedVars performs parsing of a query with given needVars.
//
// The needVars parameter is passed in the case of upsert block.
// For example, when parsing the query block inside -
//
//	upsert {
//	  query {
//	    me(func: eq(email, "someone@gmail.com"), first: 1) {
//	      v as uid
//	    }
//	  }
//
//	  mutation {
//	    set {
//	      uid(v) <name> "Some One" .
//	      uid(v) <email> "someone@gmail.com" .
//	    }
//	  }
//	}
//
// The variable name v needs to be passed through the needVars parameter. Otherwise, an error
// is reported complaining that the variable v is defined but not used in the query block.
func ParseWithNeedVars(r Request, needVars []string) (res Result, rerr error) {
	query := r.Str
	vmap := convertToVarMap(r.Variables)

	var lexer lex.Lexer
	lexer.Reset(query)
	lexer.Run(lexTopLevel)
	if err := lexer.ValidateResult(); err != nil {
		return res, err
	}

	var qu *GraphQuery
	it := lexer.NewIterator()
	fmap := make(fragmentMap)
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case itemOpType:
			switch item.Val {
			case "mutation":
				return res, item.Errorf("Mutation block no longer allowed.")
			case "schema":
				if res.Schema != nil {
					return res, item.Errorf("Only one schema block allowed ")
				}
				if res.Query != nil {
					return res, item.Errorf("Schema block is not allowed with query block")
				}
				if res.Schema, rerr = getSchema(it); rerr != nil {
					return res, rerr
				}
			case "fragment":
				// TODO(jchiu0): This is to be done in ParseSchema once it is ready.
				fnode, rerr := getFragment(it)
				if rerr != nil {
					return res, rerr
				}
				fmap[fnode.Name] = fnode
			case "query":
				if res.Schema != nil {
					return res, item.Errorf("Schema block is not allowed with query block")
				}
				if qu, rerr = getVariablesAndQuery(it, vmap); rerr != nil {
					return res, rerr
				}
				res.Query = append(res.Query, qu)
			}
		case itemLeftCurl:
			if qu, rerr = getQuery(it); rerr != nil {
				return res, rerr
			}
			res.Query = append(res.Query, qu)
		case itemName:
			it.Prev()
			if qu, rerr = getQuery(it); rerr != nil {
				return res, rerr
			}
			res.Query = append(res.Query, qu)
		}
	}

	if len(res.Query) != 0 {
		res.QueryVars = make([]*Vars, 0, len(res.Query))
		for i := range res.Query {
			qu := res.Query[i]
			// Try expanding fragments using fragment map.
			if err := qu.expandFragments(fmap); err != nil {
				return res, err
			}

			// Substitute all DQL variables with corresponding values
			if err := substituteVariables(qu, vmap); err != nil {
				return res, err
			}

			res.QueryVars = append(res.QueryVars, &Vars{})
			// Collect vars used and defined in Result struct.
			qu.collectVars(res.QueryVars[i])
		}

		allVars := res.QueryVars
		// Add the variables that are needed outside the query block.
		// For example, mutation block in upsert block will be using
		// variables from the query block that is getting parsed here.
		if len(needVars) != 0 {
			allVars = append(allVars, &Vars{Needs: needVars})
		}
		if err := checkDependency(allVars); err != nil {
			return res, err
		}
	}

	if err := validateResult(&res); err != nil {
		return res, err
	}

	return res, nil
}

func validateResult(res *Result) error {
	seenQueryAliases := make(map[string]bool)
	for _, q := range res.Query {
		if q.Alias == "var" || q.Alias == "shortest" {
			continue
		}
		if _, found := seenQueryAliases[q.Alias]; found {
			return errors.Errorf("Duplicate aliases not allowed: %v", q.Alias)
		}
		seenQueryAliases[q.Alias] = true
	}
	return nil
}

func flatten(vl []*Vars) (needs []string, defines []string) {
	needs, defines = make([]string, 0, 10), make([]string, 0, 10)
	for _, it := range vl {
		needs = append(needs, it.Needs...)
		defines = append(defines, it.Defines...)
	}
	return
}

func checkDependency(vl []*Vars) error {
	needs, defines := flatten(vl)

	needs = x.RemoveDuplicates(needs)
	lenBefore := len(defines)
	defines = x.RemoveDuplicates(defines)

	if len(defines) != lenBefore {
		return errors.Errorf("Some variables are declared multiple times.")
	}
	if len(defines) > len(needs) {
		return errors.Errorf("Some variables are defined but not used\nDefined:%v\nUsed:%v\n",
			defines, needs)
	}
	if len(defines) < len(needs) {
		return errors.Errorf("Some variables are used but not defined\nDefined:%v\nUsed:%v\n",
			defines, needs)
	}

	for i := range defines {
		if defines[i] != needs[i] {
			return errors.Errorf("Variables are not used properly. \nDefined:%v\nUsed:%v\n",
				defines, needs)
		}
	}
	return nil
}

func (gq *GraphQuery) collectVars(v *Vars) {
	if gq.Var != "" {
		v.Defines = append(v.Defines, gq.Var)
	}
	if gq.FacetVar != nil {
		for _, va := range gq.FacetVar {
			v.Defines = append(v.Defines, va)
		}
	}
	for _, va := range gq.NeedsVar {
		v.Needs = append(v.Needs, va.Name)
	}

	for _, ch := range gq.Children {
		ch.collectVars(v)
	}
	if gq.Filter != nil {
		gq.Filter.collectVars(v)
	}
	if gq.MathExp != nil {
		gq.MathExp.collectVars(v)
	}

	shortestPathFrom := gq.ShortestPathArgs.From
	if shortestPathFrom != nil && len(shortestPathFrom.NeedsVar) > 0 {
		v.Needs = append(v.Needs, shortestPathFrom.NeedsVar[0].Name)
	}
	shortestPathTo := gq.ShortestPathArgs.To
	if shortestPathTo != nil && len(shortestPathTo.NeedsVar) > 0 {
		v.Needs = append(v.Needs, shortestPathTo.NeedsVar[0].Name)
	}
}

func (f *MathTree) collectVars(v *Vars) {
	if f == nil {
		return
	}
	if f.Var != "" {
		v.Needs = append(v.Needs, f.Var)
		return
	}
	for _, fch := range f.Child {
		fch.collectVars(v)
	}
}

func (f *FilterTree) collectVars(v *Vars) {
	if f.Func != nil {
		for _, va := range f.Func.NeedsVar {
			v.Needs = append(v.Needs, va.Name)
		}
	}
	for _, fch := range f.Child {
		fch.collectVars(v)
	}
}

func (f *FilterTree) hasVars() bool {
	if (f.Func != nil) && (len(f.Func.NeedsVar) > 0) {
		return true
	}
	for _, fch := range f.Child {
		if fch.hasVars() {
			return true
		}
	}
	return false
}

// getVariablesAndQuery checks if the query has a variable list and stores it in
// vmap. For variable list to be present, the query should have a name which is
// also checked for. It also calls getQuery to create the GraphQuery object tree.
func getVariablesAndQuery(it *lex.ItemIterator, vmap varMap) (gq *GraphQuery, rerr error) {
	var name string
L2:
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case itemName:
			if name != "" {
				return nil, item.Errorf("Multiple word query name not allowed.")
			}
			name = item.Val
		case itemLeftRound:
			if name == "" {
				return nil, item.Errorf("Variables can be defined only in named queries.")
			}

			if rerr = parseDqlVariables(it, vmap); rerr != nil {
				return nil, rerr
			}

			if rerr = checkValueType(vmap); rerr != nil {
				return nil, rerr
			}
		case itemLeftCurl:
			if gq, rerr = getQuery(it); rerr != nil {
				return nil, rerr
			}
			break L2
		}
	}

	return gq, nil
}

// parseVarName returns the variable name.
func parseVarName(it *lex.ItemIterator) (string, error) {
	val := "$"
	var consumeAtLeast bool
	for {
		items, err := it.Peek(1)
		if err != nil {
			return val, err
		}
		if items[0].Typ != itemName {
			if !consumeAtLeast {
				return "", it.Errorf("Expected variable name after $")
			}
			break
		}
		consumeAtLeast = true
		val += items[0].Val
		// Consume the current item.
		if !it.Next() {
			break
		}
	}
	return val, nil
}

func parseRecurseArgs(it *lex.ItemIterator, gq *GraphQuery) error {
	if ok := trySkipItemTyp(it, itemLeftRound); !ok {
		// We don't have a (, we can return.
		return nil
	}

	var key, val string
	var ok bool
	for it.Next() {
		item := it.Item()
		if item.Typ != itemName {
			return item.Errorf("Expected key inside @recurse()")
		}
		key = strings.ToLower(item.Val)

		if ok := trySkipItemTyp(it, itemColon); !ok {
			return it.Errorf("Expected colon(:) after %s", key)
		}
		if !it.Next() {
			return it.Errorf("Expected argument")
		}

		// Consume the next item.
		item = it.Item()
		val = item.Val
		switch key {
		case "depth":
			// Check whether the argument is variable or value.
			if item.Typ == itemDollar {
				// Consume the variable name.
				varName, err := parseVarName(it)
				if err != nil {
					return err
				}
				if gq.RecurseArgs.varMap == nil {
					gq.RecurseArgs.varMap = make(map[string]string)
				}
				gq.RecurseArgs.varMap["depth"] = varName
			} else {
				if item.Typ != itemName {
					return item.Errorf("Expected value inside @recurse() for key: %s", key)
				}
				depth, err := strconv.ParseUint(val, 0, 64)
				if err != nil {
					return errors.New("Value inside depth should be type of integer")
				}
				gq.RecurseArgs.Depth = depth
			}
		case "loop":
			if item.Typ == itemDollar {
				// Consume the variable name.
				varName, err := parseVarName(it)
				if err != nil {
					return err
				}
				if gq.RecurseArgs.varMap == nil {
					gq.RecurseArgs.varMap = make(map[string]string)
				}
				gq.RecurseArgs.varMap["loop"] = varName
			} else {
				allowLoop, err := strconv.ParseBool(val)
				if err != nil {
					return errors.New("Value inside loop should be type of boolean")
				}
				gq.RecurseArgs.AllowLoop = allowLoop
			}
		default:
			return item.Errorf("Unexpected key: [%s] inside @recurse block", key)
		}

		if _, ok = tryParseItemType(it, itemRightRound); ok {
			return nil
		}

		if _, ok := tryParseItemType(it, itemComma); !ok {
			return it.Errorf("Expected comma after value: %s inside recurse block", val)
		}
	}
	return nil
}

// getQuery creates a GraphQuery object tree by calling getRoot
// and goDeep functions by looking at '{'.
func getQuery(it *lex.ItemIterator) (gq *GraphQuery, rerr error) {
	// First, get the root
	gq, rerr = getRoot(it)
	if rerr != nil {
		return nil, rerr
	}

	var seenFilter bool
L:
	// Recurse to deeper levels through godeep.
	if !it.Next() {
		return nil, it.Errorf("Expecting more lexer items while parsing query")
	}

	item := it.Item()
	switch item.Typ {
	case itemLeftCurl:
		if rerr = godeep(it, gq); rerr != nil {
			return nil, rerr
		}
	case itemAt:
		it.Next()
		item := it.Item()
		if item.Typ == itemName {
			switch strings.ToLower(item.Val) {
			case "filter":
				if seenFilter {
					return nil, item.Errorf("Repeated filter at root")
				}
				seenFilter = true
				filter, err := parseFilter(it)
				if err != nil {
					return nil, err
				}
				gq.Filter = filter

			case "normalize":
				gq.Normalize = true
			case "cascade":
				if err := parseCascade(it, gq); err != nil {
					return nil, err
				}
			case "groupby":
				gq.IsGroupby = true
				if err := parseGroupby(it, gq); err != nil {
					return nil, err
				}
			case "ignorereflex":
				gq.IgnoreReflex = true
			case "recurse":
				gq.Recurse = true
				if err := parseRecurseArgs(it, gq); err != nil {
					return nil, err
				}
			default:
				return nil, item.Errorf("Unknown directive [%s]", item.Val)
			}
			goto L
		}
	case itemRightCurl:
		// Do nothing.
	case itemName:
		it.Prev()
		return gq, nil
	default:
		return nil, item.Errorf("Malformed Query. Missing {. Got %v", item.Val)
	}

	return gq, nil
}

// getFragment parses a fragment definition (not reference).
func getFragment(it *lex.ItemIterator) (*fragmentNode, error) {
	var name string
loop:
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case itemName:
			v := strings.TrimSpace(item.Val)
			if len(v) > 0 && name == "" {
				// Currently, we take the first nontrivial token as the
				// fragment name and ignore everything after that until we see
				// a left curl.
				name = v
			}
		case itemLeftCurl:
			break loop
		default:
			return nil, item.Errorf("Unexpected item in fragment: %v %v", item.Typ, item.Val)
		}
	}
	if name == "" {
		return nil, it.Errorf("Empty fragment name")
	}

	gq := &GraphQuery{
		Args: make(map[string]string),
	}
	if err := godeep(it, gq); err != nil {
		return nil, err
	}
	fn := &fragmentNode{
		Name: name,
		Gq:   gq,
	}
	return fn, nil
}

// parses till rightSquare is found (parses [a, b]) excluding leftSquare
// This function can be reused for query later
func parseListItemNames(it *lex.ItemIterator) ([]string, error) {
	var items []string
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case itemRightSquare:
			return items, nil
		case itemName:
			val := collectName(it, item.Val)
			items = append(items, val)
		case itemComma:
			it.Next()
			item = it.Item()
			if item.Typ != itemName {
				return items, item.Errorf("Invalid scheam block")
			}
			val := collectName(it, item.Val)
			items = append(items, val)
		default:
			return items, item.Errorf("Invalid schema block")
		}
	}
	return items, it.Errorf("Expecting ] to end list but none was found")
}

// parseSchemaPredsOrTypes parses till rightround is found
func parseSchemaPredsOrTypes(it *lex.ItemIterator, s *pb.SchemaRequest) error {
	// pred or type should be followed by colon
	it.Next()
	item := it.Item()
	if item.Typ != itemName && !(item.Val == "pred" || item.Val == "type") {
		return item.Errorf("Invalid schema block")
	}
	parseTypes := false
	if item.Val == "type" {
		parseTypes = true
	}

	it.Next()
	item = it.Item()
	if item.Typ != itemColon {
		return item.Errorf("Invalid schema block")
	}

	// can be a or [a,b]
	it.Next()
	item = it.Item()
	switch item.Typ {
	case itemName:
		if parseTypes {
			s.Types = append(s.Types, item.Val)
		} else {
			s.Predicates = append(s.Predicates, item.Val)
		}
	case itemLeftSquare:
		names, err := parseListItemNames(it)
		if err != nil {
			return err
		}

		if parseTypes {
			s.Types = names
		} else {
			s.Predicates = names
		}
	default:
		return item.Errorf("Invalid schema block")
	}

	it.Next()
	item = it.Item()
	if item.Typ == itemRightRound {
		return nil
	}
	return item.Errorf("Invalid schema blocks")
}

// parses till rightcurl is found
func parseSchemaFields(it *lex.ItemIterator, s *pb.SchemaRequest) error {
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case itemRightCurl:
			return nil
		case itemName:
			s.Fields = append(s.Fields, item.Val)
		default:
			return item.Errorf("Invalid schema block.")
		}
	}
	return it.Errorf("Expecting } to end fields list, but none was found")
}

func getSchema(it *lex.ItemIterator) (*pb.SchemaRequest, error) {
	var s pb.SchemaRequest
	leftRoundSeen := false
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case itemLeftCurl:
			if err := parseSchemaFields(it, &s); err != nil {
				return nil, err
			}
			return &s, nil
		case itemLeftRound:
			if leftRoundSeen {
				return nil, item.Errorf("Too many left rounds in schema block")
			}
			leftRoundSeen = true
			if err := parseSchemaPredsOrTypes(it, &s); err != nil {
				return nil, err
			}
		default:
			return nil, item.Errorf("Invalid schema block")
		}
	}
	return nil, it.Errorf("Invalid schema block.")
}

// parseDqlVariables parses the the DQL variable declaration.
func parseDqlVariables(it *lex.ItemIterator, vmap varMap) error {
	expectArg := true
	if item, ok := it.PeekOne(); ok && item.Typ == itemRightRound {
		return nil
	}
loop:
	for it.Next() {
		var varName string
		// Get variable name.
		item := it.Item()
		switch item.Typ {
		case itemDollar:
			if !expectArg {
				return item.Errorf("Missing comma in var declaration")
			}
			it.Next()
			item = it.Item()
			if item.Typ == itemName {
				varName = fmt.Sprintf("$%s", item.Val)
			} else {
				return item.Errorf("Expecting a variable name. Got: %v", item)
			}
		case itemRightRound:
			if expectArg {
				return item.Errorf("Invalid comma in var block")
			}
			break loop
		case itemComma:
			if expectArg {
				return item.Errorf("Invalid comma in var block")
			}
			expectArg = true
			continue
		default:
			return item.Errorf("Unexpected item in place of variable. Got: %v %v", item,
				item.Typ == itemDollar)
		}

		it.Next()
		item = it.Item()
		if item.Typ != itemColon {
			return item.Errorf("Expecting a colon. Got: %v", item)
		}

		// Get variable type.
		it.Next()
		item = it.Item()
		if item.Typ != itemName {
			return item.Errorf("Expecting a variable type. Got: %v", item)
		}

		// Ensure that the type is not nil.
		varType := item.Val
		if varType == "" {
			return item.Errorf("Type of a variable can't be empty")
		}
		it.Next()
		item = it.Item()
		if item.Typ == itemMathOp && item.Val == "!" {
			varType += item.Val
			it.Next()
			item = it.Item()
		}
		// Insert the variable into the map. The variable might already be defiend
		// in the variable list passed with the query.
		if _, ok := vmap[varName]; ok {
			vmap[varName] = varInfo{
				Value: vmap[varName].Value,
				Type:  varType,
			}
		} else {
			vmap[varName] = varInfo{
				Type: varType,
			}
		}

		// Check for '=' sign and optional default value.
		switch item.Typ {
		case itemEqual:
			it.Next()
			it := it.Item()
			if it.Typ != itemName {
				return item.Errorf("Expecting default value of a variable. Got: %v", item)
			}

			if varType[len(varType)-1] == '!' {
				return item.Errorf("Type ending with ! can't have default value: Got: %v", varType)
			}

			// If value is empty replace, otherwise ignore the default value
			// as the intialised value will override the default value.
			if vmap[varName].Value == "" {
				uq, err := unquoteIfQuoted(it.Val)
				if err != nil {
					return err
				}
				vmap[varName] = varInfo{
					Value: uq,
					Type:  varType,
				}
			}
		case itemRightRound:
			break loop
		default:
			// We consumed an extra item to see if it was an '=' sign, so move back.
			it.Prev()
		}
		expectArg = false
	}
	return nil
}

// unquoteIfQuoted checks if str is quoted (starts and ends with quotes). If
// so, it tries to unquote str possibly returning an error. Otherwise, the
// original value is returned.
func unquoteIfQuoted(str string) (string, error) {
	if len(str) < 2 || str[0] != quote || str[len(str)-1] != quote {
		return str, nil
	}
	uq, err := strconv.Unquote(str)
	return uq, errors.Wrapf(err, "could not unquote %q:", str)
}

// parseArguments parses the arguments part of the DQL query root.
func parseArguments(it *lex.ItemIterator, gq *GraphQuery) (result []pair, rerr error) {
	expectArg := true
	orderCount := 0
loop:
	for it.Next() {
		var p pair
		// Get key.
		item := it.Item()
		switch item.Typ {
		case itemName:
			if !expectArg {
				return result, item.Errorf("Expecting a comma. But got: %v", item.Val)
			}
			p.Key = collectName(it, item.Val)
			if isSortkey(p.Key) {
				orderCount++
			}
			expectArg = false
		case itemRightRound:
			if expectArg {
				return result, item.Errorf("Expected argument but got ')'.")
			}
			break loop
		case itemComma:
			if expectArg {
				return result, item.Errorf("Expected Argument but got comma.")
			}
			expectArg = true
			continue
		default:
			return result, item.Errorf("Expecting argument name. Got: %v", item)
		}

		it.Next()
		item = it.Item()
		if item.Typ != itemColon {
			return result, item.Errorf("Expecting a colon. Got: %v in %v", item, gq.Attr)
		}

		// Get value.
		it.Next()
		item = it.Item()
		var val string
		if item.Val == valueFunc {
			count, err := parseVarList(it, gq)
			if err != nil {
				return result, err
			}
			if count != 1 {
				return result, item.Errorf("Only one variable expected. Got %d", count)
			}
			gq.NeedsVar[len(gq.NeedsVar)-1].Typ = ValueVar
			p.Val = gq.NeedsVar[len(gq.NeedsVar)-1].Name
			result = append(result, p)
			if isSortkey(p.Key) && orderCount > 1 {
				return result, item.Errorf("Multiple sorting only allowed by predicates. Got: %+v",
					p.Val)
			}
			continue
		}

		switch {
		case item.Typ == itemDollar:
			val = "$"
			it.Next()
			item = it.Item()
			if item.Typ != itemName {
				return result, item.Errorf("Expecting argument value. Got: %v", item)
			}
		case item.Typ == itemMathOp:
			if item.Val != "+" && item.Val != "-" {
				return result, item.Errorf("Only Plus and minus are allowed unary ops. Got: %v",
					item.Val)
			}
			val = item.Val
			it.Next()
			item = it.Item()
		case item.Typ != itemName:
			return result, item.Errorf("Expecting argument value. Got: %v", item)
		}

		p.Val = collectName(it, val+item.Val)

		// Get language list, if present
		items, err := it.Peek(1)
		if err == nil && items[0].Typ == itemAt {
			it.Next() // consume '@'
			it.Next() // move forward
			langs, err := parseLanguageList(it)
			if err != nil {
				return nil, err
			}
			p.Val = p.Val + "@" + strings.Join(langs, ":")
		}

		result = append(result, p)
	}
	return result, nil
}

// debugString converts FilterTree to a string. Good for testing, debugging.
// nolint: unused
func (f *FilterTree) debugString() string {
	buf := bytes.NewBuffer(make([]byte, 0, 20))
	f.stringHelper(buf)
	return buf.String()
}

// stringHelper does simple DFS to convert FilterTree to string.
// nolint: unused
func (f *FilterTree) stringHelper(buf *bytes.Buffer) {
	x.AssertTrue(f != nil)
	if f.Func != nil && len(f.Func.Name) > 0 {
		// Leaf node.
		x.Check2(buf.WriteRune('('))
		x.Check2(buf.WriteString(f.Func.Name))

		if len(f.Func.Attr) > 0 {
			x.Check2(buf.WriteRune(' '))
			switch {
			case f.Func.IsCount:
				x.Check2(buf.WriteString("count("))
			case f.Func.IsValueVar:
				x.Check2(buf.WriteString("val("))
			case f.Func.IsLenVar:
				x.Check2(buf.WriteString("len("))
			}
			x.Check2(buf.WriteString(f.Func.Attr))
			if f.Func.IsCount || f.Func.IsValueVar || f.Func.IsLenVar {
				x.Check2(buf.WriteRune(')'))
			}
			if len(f.Func.Lang) > 0 {
				x.Check2(buf.WriteRune('@'))
				x.Check2(buf.WriteString(f.Func.Lang))
			}

			for _, arg := range f.Func.Args {
				if arg.IsValueVar {
					x.Check2(buf.WriteString(" val("))
				} else {
					x.Check2(buf.WriteString(" \""))
				}
				x.Check2(buf.WriteString(arg.Value))
				if arg.IsValueVar {
					x.Check2(buf.WriteRune(')'))
				} else {
					x.Check2(buf.WriteRune('"'))
				}
			}
		}
		x.Check2(buf.WriteRune(')'))
		return
	}
	// Non-leaf node.
	x.Check2(buf.WriteRune('('))
	switch f.Op {
	case "and":
		x.Check2(buf.WriteString("AND"))
	case "or":
		x.Check2(buf.WriteString("OR"))
	case "not":
		x.Check2(buf.WriteString("NOT"))
	default:
		x.Fatalf("Unknown operator: %q", f.Op)
	}

	for _, c := range f.Child {
		x.Check2(buf.WriteRune(' '))
		c.stringHelper(buf)
	}
	x.Check2(buf.WriteRune(')'))
}

type filterTreeStack struct{ a []*FilterTree }

func (s *filterTreeStack) empty() bool        { return len(s.a) == 0 }
func (s *filterTreeStack) size() int          { return len(s.a) }
func (s *filterTreeStack) push(t *FilterTree) { s.a = append(s.a, t) }

func (s *filterTreeStack) popAssert() *FilterTree {
	x.AssertTrue(!s.empty())
	last := s.a[len(s.a)-1]
	s.a = s.a[:len(s.a)-1]
	return last
}

func (s *filterTreeStack) pop() (*FilterTree, error) {
	if s.empty() {
		return nil, errors.Errorf("Empty stack")
	}
	last := s.a[len(s.a)-1]
	s.a = s.a[:len(s.a)-1]
	return last, nil
}

func (s *filterTreeStack) peek() *FilterTree {
	x.AssertTruef(!s.empty(), "Trying to peek empty stack")
	return s.a[len(s.a)-1]
}

func evalStack(opStack, valueStack *filterTreeStack) error {
	topOp, err := opStack.pop()
	if err != nil {
		return errors.Errorf("Invalid filter statement")
	}
	if topOp.Op == "not" {
		// Since "not" is a unary operator, just pop one value.
		topVal, err := valueStack.pop()
		if err != nil {
			return errors.Errorf("Invalid filter statement")
		}
		topOp.Child = []*FilterTree{topVal}
	} else {
		// "and" and "or" are binary operators, so pop two values.
		if valueStack.size() < 2 {
			return errors.Errorf("Invalid filter statement")
		}
		topVal1 := valueStack.popAssert()
		topVal2 := valueStack.popAssert()
		topOp.Child = []*FilterTree{topVal2, topVal1}
	}
	// Push the new value (tree) into the valueStack.
	valueStack.push(topOp)
	return nil
}

func parseGeoArgs(it *lex.ItemIterator, g *Function) error {
	buf := new(bytes.Buffer)
	if _, err := buf.WriteString("["); err != nil {
		return err
	}
	depth := 1
loop:
	for {
		if valid := it.Next(); !valid {
			return it.Errorf("Got EOF while parsing Geo tokens")
		}
		item := it.Item()
		switch item.Typ {
		case itemLeftSquare:
			if _, err := buf.WriteString(item.Val); err != nil {
				return err
			}
			depth++
		case itemRightSquare:
			if _, err := buf.WriteString(item.Val); err != nil {
				return err
			}
			depth--
		case itemMathOp, itemComma, itemName:
			// Writing tokens to buffer.
			if _, err := buf.WriteString(item.Val); err != nil {
				return err
			}
		default:
			return item.Errorf("Found invalid item: %s while parsing geo arguments.",
				item.Val)
		}

		switch {
		case depth > 4 || depth < 0:
			return item.Errorf("Invalid bracket sequence")
		case depth == 0:
			break loop
		}
	}
	// Lets append the concatenated Geo token to Args.
	// TODO - See if we can directly encode to Geo format.
	g.Args = append(g.Args, Arg{Value: buf.String()})
	items, err := it.Peek(1)
	if err != nil {
		return it.Errorf("Unexpected EOF while parsing args")
	}
	item := items[0]
	if item.Typ != itemRightRound && item.Typ != itemComma {
		return item.Errorf("Expected right round or comma. Got: %+v",
			items[0])
	}
	return nil
}

// parseFuncArgs will try to parse the arguments inside an array ([]). If the values
// are prefixed with $ they are treated as Dql variables, otherwise they are used as scalar values.
// Returns nil on success while appending arguments to the function Args slice. Otherwise
// returns an error, which can be a parsing or value error.
func parseFuncArgs(it *lex.ItemIterator, g *Function) error {
	var expectArg, isDollar bool

	expectArg = true
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case itemRightSquare:
			return nil
		case itemDollar:
			if !expectArg {
				return item.Errorf("Missing comma in argument list declaration")
			}
			if item, ok := it.PeekOne(); !ok || item.Typ != itemName {
				return item.Errorf("Expecting a variable name. Got: %v", item)
			}
			isDollar = true
			continue
		case itemName:
			// This is not a $variable, just add the value.
			if !isDollar {
				val, err := getValueArg(item.Val)
				if err != nil {
					return err
				}
				g.Args = append(g.Args, Arg{Value: val})
				break
			}
			// This is a $variable that must be expanded later.
			val := "$" + item.Val
			g.Args = append(g.Args, Arg{Value: val, IsDQLVar: true})
		case itemComma:
			if expectArg {
				return item.Errorf("Invalid comma in argument list")
			}
			expectArg = true
			continue
		default:
			return item.Errorf("Invalid arg list")
		}
		expectArg = false
		isDollar = false
	}
	return it.Errorf("Expecting ] to end list but got %v instead", it.Item().Val)
}

// getValueArg returns a space-trimmed and unquoted version of val.
// Returns the cleaned string, otherwise empty string and an error.
func getValueArg(val string) (string, error) {
	return unquoteIfQuoted(strings.TrimSpace(val))
}

func validFuncName(name string) bool {
	if isGeoFunc(name) || IsInequalityFn(name) {
		return true
	}

	switch name {
	case "regexp", "anyofterms", "allofterms", "alloftext", "anyoftext", "ngram",
		"has", "uid", "uid_in", "anyof", "allof", "type", "match", "similar_to":
		return true
	}
	return false
}

type regexArgs struct {
	expr  string
	flags string
}

func parseRegexArgs(val string) (regexArgs, error) {
	// it is possible that val comes in a variable and has not been validated yet.
	// we do the validation here and ensure that it looks like: /<expr>/.
	if len(val) == 0 {
		return regexArgs{}, errors.Errorf("Found empty regex: [%s]", val)
	}
	end := strings.LastIndex(val, "/")
	if end < 1 || val[0] != '/' {
		return regexArgs{}, errors.Errorf("Found invalid regex: [%s]", val)
	}

	expr := strings.Replace(val[1:end], "\\/", "/", -1)
	flags := ""
	if end+1 < len(val) {
		flags = val[end+1:]
	}

	return regexArgs{expr, flags}, nil
}

func parseFunction(it *lex.ItemIterator, gq *GraphQuery) (*Function, error) {
	function := &Function{}
	var expectArg, seenFuncArg, expectLang, isDollar bool
L:
	for it.Next() {
		item := it.Item()
		if item.Typ != itemName {
			return nil, item.Errorf("Expected a function but got %q", item.Val)

		}

		name := collectName(it, item.Val)
		function.Name = strings.ToLower(name)
		if _, ok := tryParseItemType(it, itemLeftRound); !ok {
			return nil, it.Errorf("Expected ( after func name [%s]", function.Name)
		}

		attrItemsAgo := -1
		expectArg = true
		for it.Next() {
			itemInFunc := it.Item()
			if attrItemsAgo >= 0 {
				attrItemsAgo++
			}
			var val string
			switch itemInFunc.Typ {
			case itemRightRound:
				break L
			case itemComma:
				if expectArg {
					return nil, itemInFunc.Errorf("Invalid use of comma.")
				}
				if isDollar {
					return nil, itemInFunc.Errorf("Invalid use of comma after dollar.")
				}
				expectArg = true
				continue
			case itemLeftRound:
				// Function inside a function.
				if seenFuncArg {
					return nil, itemInFunc.Errorf("Multiple functions as arguments not allowed")
				}
				it.Prev()
				it.Prev()
				nestedFunc, err := parseFunction(it, gq)
				if err != nil {
					return nil, err
				}
				seenFuncArg = true
				switch nestedFunc.Name {
				case valueFunc:
					if len(nestedFunc.NeedsVar) > 1 {
						return nil, itemInFunc.Errorf("Multiple variables not allowed in a function")
					}
					// Variable is used in place of attribute, eq(val(a), 5)
					if len(function.Attr) == 0 {
						function.Attr = nestedFunc.NeedsVar[0].Name
						function.IsValueVar = true
					} else {
						// eq(name, val(a))
						function.Args = append(function.Args,
							Arg{Value: nestedFunc.NeedsVar[0].Name, IsValueVar: true})
					}
					function.NeedsVar = append(function.NeedsVar, nestedFunc.NeedsVar...)
					function.NeedsVar[0].Typ = ValueVar
				case lenFunc:
					if len(nestedFunc.NeedsVar) > 1 {
						return nil,
							itemInFunc.Errorf("Multiple variables not allowed in len function")
					}
					if !IsInequalityFn(function.Name) {
						return nil,
							itemInFunc.Errorf("len function only allowed inside inequality" +
								" function")
					}
					if len(function.Attr) == 0 {
						// eq(len(a), 1)
						function.Attr = nestedFunc.NeedsVar[0].Name
						function.IsLenVar = true
					} else {
						// eq(1, len(a))
						return nil, itemInFunc.Errorf("incorrect order, the first argument should be len function")
					}
					function.NeedsVar = append(function.NeedsVar, nestedFunc.NeedsVar...)
				case countFunc:
					function.Attr = nestedFunc.Attr
					function.IsCount = true
				case uidFunc:
					// TODO (Anurag): See if is is possible to support uid(1,2,3) when
					// uid is nested inside a function like @filter(uid_in(predicate, uid()))
					if len(nestedFunc.NeedsVar) != 1 {
						return nil,
							itemInFunc.Errorf("Nested uid fn expects 1 uid variable, got %v", len(nestedFunc.NeedsVar))
					}
					if len(nestedFunc.UID) != 0 {
						return nil,
							itemInFunc.Errorf("Nested uid fn expects only uid variable, got UID")
					}
					function.NeedsVar = append(function.NeedsVar, nestedFunc.NeedsVar...)
					function.NeedsVar[0].Typ = UidVar
					function.Args = append(function.Args, Arg{Value: nestedFunc.NeedsVar[0].Name})
				default:
					return nil, itemInFunc.Errorf("Only val/count/len/uid allowed as function "+
						"within another. Got: %s", nestedFunc.Name)
				}
				expectArg = false
				continue
			case itemAt:
				if attrItemsAgo != 1 {
					return nil, itemInFunc.Errorf("Invalid usage of '@' in function " +
						"argument, must only appear immediately after attr.")
				}
				expectLang = true
				continue
			case itemMathOp:
				val = itemInFunc.Val
				it.Next()
				itemInFunc = it.Item()
			case itemDollar:
				if isDollar {
					return nil, itemInFunc.Errorf("Invalid use of $ in func args")
				}
				isDollar = true
				continue
			case itemRegex:
				ra, err := parseRegexArgs(itemInFunc.Val)
				if err != nil {
					return nil, err
				}
				function.Args = append(function.Args, Arg{Value: ra.expr}, Arg{Value: ra.flags})
				expectArg = false
				continue
				// Lets reassemble the geo tokens.
			case itemLeftSquare:
				var err error
				switch {
				case isGeoFunc(function.Name):
					err = parseGeoArgs(it, function)

				case IsInequalityFn(function.Name):
					err = parseFuncArgs(it, function)

				case function.Name == "uid_in" || function.Name == "similar_to":
					err = parseFuncArgs(it, function)

				default:
					err = itemInFunc.Errorf("Unexpected character [ while parsing request.")
				}
				if err != nil {
					return nil, err
				}
				expectArg = false
				continue
			case itemRightSquare:
				if _, err := it.Peek(1); err != nil {
					return nil,
						itemInFunc.Errorf("Unexpected EOF while parsing args")
				}
				expectArg = false
				continue
			default:
				if itemInFunc.Typ != itemName {
					return nil, itemInFunc.Errorf("Expected arg after func [%s], but got item %v",
						function.Name, itemInFunc)
				}
			}

			item, ok := it.PeekOne()
			if !ok {
				return nil, item.Errorf("Unexpected item: %v", item)
			}
			// Part of function continue
			if item.Typ == itemLeftRound {
				continue
			}

			if !expectArg && !expectLang {
				return nil, itemInFunc.Errorf("Expected comma or language but got: %s",
					itemInFunc.Val)
			}

			vname := collectName(it, itemInFunc.Val)
			// TODO - Move this to a function.
			v := strings.Trim(vname, " \t")
			var err error
			v, err = unquoteIfQuoted(v)
			if err != nil {
				return nil, err
			}
			val += v

			if isDollar {
				val = "$" + val
				isDollar = false
				if function.Name == uidFunc && gq != nil {
					if len(gq.Args["id"]) > 0 {
						return nil, itemInFunc.Errorf("Only one DQL variable " +
							"allowed inside uid function.")
					}
					gq.Args["id"] = val
				} else {
					function.Args = append(function.Args, Arg{Value: val, IsDQLVar: true})
				}
				expectArg = false
				continue
			}

			// Unlike other functions, uid function has no attribute, everything is args.
			switch {
			case len(function.Attr) == 0 && function.Name != uidFunc &&
				function.Name != typFunc:

				if strings.ContainsRune(itemInFunc.Val, '"') {
					return nil, itemInFunc.Errorf("Attribute in function"+
						" must not be quoted with \": %s", itemInFunc.Val)
				}
				if function.Name == uidInFunc && item.Typ == itemRightRound {
					return nil, itemInFunc.Errorf("uid_in function expects an argument, got none")
				}
				function.Attr = val
				attrItemsAgo = 0
			case expectLang:
				if val == "*" {
					return nil, errors.Errorf(
						"The * symbol cannot be used as a valid language inside functions")
				}
				function.Lang = val
				expectLang = false
			case function.Name != uidFunc:
				// For UID function. we set g.UID
				function.Args = append(function.Args, Arg{Value: val})
			}

			if function.Name == "var" {
				return nil, itemInFunc.Errorf("Unexpected var(). Maybe you want to try using uid()")
			}

			expectArg = false
			switch function.Name {
			case valueFunc:
				// E.g. @filter(gt(val(a), 10))
				function.NeedsVar = append(function.NeedsVar, VarContext{
					Name: val,
					Typ:  ValueVar,
				})
			case lenFunc:
				// E.g. @filter(gt(len(a), 10))
				// TODO(Aman): type could be ValueVar too!
				function.NeedsVar = append(function.NeedsVar, VarContext{
					Name: val,
					Typ:  UidVar,
				})
			case uidFunc:
				// uid function could take variables as well as actual uids.
				// If we can parse the value that means its an uid otherwise a variable.
				uid, err := strconv.ParseUint(val, 0, 64)
				switch e := err.(type) {
				case nil:
					// It could be uid function at root.
					if gq != nil {
						gq.UID = append(gq.UID, uid)
						// Or uid function in filter.
					} else {
						function.UID = append(function.UID, uid)
					}
					continue
				case *strconv.NumError:
					if e.Err == strconv.ErrRange {
						return nil, itemInFunc.Errorf("The uid value %q is too large.", val)
					}
				}
				// E.g. @filter(uid(a, b, c))
				function.NeedsVar = append(function.NeedsVar, VarContext{
					Name: val,
					Typ:  UidVar,
				})
			}
		}
	}

	if function.Name != uidFunc && function.Name != typFunc && len(function.Attr) == 0 {
		return nil, it.Errorf("Got empty attr for function: [%s]", function.Name)
	}

	if function.Name == typFunc && len(function.Args) != 1 {
		return nil, it.Errorf("type function only supports one argument. Got: %v", function.Args)
	}

	return function, nil
}

type facetRes struct {
	f           *pb.FacetParams
	ft          *FilterTree
	vmap        map[string]string
	facetsOrder []*FacetOrder
}

func parseFacets(it *lex.ItemIterator) (res facetRes, err error) {
	res, ok, err := tryParseFacetList(it)
	if err != nil || ok {
		return res, err
	}

	filterTree, err := parseFilter(it)
	res.ft = filterTree
	return res, err
}

type facetItem struct {
	name      string
	alias     string
	varName   string
	ordered   bool
	orderdesc bool
}

// If err != nil, an error happened, abandon parsing.  If err == nil && parseOk == false, the
// attempt to parse failed, no data was consumed, and there might be a valid alternate parse with a
// different function.
func tryParseFacetItem(it *lex.ItemIterator) (res facetItem, parseOk bool, err error) {
	// We parse this:
	// [{orderdesc|orderasc|alias}:] [varname as] name

	savePos := it.Save()
	defer func() {
		if err == nil && !parseOk {
			it.Restore(savePos)
		}
	}()

	item, ok := tryParseItemType(it, itemName)
	if !ok {
		return res, false, nil
	}

	isOrderasc := item.Val == "orderasc"
	if _, ok := tryParseItemType(it, itemColon); ok {
		if isOrderasc || item.Val == "orderdesc" {
			res.ordered = true
			res.orderdesc = !isOrderasc
		} else {
			res.alias = item.Val
		}

		// Step past colon.
		item, ok = tryParseItemType(it, itemName)
		if !ok {
			return res, false, item.Errorf("Expected name after colon")
		}
	}

	// We've possibly set ordered, orderdesc, alias and now we have consumed another item,
	// which is a name.
	name1 := item.Val

	// Now try to consume "as".
	if !trySkipItemVal(it, "as") {
		name1 = collectName(it, name1)
		res.name = name1
		return res, true, nil
	}
	item, ok = tryParseItemType(it, itemName)
	if !ok {
		return res, false, item.Errorf("Expected name in facet list")
	}

	res.name = collectName(it, item.Val)
	res.varName = name1
	return res, true, nil
}

// If err != nil, an error happened, abandon parsing.  If err == nil && parseOk == false, the
// attempt to parse failed, but there might be a valid alternate parse with a different function,
// such as parseFilter.
func tryParseFacetList(it *lex.ItemIterator) (res facetRes, parseOk bool, err error) {
	savePos := it.Save()
	defer func() {
		if err == nil && !parseOk {
			it.Restore(savePos)
		}
	}()

	// Skip past '('
	if _, ok := tryParseItemType(it, itemLeftRound); !ok {
		it.Restore(savePos)
		var facets pb.FacetParams
		facets.AllKeys = true
		res.f = &facets
		res.vmap = make(map[string]string)
		return res, true, nil
	}

	facetVar := make(map[string]string)
	var facets pb.FacetParams
	var facetsOrder []*FacetOrder

	if _, ok := tryParseItemType(it, itemRightRound); ok {
		// @facets() just parses to an empty set of facets.
		res.f, res.vmap, res.facetsOrder = &facets, facetVar, facetsOrder
		return res, true, nil
	}

	facetsOrderKeys := make(map[string]struct{})
	for {
		// We've just consumed a leftRound or a comma.

		// Parse a facet item.
		// copy the iterator first to facetItemIt so that it corresponds to the parsed facetItem
		// facetItemIt is used later for reporting errors with line and column numbers
		facetItemIt := it
		facetItem, ok, err := tryParseFacetItem(it)
		if !ok || err != nil {
			return res, ok, err
		}

		// Combine the facetitem with our result.
		{
			if facetItem.varName != "" {
				if _, has := facetVar[facetItem.name]; has {
					return res, false, facetItemIt.Errorf("Duplicate variable mappings for facet %v",
						facetItem.name)
				}
				facetVar[facetItem.name] = facetItem.varName
			}
			facets.Param = append(facets.Param, &pb.FacetParam{
				Key:   facetItem.name,
				Alias: facetItem.alias,
			})
			if facetItem.ordered {
				if _, ok := facetsOrderKeys[facetItem.name]; ok {
					return res, false,
						it.Errorf("Sorting by facet: [%s] can only be done once", facetItem.name)
				}
				facetsOrderKeys[facetItem.name] = struct{}{}
				facetsOrder = append(facetsOrder,
					&FacetOrder{Key: facetItem.name, Desc: facetItem.orderdesc})
			}
		}

		// Now what?  Either close-paren or a comma.
		if _, ok := tryParseItemType(it, itemRightRound); ok {
			sort.Slice(facets.Param, func(i, j int) bool {
				return facets.Param[i].Key < facets.Param[j].Key
			})
			// deduplicate facets
			out := facets.Param[:0]
			flen := len(facets.Param)
			for i := 1; i < flen; i++ {
				if facets.Param[i-1].Key == facets.Param[i].Key {
					continue
				}
				out = append(out, facets.Param[i-1])
			}
			out = append(out, facets.Param[flen-1])
			facets.Param = out
			res.f, res.vmap, res.facetsOrder = &facets, facetVar, facetsOrder
			return res, true, nil
		}
		if item, ok := tryParseItemType(it, itemComma); !ok {
			if len(facets.Param) < 2 {
				// We have only consumed ``'@facets' '(' <facetItem>`, which means parseFilter might
				// succeed. Return no-parse, no-error.
				return res, false, nil
			}
			// We've consumed `'@facets' '(' <facetItem> ',' <facetItem>`, so this is definitely
			// not a filter.  Return an error.
			return res, false, item.Errorf(
				"Expected ',' or ')' in facet list: %s", item.Val)
		}
	}
}

// parseCascade parses the cascade directive.
// Two formats:
//  1. @cascade
//  2. @cascade(pred1, pred2, ...)
func parseCascade(it *lex.ItemIterator, gq *GraphQuery) error {
	item := it.Item()
	items, err := it.Peek(1)
	if err != nil {
		return item.Errorf("Unable to peek lexer after cascade")
	}

	// check if it is without any args:
	// 1. @cascade {
	// 2. @cascade }
	// 3. @cascade @
	// 4. @cascade\n someOtherPred
	if items[0].Typ == itemLeftCurl || items[0].Typ == itemRightCurl || items[0].
		Typ == itemAt || items[0].Typ == itemName {
		// __all__ implies @cascade i.e.  implies values for all the children are mandatory.
		gq.Cascade = append(gq.Cascade, "__all__")
		return nil
	}

	count := 0
	expectArg := true
	it.Next()
	item = it.Item()
	if item.Typ != itemLeftRound {
		return item.Errorf("Expected a left round after cascade, got: %s", item.String())
	}

loop:
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case itemRightRound:
			break loop
		case itemComma:
			if expectArg {
				return item.Errorf("Expected a predicate but got comma")
			}
			expectArg = true
		case itemName:
			if !expectArg {
				return item.Errorf("Expected a comma or right round but got: %v", item.Val)
			}
			gq.Cascade = append(gq.Cascade, collectName(it, item.Val))
			count++
			expectArg = false
		default:
			return item.Errorf("Unexpected item while parsing: %v", item.Val)
		}
	}
	if expectArg {
		// use the initial item to report error line and column numbers
		return item.Errorf("Unnecessary comma in cascade()")
	}
	if count == 0 {
		return item.Errorf("At least one predicate required in parameterized cascade()")
	}
	return nil
}

// parseGroupby parses the groupby directive.
func parseGroupby(it *lex.ItemIterator, gq *GraphQuery) error {
	count := 0
	expectArg := true
	it.Next()
	item := it.Item()
	alias := ""
	if item.Typ != itemLeftRound {
		return item.Errorf("Expected a left round after groupby")
	}

loop:
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case itemRightRound:
			break loop
		case itemComma:
			if expectArg {
				return item.Errorf("Expected a predicate but got comma")
			}
			expectArg = true
		case itemName:
			if !expectArg {
				return item.Errorf("Expected a comma or right round but got: %v", item.Val)
			}

			val := collectName(it, item.Val)
			peekIt, err := it.Peek(1)
			if err != nil {
				return err
			}
			if peekIt[0].Typ == itemColon {
				if alias != "" {
					return item.Errorf("Expected predicate after %s:", alias)
				}
				if validKey(val) {
					return item.Errorf("Can't use keyword %s as alias in groupby", val)
				}
				alias = val
				it.Next() // Consume the itemColon
				continue
			}

			var langs []string
			items, err := it.Peek(1)
			if err == nil && items[0].Typ == itemAt {
				it.Next() // consume '@'
				it.Next() // move forward
				langs, err = parseLanguageList(it)
				if err != nil {
					return err
				}
			}
			attrLang := GroupByAttr{
				Attr:  val,
				Alias: alias,
				Langs: langs,
			}
			alias = ""
			gq.GroupbyAttrs = append(gq.GroupbyAttrs, attrLang)
			count++
			expectArg = false
		}
	}
	if expectArg {
		// use the initial item to report error line and column numbers
		return item.Errorf("Unnecessary comma in groupby()")
	}
	if count == 0 {
		return item.Errorf("Expected atleast one attribute in groupby")
	}
	return nil
}

// parseFilter parses the filter directive to produce a QueryFilter / parse tree.
func parseFilter(it *lex.ItemIterator) (*FilterTree, error) {
	it.Next()
	item := it.Item()
	if item.Typ != itemLeftRound {
		return nil, item.Errorf("Expected ( after filter directive")
	}

	// opStack is used to collect the operators in right order.
	opStack := new(filterTreeStack)
	opStack.push(&FilterTree{Op: "("}) // Push ( onto operator stack.
	// valueStack is used to collect the values.
	valueStack := new(filterTreeStack)

loop:
	for it.Next() {
		item := it.Item()
		lval := strings.ToLower(item.Val)
		switch {
		case lval == "and" || lval == "or" || lval == "not": // Handle operators.
			op := lval
			opPred := filterOpPrecedence[op]
			x.AssertTruef(opPred > 0, "Expected opPred > 0: %d", opPred)
			// Evaluate the stack until we see an operator with strictly lower pred.
			for !opStack.empty() {
				topOp := opStack.peek()
				if filterOpPrecedence[topOp.Op] < opPred {
					break
				}
				err := evalStack(opStack, valueStack)
				if err != nil {
					return nil, err
				}
			}
			opStack.push(&FilterTree{Op: op}) // Push current operator.
		case item.Typ == itemName: // Value.
			it.Prev()
			f, err := parseFunction(it, nil)
			if err != nil {
				return nil, err
			}
			leaf := &FilterTree{Func: f}
			valueStack.push(leaf)
		case item.Typ == itemLeftRound: // Just push to op stack.
			opStack.push(&FilterTree{Op: "("})

		case item.Typ == itemRightRound: // Pop op stack until we see a (.
			for !opStack.empty() {
				topOp := opStack.peek()
				if topOp.Op == "(" {
					break
				}
				err := evalStack(opStack, valueStack)
				if err != nil {
					return nil, err
				}
			}
			_, err := opStack.pop() // Pop away the (.
			if err != nil {
				return nil, item.Errorf("Invalid filter statement")
			}
			if opStack.empty() {
				// The parentheses are balanced out. Let's break.
				break loop
			}
		default:
			return nil, item.Errorf("Unexpected item while parsing @filter: %v", item)
		}
	}

	// For filters, we start with ( and end with ). We expect to break out of loop
	// when the parentheses balance off, and at that point, opStack should be empty.
	// For other applications, typically after all items are
	// consumed, we will run a loop like "while opStack is nonempty, evalStack".
	// This is not needed here.
	if !opStack.empty() {
		return nil, item.Errorf("Unbalanced parentheses in @filter statement")
	}

	if valueStack.empty() {
		// This happens when we have @filter(). We can either return an error or
		// ignore. Currently, let's just ignore and pretend there is no filter.
		return nil, nil
	}

	if valueStack.size() != 1 {
		return nil, item.Errorf("Expected one item in value stack, but got %d",
			valueStack.size())
	}
	return valueStack.pop()
}

// Parses ID list. Only used for DQL variables.
// TODO - Maybe get rid of this by lexing individual IDs.
func parseID(val string) ([]uint64, error) {
	val = x.WhiteSpace.Replace(val)
	if val == "" {
		return nil, errors.Errorf("ID can't be empty")
	}
	var uids []uint64
	if val[0] != '[' {
		uid, err := strconv.ParseUint(val, 0, 64)
		if err != nil {
			return nil, err
		}
		uids = append(uids, uid)
		return uids, nil
	}

	if val[len(val)-1] != ']' {
		return nil, errors.Errorf("Invalid id list at root. Got: %+v", val)
	}
	var buf bytes.Buffer
	for _, c := range val[1:] {
		if c == ',' || c == ']' {
			if buf.Len() == 0 {
				continue
			}
			uid, err := strconv.ParseUint(buf.String(), 0, 64)
			if err != nil {
				return nil, err
			}
			uids = append(uids, uid)
			buf.Reset()
			continue
		}
		if c == '[' || c == ')' {
			return nil, errors.Errorf("Invalid id list at root. Got: %+v", val)
		}
		if _, err := buf.WriteRune(c); err != nil {
			return nil, err
		}
	}
	return uids, nil
}

func parseVarList(it *lex.ItemIterator, gq *GraphQuery) (int, error) {
	count := 0
	expectArg := true
	it.Next()
	item := it.Item()
	if item.Typ != itemLeftRound {
		return count, item.Errorf("Expected a left round after var")
	}

loop:
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case itemRightRound:
			break loop
		case itemComma:
			if expectArg {
				return count, item.Errorf("Expected a variable but got comma")
			}
			expectArg = true
		case itemName:
			if !expectArg {
				return count, item.Errorf("Expected a variable but got %s", item.Val)
			}
			count++
			gq.NeedsVar = append(gq.NeedsVar, VarContext{
				Name: item.Val,
				Typ:  UidVar,
			})
			expectArg = false
		}
	}
	if expectArg {
		return count, item.Errorf("Unnecessary comma in val()")
	}
	return count, nil
}

func parseTypeList(it *lex.ItemIterator, gq *GraphQuery) error {
	typeList := it.Item().Val
	expectArg := false
loop:
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case itemRightRound:
			it.Prev()
			break loop
		case itemComma:
			if expectArg {
				return item.Errorf("Expected a variable but got comma")
			}
			expectArg = true
		case itemName:
			if !expectArg {
				return item.Errorf("Expected a variable but got %s", item.Val)
			}
			typeList = fmt.Sprintf("%s,%s", typeList, item.Val)
			expectArg = false
		default:
			return item.Errorf("Unexpected token %s when reading a type list", item.Val)
		}
	}
	if expectArg {
		return it.Item().Errorf("Unnecessary comma in val()")
	}
	gq.Expand = typeList
	return nil
}

func parseDirective(it *lex.ItemIterator, curp *GraphQuery) error {
	valid := true
	it.Prev()
	item := it.Item()
	if item.Typ == itemLeftCurl {
		// Ideally we should check that curp was created at current depth.
		valid = false
	}
	it.Next()

	isExpand := false
	if curp != nil && len(curp.Expand) > 0 {
		isExpand = true
	}
	// No directive is allowed on pb.subgraph like expand all (except type filters),
	// value variables, etc.
	if !valid || curp == nil || (curp.IsInternal && !isExpand) {
		return item.Errorf("Invalid use of directive.")
	}

	it.Next()
	item = it.Item()
	peek, err := it.Peek(1)
	if err != nil || item.Typ != itemName {
		return item.Errorf("Expected directive or language list")
	}

	if isExpand && item.Val != "filter" {
		return item.Errorf(errExpandType)
	}

	switch {
	case item.Val == "facets": // because @facets can come w/t '()'
		res, err := parseFacets(it)
		if err != nil {
			return err
		}
		switch {
		case res.f != nil:
			curp.FacetVar = res.vmap
			curp.FacetsOrder = res.facetsOrder
			if curp.Facets != nil {
				return item.Errorf("Only one facets allowed")
			}
			curp.Facets = res.f
		case res.ft != nil:
			if curp.FacetsFilter != nil {
				return item.Errorf("Only one facets filter allowed")
			}
			if res.ft.hasVars() {
				return item.Errorf(
					"variables are not allowed in facets filter.")
			}
			curp.FacetsFilter = res.ft
		default:
			return item.Errorf("Facets parsing failed.")
		}
	case item.Val == "cascade":
		if err := parseCascade(it, curp); err != nil {
			return err
		}
	case item.Val == "normalize":
		curp.Normalize = true
	case peek[0].Typ == itemLeftRound:
		// this is directive
		switch item.Val {
		case "filter":
			if curp.Filter != nil {
				return item.Errorf("Use AND, OR and round brackets instead" +
					" of multiple filter directives.")
			}
			filter, err := parseFilter(it)
			if err != nil {
				return err
			}
			if isExpand && filter != nil && filter.Func != nil && filter.Func.Name != "type" {
				return item.Errorf(errExpandType)
			}
			curp.Filter = filter
		case "groupby":
			if curp.IsGroupby {
				return item.Errorf("Only one group by directive allowed.")
			}
			curp.IsGroupby = true
			if err := parseGroupby(it, curp); err != nil {
				return err
			}
		default:
			return item.Errorf("Unknown directive [%s]", item.Val)
		}
	case len(curp.Attr) > 0 && len(curp.Langs) == 0:
		// this is language list
		if curp.Langs, err = parseLanguageList(it); err != nil {
			return err
		}
		if len(curp.Langs) == 0 {
			return item.Errorf("Expected at least 1 language in list for %s", curp.Attr)
		}
	default:
		return item.Errorf("Expected directive or language list, got @%s", item.Val)
	}
	return nil
}

func parseLanguageList(it *lex.ItemIterator) ([]string, error) {
	item := it.Item()
	var langs []string
	for ; item.Typ == itemName || item.Typ == itemPeriod || item.Typ == itemStar; item = it.Item() {
		langs = append(langs, item.Val)
		it.Next()
		if it.Item().Typ == itemColon {
			it.Next()
		} else {
			break
		}
	}
	if it.Item().Typ == itemPeriod {
		peekIt, err := it.Peek(1)
		if err != nil {
			return nil, err
		}
		if peekIt[0].Typ == itemPeriod {
			return nil, it.Errorf("Expected only one dot(.) while parsing language list.")
		}
	}
	it.Prev()

	for _, lang := range langs {
		if lang == string(star) && len(langs) > 1 {
			return nil, errors.Errorf(
				"If * is used, no other languages are allowed in the language list. Found %v",
				langs)
		}
	}

	return langs, nil
}

func validKeyAtRoot(k string) bool {
	switch k {
	case "func", "orderasc", "orderdesc", "first", "offset", "after":
		return true
	case "from", "to", "numpaths", "minweight", "maxweight", "maxfrontiersize":
		// Specific to shortest path
		return true
	case "depth":
		return true
	}
	return false
}

// Check for validity of key at non-root nodes.
func validKey(k string) bool {
	switch k {
	case "orderasc", "orderdesc", "first", "offset", "after":
		return true
	}
	return false
}

func attrAndLang(attrData string) (attr string, langs []string) {
	idx := strings.Index(attrData, "@")
	if idx < 0 {
		return attrData, langs
	}
	attr = attrData[:idx]
	langs = strings.Split(attrData[idx+1:], ":")
	return
}

func isEmpty(gq *GraphQuery) bool {
	return gq.Func == nil && len(gq.NeedsVar) == 0 && len(gq.Args) == 0 &&
		gq.ShortestPathArgs.From == nil && gq.ShortestPathArgs.To == nil
}

// getRoot gets the root graph query object after parsing the args.
func getRoot(it *lex.ItemIterator) (gq *GraphQuery, rerr error) {
	gq = &GraphQuery{
		Args: make(map[string]string),
	}
	if !it.Next() {
		return nil, it.Errorf("Invalid query")
	}
	item := it.Item()
	if item.Typ != itemName {
		return nil, item.Errorf("Expected some name. Got: %v", item)
	}

	peekIt, err := it.Peek(1)
	if err != nil {
		return nil, it.Errorf("Invalid Query")
	}
	if peekIt[0].Typ == itemName && strings.ToLower(peekIt[0].Val) == "as" {
		gq.Var = item.Val
		it.Next() // Consume the "AS".
		it.Next()
		item = it.Item()
	}

	gq.Alias = item.Val
	if !it.Next() {
		return nil, item.Errorf("Invalid query")
	}
	item = it.Item()
	if item.Typ != itemLeftRound {
		return nil, item.Errorf("Expected Left round brackets. Got: %v", item)
	}

	expectArg := true
	order := make(map[string]bool)
	// Parse in KV fashion. Depending on the value of key, decide the path.
loop:
	for it.Next() {
		var key string
		// Get key.
		item := it.Item()
		switch item.Typ {
		case itemName:
			if !expectArg {
				return nil, item.Errorf("Not expecting argument. Got: %v", item)
			}
			key = item.Val
			expectArg = false
		case itemRightRound:
			if isEmpty(gq) {
				// Used to do aggregation at root which would be fetched in another block.
				gq.IsEmpty = true
			}
			break loop
		case itemComma:
			if expectArg {
				return nil, item.Errorf("Expected Argument but got comma.")
			}
			expectArg = true
			continue
		default:
			return nil, item.Errorf("Expecting argument name. Got: %v", item)
		}

		if !validKeyAtRoot(key) {
			return nil, item.Errorf("Got invalid keyword: %s at root", key)
		}

		if !it.Next() {
			return nil, item.Errorf("Invalid query")
		}
		item = it.Item()
		if item.Typ != itemColon {
			return nil, item.Errorf("Expecting a colon. Got: %v", item)
		}

		switch key {
		case "func":
			// Store the generator function.
			if gq.Func != nil {
				return gq, item.Errorf("Only one function allowed at root")
			}
			gen, err := parseFunction(it, gq)
			if err != nil {
				return gq, err
			}
			if !validFuncName(gen.Name) {
				return nil, item.Errorf("Function name: %s is not valid.", gen.Name)
			}
			gq.Func = gen
			gq.NeedsVar = append(gq.NeedsVar, gen.NeedsVar...)
		case "from", "to":
			if gq.Alias != "shortest" {
				return gq, item.Errorf("from/to only allowed for shortest path queries")
			}

			fn := &Function{}
			peekIt, err := it.Peek(1)
			if err != nil {
				return nil, item.Errorf("Invalid query")
			}

			assignShortestPathFn := func(fn *Function, key string) {
				switch key {
				case "from":
					gq.ShortestPathArgs.From = fn
				case "to":
					gq.ShortestPathArgs.To = fn
				}
			}

			if peekIt[0].Val == uidFunc {
				gen, err := parseFunction(it, gq)
				if err != nil {
					return gq, err
				}
				fn.NeedsVar = gen.NeedsVar
				fn.Name = gen.Name
				assignShortestPathFn(fn, key)
				continue
			}

			// This means it's not a uid function, so it has to be an actual uid.
			it.Next()
			item := it.Item()
			val := collectName(it, item.Val)
			uid, err := strconv.ParseUint(val, 0, 64)
			switch e := err.(type) {
			case nil:
				fn.UID = append(fn.UID, uid)
			case *strconv.NumError:
				if e.Err == strconv.ErrRange {
					return nil, item.Errorf("The uid value %q is too large.", val)
				}
				return nil,
					item.Errorf("from/to in shortest path can only accept uid function or an uid."+
						" Got: %s", val)
			}
			assignShortestPathFn(fn, key)

		default:
			var val string
			if !it.Next() {
				return nil, it.Errorf("Invalid query")
			}
			item := it.Item()

			switch item.Typ {
			case itemDollar:
				it.Next()
				item = it.Item()
				if item.Typ == itemName {
					val = fmt.Sprintf("$%s", item.Val)
				} else {
					return nil, item.Errorf("Expecting a variable name. Got: %v", item)
				}
				goto ASSIGN
			case itemMathOp:
				if item.Val != "+" && item.Val != "-" {
					return nil,
						item.Errorf("Only Plus and minus are allowed unary ops. Got: %v",
							item.Val)
				}
				val = item.Val
				it.Next()
				item = it.Item()
			}

			if val == "" && item.Val == valueFunc {
				count, err := parseVarList(it, gq)
				if err != nil {
					return nil, err
				}
				if count != 1 {
					return nil, item.Errorf("Expected only one variable but got: %d", count)
				}
				// Modify the NeedsVar context here.
				gq.NeedsVar[len(gq.NeedsVar)-1].Typ = ValueVar
			} else {
				val = collectName(it, val+item.Val)
				// Get language list, if present
				items, err := it.Peek(1)
				if err == nil && items[0].Typ == itemLeftRound {
					if (key == "orderasc" || key == "orderdesc") && val != valueFunc {
						return nil, it.Errorf("Expected val(). Got %s() with order.", val)
					}
				}
				if err == nil && items[0].Typ == itemAt {
					it.Next() // consume '@'
					it.Next() // move forward
					langs, err := parseLanguageList(it)
					if err != nil {
						return nil, err
					}
					val = val + "@" + strings.Join(langs, ":")
				}

			}

			// TODO - Allow only order by one of variable/predicate for now.
			if val == "" {
				// This should only happen in cases like: orderasc: val(c)
				if len(gq.NeedsVar) == 0 {
					return nil, it.Errorf("unable to get value when parsing key value pairs")
				}
				val = gq.NeedsVar[len(gq.NeedsVar)-1].Name
				// Right now we only allow one sort by a variable
				if len(gq.Order) > 0 && isSortkey(key) {
					return nil, it.Errorf("Multiple sorting only allowed by predicates. "+
						"Got: %+v", val)
				}
			}
			if isSortkey(key) {

				// we dont support variable + predicate sorting
				for _, order := range gq.Order {
					for _, needVar := range gq.NeedsVar {
						if needVar.Name == order.Attr {
							return nil, it.Errorf("Val() is not allowed in multiple sorting."+
								" Got: [%v]", needVar.Name)
						}
					}
				}

				if order[val] {
					return nil, it.Errorf("Sorting by an attribute: [%s] can only be done once", val)
				}
				attr, langs := attrAndLang(val)
				if len(langs) > 1 {
					return nil, it.Errorf("Sorting by an attribute: [%s] "+
						"can only be done on one language", val)
				}
				gq.Order = append(gq.Order,
					&pb.Order{Attr: attr, Desc: key == "orderdesc", Langs: langs})
				order[val] = true
				continue
			}

		ASSIGN:
			if _, ok := gq.Args[key]; ok {
				return gq, it.Errorf("Repeated key %q at root", key)
			}
			gq.Args[key] = val
		}
	}

	return gq, nil
}

func isSortkey(k string) bool {
	return k == "orderasc" || k == "orderdesc"
}

type countType int

const (
	notSeen      countType = iota // default value
	seen                          // when we see count keyword
	seenWithPred                  // when we see a predicate within count.
)

func validateEmptyBlockItem(it *lex.ItemIterator, val string) error {
	savePos := it.Save()
	defer func() {
		it.Restore(savePos)
	}()

	fname := val
	// Could have alias so peek forward to get actual function name.
	skipped := trySkipItemTyp(it, itemColon)
	if skipped {
		item, ok := tryParseItemType(it, itemName)
		if !ok {
			return item.Errorf("Expected name. Got: %s", item.Val)
		}
		fname = item.Val
	}
	ok := trySkipItemTyp(it, itemLeftRound)
	if !ok || (!isMathBlock(fname) && !isAggregator(fname)) {
		return it.Errorf("Only aggregation/math functions allowed inside empty blocks."+
			" Got: %v", fname)
	}
	return nil
}

// godeep constructs the subgraph from the lexed items and a GraphQuery node.
func godeep(it *lex.ItemIterator, gq *GraphQuery) error {
	if gq == nil {
		return it.Errorf("Bad nesting of predicates or functions")
	}
	var count countType
	var alias, varName string
	curp := gq // Used to track current node, for nesting.
	for it.Next() {
		item := it.Item()
		switch item.Typ {
		case lex.ItemEOF:
			return nil
		case itemRightCurl:
			return nil
		case itemPeriod:
			// looking for ...
			dots := 1
			for range 2 {
				if it.Next() && it.Item().Typ == itemPeriod {
					dots++
				}
			}
			if dots == 3 {
				it.Next()
				item = it.Item()
				if item.Typ == itemName {
					// item.Val is expected to start with "..." and to have len >3.
					gq.Children = append(gq.Children, &GraphQuery{fragment: item.Val})
					// Unlike itemName, there is no nesting, so do not change "curp".
				}
			} else {
				return item.Errorf("Expected 3 periods (\"...\"), got %d.", dots)
			}
		case itemName:
			peekIt, err := it.Peek(1)
			if err != nil {
				return item.Errorf("Invalid query")
			}
			if peekIt[0].Typ == itemName && strings.ToLower(peekIt[0].Val) == "as" {
				varName = item.Val
				it.Next() // "As" was checked before.
				continue
			}

			val := collectName(it, item.Val)
			valLower := strings.ToLower(val)

			peekIt, err = it.Peek(1)
			if err != nil {
				return err
			}
			if peekIt[0].Typ == itemColon {
				if len(alias) > 0 {
					return item.Errorf("Invalid colon after alias declaration")
				}
				alias = val
				it.Next() // Consume the itemcolon
				continue
			}

			if gq.IsGroupby && (!isAggregator(val) && val != "count" && count != seen) {
				// Only aggregator or count allowed inside the groupby block.
				return it.Errorf("Only aggregator/count "+
					"functions allowed inside @groupby. Got: %v", val)
			}

			if gq.IsEmpty {
				if err := validateEmptyBlockItem(it, valLower); err != nil {
					return err
				}
			}

			switch {
			case valLower == "checkpwd":
				child := &GraphQuery{
					Args:  make(map[string]string),
					Var:   varName,
					Alias: alias,
				}
				varName, alias = "", ""
				it.Prev()
				if child.Func, err = parseFunction(it, gq); err != nil {
					return err
				}
				child.Func.Args = append(child.Func.Args, Arg{Value: child.Func.Attr})
				child.Attr = child.Func.Attr
				gq.Children = append(gq.Children, child)
				curp = nil
				continue
			case isAggregator(valLower):
				child := &GraphQuery{
					Attr:       valueFunc,
					Args:       make(map[string]string),
					Var:        varName,
					IsInternal: true,
					Alias:      alias,
				}
				varName, alias = "", ""
				it.Next()
				if it.Item().Typ != itemLeftRound {
					it.Prev()
					goto Fall
				}
				it.Next()
				if gq.IsGroupby {
					item = it.Item()
					attr := collectName(it, item.Val)
					// Get language list, if present
					items, err := it.Peek(1)
					if err == nil && items[0].Typ == itemAt {
						it.Next() // consume '@'
						it.Next() // move forward
						if child.Langs, err = parseLanguageList(it); err != nil {
							return err
						}
					}
					child.Attr = attr
					child.IsInternal = false
				} else {
					if it.Item().Val != valueFunc {
						return it.Errorf("Only variables allowed in aggregate functions. Got: %v",
							it.Item().Val)
					}
					count, err := parseVarList(it, child)
					if err != nil {
						return err
					}
					if count != 1 {
						return it.Errorf("Expected one variable inside val() of"+
							" aggregator but got %v", count)
					}
					child.NeedsVar[len(child.NeedsVar)-1].Typ = ValueVar
				}
				child.Func = &Function{
					Name:     valLower,
					NeedsVar: child.NeedsVar,
				}
				it.Next() // Skip the closing ')'
				gq.Children = append(gq.Children, child)
				curp = nil
				continue
			case isMathBlock(valLower):
				if varName == "" && alias == "" {
					return it.Errorf("Function math should be used with a variable or have an alias")
				}
				mathTree, again, err := parseMathFunc(gq, it, false)
				if err != nil {
					return err
				}
				if again {
					return it.Errorf("Comma encountered in math() at unexpected place.")
				}
				child := &GraphQuery{
					Attr:       val,
					Alias:      alias,
					Args:       make(map[string]string),
					Var:        varName,
					MathExp:    mathTree,
					IsInternal: true,
				}
				// TODO - See that if we can instead initialize this at the top.
				varName, alias = "", ""
				gq.Children = append(gq.Children, child)
				curp = nil
				continue
			case isExpandFunc(valLower):
				if varName != "" {
					return it.Errorf("expand() cannot be used with a variable: %s", val)
				}
				if alias != "" {
					return it.Errorf("expand() cannot have an alias")
				}
				it.Next() // Consume the '('
				if it.Item().Typ != itemLeftRound {
					return it.Errorf("Invalid use of expand()")
				}
				it.Next()
				item := it.Item()
				child := &GraphQuery{
					Attr:       val,
					Args:       make(map[string]string),
					IsInternal: true,
				}
				switch item.Val {
				case valueFunc:
					count, err := parseVarList(it, child)
					if err != nil {
						return err
					}
					if count != 1 {
						return item.Errorf("Invalid use of expand(). Exactly one variable expected.")
					}
					child.NeedsVar[len(child.NeedsVar)-1].Typ = ListVar
					child.Expand = child.NeedsVar[len(child.NeedsVar)-1].Name
				case "_all_":
					child.Expand = "_all_"
				case "_forward_":
					return item.Errorf("Argument _forward_ has been deprecated")
				case "_reverse_":
					return item.Errorf("Argument _reverse_ has been deprecated")
				default:
					if err := parseTypeList(it, child); err != nil {
						return err
					}
				}
				it.Next() // Consume ')'
				gq.Children = append(gq.Children, child)
				// Note: curp is not set to nil. So it can have children, filters, etc.
				curp = child
				continue
			case valLower == "count":
				if count != notSeen {
					return it.Errorf("Invalid mention of function count")
				}
				count = seen
				it.Next()
				item = it.Item()
				if item.Typ != itemLeftRound {
					it.Prev()
					count = notSeen
					goto Fall
				}

				peekIt, err := it.Peek(2)
				if err != nil {
					return err
				}

				switch {
				case peekIt[0].Typ == itemRightRound:
					return it.Errorf("Cannot use count(), please use count(uid)")
				case peekIt[0].Val == uidFunc && peekIt[1].Typ == itemRightRound:
					if gq.IsGroupby {
						// count(uid) case which occurs inside @groupby
						val = uidFunc
						// Skip uid)
						it.Next()
						it.Next()
						goto Fall
					}

					count = notSeen
					child := &GraphQuery{
						Attr:       "uid",
						Alias:      alias,
						Var:        varName,
						IsCount:    true,
						IsInternal: true,
					}
					gq.Children = append(gq.Children, child)
					varName, alias = "", ""

					it.Next()
					it.Next()
				}
				continue
			case valLower == valueFunc:
				if varName != "" {
					return it.Errorf("Cannot assign a variable to val()")
				}
				if count == seen {
					return it.Errorf("Count of a variable is not allowed")
				}
				peekIt, err = it.Peek(1)
				if err != nil {
					return err
				}
				if peekIt[0].Typ != itemLeftRound {
					goto Fall
				}

				child := &GraphQuery{
					Attr:       val,
					Args:       make(map[string]string),
					IsInternal: true,
					Alias:      alias,
				}
				alias = ""
				count, err := parseVarList(it, child)
				if err != nil {
					return err
				}
				if count != 1 {
					return it.Errorf("Invalid use of val(). Exactly one variable expected.")
				}
				// Only value vars can be retrieved.
				child.NeedsVar[len(child.NeedsVar)-1].Typ = ValueVar
				gq.Children = append(gq.Children, child)
				curp = nil
				continue
			case valLower == uidFunc:
				if count == seen {
					return it.Errorf("Count of a variable is not allowed")
				}
				peekIt, err = it.Peek(1)
				if err != nil {
					return err
				}
				if peekIt[0].Typ != itemLeftRound {
					goto Fall
				}
				return it.Errorf("Cannot do uid() of a variable")
			}
		Fall:
			if count == seenWithPred {
				return it.Errorf("Multiple predicates not allowed in single count.")
			}
			child := &GraphQuery{
				Args:    make(map[string]string),
				Attr:    val,
				IsCount: count == seen,
				Var:     varName,
				Alias:   alias,
			}

			if gq.IsCount {
				return it.Errorf("Cannot have children attributes when asking for count.")
			}
			gq.Children = append(gq.Children, child)
			varName, alias = "", ""
			curp = child
			if count == seen {
				count = seenWithPred
			}
		case itemLeftCurl:
			if curp == nil {
				return it.Errorf("Query syntax invalid.")
			}
			if len(curp.Langs) > 0 {
				return it.Errorf("Cannot have children for attr: %s with lang tags: %v", curp.Attr,
					curp.Langs)
			}
			if err := godeep(it, curp); err != nil {
				return err
			}
		case itemLeftRound:
			if curp == nil {
				return it.Errorf("Query syntax invalid.")
			}
			if curp.Attr == "" {
				return it.Errorf("Predicate name cannot be empty.")
			}
			args, err := parseArguments(it, curp)
			if err != nil {
				return err
			}
			// Stores args in GraphQuery, will be used later while retrieving results.
			order := make(map[string]bool)
			for _, p := range args {
				if !validKey(p.Key) {
					return it.Errorf("Got invalid keyword: %s", p.Key)
				}
				if _, ok := curp.Args[p.Key]; ok {
					return it.Errorf("Got repeated key %q at level %q", p.Key, curp.Attr)
				}
				if p.Val == "" {
					return it.Errorf("Got empty argument")
				}
				if p.Key == "orderasc" || p.Key == "orderdesc" {
					if order[p.Val] {
						return it.Errorf("Sorting by an attribute: [%s] "+
							"can only be done once", p.Val)
					}
					attr, langs := attrAndLang(p.Val)
					if len(langs) > 1 {
						return it.Errorf("Sorting by an attribute: [%s] "+
							"can only be done on one language", p.Val)
					}
					curp.Order = append(curp.Order,
						&pb.Order{Attr: attr, Desc: p.Key == "orderdesc", Langs: langs})
					order[p.Val] = true
					continue
				}

				curp.Args[p.Key] = p.Val
			}
		case itemAt:
			err := parseDirective(it, curp)
			if err != nil {
				return err
			}

		case itemRightRound:
			if count != seenWithPred {
				return it.Errorf("Invalid mention of brackets")
			}
			count = notSeen
		}
	}
	return nil
}

func isAggregator(fname string) bool {
	return fname == "min" || fname == "max" || fname == "sum" || fname == "avg"
}

func isExpandFunc(name string) bool {
	return name == "expand"
}

func isMathBlock(name string) bool {
	return name == "math"
}

func isGeoFunc(name string) bool {
	return name == "near" || name == "contains" || name == "within" || name == "intersects"
}

func IsInequalityFn(name string) bool {
	switch name {
	case "eq", "le", "ge", "gt", "lt", "between":
		return true
	}
	return false
}

// Name can have dashes or alphanumeric characters. Lexer lexes them as separate items.
// We put it back together here.
func collectName(it *lex.ItemIterator, val string) string {
	var dashOrName bool // false if expecting dash, true if expecting name
	for {
		items, err := it.Peek(1)
		if err == nil && ((items[0].Typ == itemName && dashOrName) ||
			(items[0].Val == "-" && !dashOrName)) {
			it.Next()
			val += it.Item().Val
			dashOrName = !dashOrName
		} else {
			break
		}
	}
	return val
}

// Steps the parser.
func tryParseItemType(it *lex.ItemIterator, typ lex.ItemType) (lex.Item, bool) {
	item, ok := it.PeekOne()
	if !ok || item.Typ != typ {
		return item, false
	}
	it.Next()
	return item, true
}

func trySkipItemVal(it *lex.ItemIterator, val string) bool {
	item, ok := it.PeekOne()
	if !ok || item.Val != val {
		return false
	}
	it.Next()
	return true
}

func trySkipItemTyp(it *lex.ItemIterator, typ lex.ItemType) bool {
	item, ok := it.PeekOne()
	if !ok || item.Typ != typ {
		return false
	}
	it.Next()
	return true
}
