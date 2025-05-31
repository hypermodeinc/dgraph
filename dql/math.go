/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dql

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/hypermodeinc/dgraph/v25/lex"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
)

type mathTreeStack struct{ a []*MathTree }

func (s *mathTreeStack) empty() bool      { return len(s.a) == 0 }
func (s *mathTreeStack) size() int        { return len(s.a) }
func (s *mathTreeStack) push(t *MathTree) { s.a = append(s.a, t) }

func (s *mathTreeStack) popAssert() *MathTree {
	x.AssertTruef(!s.empty(), "Expected a non-empty stack")
	last := s.a[len(s.a)-1]
	s.a = s.a[:len(s.a)-1]
	return last
}

func (s *mathTreeStack) pop() (*MathTree, error) {
	if s.empty() {
		return nil, errors.Errorf("Empty stack")
	}
	last := s.a[len(s.a)-1]
	s.a = s.a[:len(s.a)-1]
	return last, nil
}

func (s *mathTreeStack) peek() *MathTree {
	x.AssertTruef(!s.empty(), "Trying to peek empty stack")
	return s.a[len(s.a)-1]
}

// MathTree represents math operations in tree form for evaluation.
type MathTree struct {
	Fn    string
	Var   string
	Const types.Val // This will always be parsed as a float value
	Val   *types.ShardedMap
	Child []*MathTree
}

func isUnary(f string) bool {
	return f == "exp" || f == "ln" || f == "u-" || f == "sqrt" ||
		f == "floor" || f == "ceil" || f == "since"
}

func isBinaryMath(f string) bool {
	return f == "*" || f == "+" || f == "-" || f == "/" || f == "%"
}

func isTernary(f string) bool {
	return f == "cond"
}

func isZero(f string, rval types.Val) bool {
	switch rval.Tid {
	case types.FloatID:
		g, ok := rval.Value.(float64)
		if !ok {
			return false
		}
		switch f {
		case "floor":
			return g >= 0 && g < 1.0
		case "/", "%", "ceil", "sqrt", "u-":
			return g == 0
		case "ln":
			return g == 1
		}
		return false
	case types.IntID:
		g, ok := rval.Value.(int64)
		if !ok {
			return false
		}
		switch f {
		case "floor", "/", "%", "ceil", "sqrt", "u-":
			return g == 0
		case "ln":
			return g == 1
		}
		return false
	}

	return false
}

func evalMathStack(opStack, valueStack *mathTreeStack) error {
	topOp, err := opStack.pop()
	if err != nil {
		return errors.Errorf("Invalid Math expression")
	}
	switch {
	case isUnary(topOp.Fn):
		// Since "not" is a unary operator, just pop one value.
		topVal, err := valueStack.pop()
		if err != nil {
			return errors.Errorf("Invalid math statement. Expected 1 operands")
		}
		if opStack.size() > 1 {
			peek := opStack.peek().Fn
			if (peek == "/" || peek == "%") && isZero(topOp.Fn, topVal.Const) {
				return errors.Errorf("Division by zero")
			}
		}
		topOp.Child = []*MathTree{topVal}

	case isTernary(topOp.Fn):
		if valueStack.size() < 3 {
			return errors.Errorf("Invalid Math expression. Expected 3 operands")
		}
		topVal1 := valueStack.popAssert()
		topVal2 := valueStack.popAssert()
		topVal3 := valueStack.popAssert()
		topOp.Child = []*MathTree{topVal3, topVal2, topVal1}

	default:
		if valueStack.size() < 2 {
			return errors.Errorf("Invalid Math expression. Expected 2 operands")
		}
		if isZero(topOp.Fn, valueStack.peek().Const) {
			return errors.Errorf("Division by zero.")
		}
		topVal1 := valueStack.popAssert()
		topVal2 := valueStack.popAssert()
		topOp.Child = []*MathTree{topVal2, topVal1}

	}
	// Push the new value (tree) into the valueStack.
	valueStack.push(topOp)
	return nil
}

func isMathFunc(f string) bool {
	// While adding an op, also add it to the corresponding function type.
	return f == "*" || f == "%" || f == "+" || f == "-" || f == "/" ||
		f == "exp" || f == "ln" || f == "cond" ||
		f == "<" || f == ">" || f == ">=" || f == "<=" ||
		f == "==" || f == "!=" ||
		f == "min" || f == "max" || f == "sqrt" ||
		f == "pow" || f == "logbase" || f == "floor" || f == "ceil" ||
		f == "since" || f == "dot"
}

func parseMathFunc(gq *GraphQuery, it *lex.ItemIterator, again bool) (*MathTree, bool, error) {
	if !again {
		it.Next()
		item := it.Item()
		if item.Typ != itemLeftRound {
			return nil, false, errors.Errorf("Expected ( after math")
		}
	}

	// opStack is used to collect the operators in right order.
	opStack := new(mathTreeStack)
	opStack.push(&MathTree{Fn: "("}) // Push ( onto operator stack.
	// valueStack is used to collect the values.
	valueStack := new(mathTreeStack)

loop:
	for it.Next() {
		item := it.Item()
		lval := strings.ToLower(item.Val)
		switch {
		case isMathFunc(lval):
			op := lval
			it.Prev()
			lastItem := it.Item()
			it.Next()
			if op == "-" &&
				(lastItem.Val == "(" || lastItem.Val == "," || isBinaryMath(lastItem.Val)) {
				op = "u-" // This is a unary -
			}
			opPred := mathOpPrecedence[op]
			x.AssertTruef(opPred > 0, "Expected opPred > 0 for %v: %d", op, opPred)
			// Evaluate the stack until we see an operator with strictly lower pred.
			for !opStack.empty() {
				topOp := opStack.peek()
				if mathOpPrecedence[topOp.Fn] < opPred {
					break
				}
				err := evalMathStack(opStack, valueStack)
				if err != nil {
					return nil, false, err
				}
			}
			opStack.push(&MathTree{Fn: op}) // Push current operator.
			peekIt, err := it.Peek(1)
			if err != nil {
				return nil, false, err
			}
			if peekIt[0].Typ == itemLeftRound {
				again := false
				var child *MathTree
				for {
					child, again, err = parseMathFunc(gq, it, again)
					if err != nil {
						return nil, false, err
					}
					valueStack.push(child)
					if !again {
						break
					}
				}
			}
		case item.Typ == itemName: // Value.
			peekIt, err := it.Peek(1)
			if err != nil {
				return nil, false, err
			}
			if peekIt[0].Typ == itemLeftRound {
				again := false
				if !isMathFunc(item.Val) {
					return nil, false, errors.Errorf("Unknown math function: %v", item.Val)
				}
				var child *MathTree
				for {
					child, again, err = parseMathFunc(gq, it, again)
					if err != nil {
						return nil, false, err
					}
					valueStack.push(child)
					if !again {
						break
					}
				}
				continue
			}
			// We will try to parse the constant as an Int first, if that fails we move to float
			child := &MathTree{}
			i, err := strconv.ParseInt(item.Val, 10, 64)
			if err != nil {
				v, err := strconv.ParseFloat(item.Val, 64)
				if err != nil {
					child.Var = item.Val
				} else {
					child.Const = types.Val{
						Tid:   types.FloatID,
						Value: v,
					}
				}
			} else {
				child.Const = types.Val{
					Tid:   types.IntID,
					Value: i,
				}
			}
			valueStack.push(child)
		case item.Typ == itemLeftRound: // Just push to op stack.
			opStack.push(&MathTree{Fn: "("})

		case item.Typ == itemComma:
			for !opStack.empty() {
				topOp := opStack.peek()
				if topOp.Fn == "(" {
					break
				}
				err := evalMathStack(opStack, valueStack)
				if err != nil {
					return nil, false, err
				}
			}
			_, err := opStack.pop() // Pop away the (.
			if err != nil {
				return nil, false, errors.Errorf("Invalid Math expression")
			}
			if !opStack.empty() {
				return nil, false, errors.Errorf("Invalid math expression.")
			}
			if valueStack.size() != 1 {
				return nil, false, errors.Errorf("Expected one item in value stack, but got %d",
					valueStack.size())
			}
			res, err := valueStack.pop()
			if err != nil {
				return nil, false, err
			}
			return res, true, nil
		case item.Typ == itemRightRound: // Pop op stack until we see a (.
			for !opStack.empty() {
				topOp := opStack.peek()
				if topOp.Fn == "(" {
					break
				}
				err := evalMathStack(opStack, valueStack)
				if err != nil {
					return nil, false, err
				}
			}
			_, err := opStack.pop() // Pop away the (.
			if err != nil {
				return nil, false, errors.Errorf("Invalid Math expression")
			}
			if opStack.empty() {
				// The parentheses are balanced out. Let's break.
				break loop
			}
		case item.Typ == itemDollar:
			varName, err := parseVarName(it)
			if err != nil {
				return nil, false, err
			}
			child := &MathTree{}
			child.Var = varName
			valueStack.push(child)
		default:
			return nil, false, errors.Errorf("Unexpected item while parsing math expression: %v",
				item)
		}
	}

	// For math Expressions, we start with ( and end with ). We expect to break out of loop
	// when the parentheses balance off, and at that point, opStack should be empty.
	// For other applications, typically after all items are
	// consumed, we will run a loop like "while opStack is nonempty, evalStack".
	// This is not needed here.
	x.AssertTruef(opStack.empty(), "Op stack should be empty when we exit")

	if valueStack.empty() {
		// This happens when we have math(). We can either return an error or
		// ignore. Currently, let's just ignore and pretend there is no expression.
		return nil, false, errors.Errorf("Empty () not allowed in math block.")
	}

	if valueStack.size() != 1 {
		return nil, false, errors.Errorf("Expected one item in value stack, but got %d",
			valueStack.size())
	}
	res, err := valueStack.pop()
	return res, false, err
}

func (t *MathTree) subs(vmap varMap) error {
	if strings.HasPrefix(t.Var, "$") {
		va, ok := vmap[t.Var]
		if !ok {
			return errors.Errorf("Variable not found in math")
		}
		var err error
		t.Const, err = parseValue(va)
		if err != nil {
			return err
		}
		t.Var = ""
	}
	for _, i := range t.Child {
		if err := i.subs(vmap); err != nil {
			return err
		}
	}
	return nil
}

// debugString converts mathTree to a string. Good for testing, debugging.
// nolint: unused
func (t *MathTree) debugString() string {
	buf := bytes.NewBuffer(make([]byte, 0, 20))
	t.stringHelper(buf)
	return buf.String()
}

// stringHelper does simple DFS to convert MathTree to string.
// nolint: unused
func (t *MathTree) stringHelper(buf *bytes.Buffer) {
	x.AssertTruef(t != nil, "Nil Math tree")
	if t.Var != "" {
		// Leaf node.
		x.Check2(buf.WriteString(t.Var))
		return
	}
	if t.Const.Value != nil {
		// Leaf node.
		var leafStr int
		var err error
		switch t.Const.Tid {
		case types.FloatID:
			leafStr, err = buf.WriteString(strconv.FormatFloat(
				t.Const.Value.(float64), 'E', -1, 64))
		case types.IntID:
			leafStr, err = buf.WriteString(strconv.FormatInt(t.Const.Value.(int64), 10))
		}
		x.Check2(leafStr, err)
		return
	}
	// Non-leaf node.
	x.Check2(buf.WriteRune('('))
	switch t.Fn {
	case "+", "-", "/", "*", "%", "exp", "ln", "cond", "min",
		"sqrt", "max", "<", ">", "<=", ">=", "==", "!=", "u-",
		"logbase", "pow", "dot":
		x.Check2(buf.WriteString(t.Fn))
	default:
		x.Fatalf("Unknown operator: %q", t.Fn)
	}

	for _, c := range t.Child {
		x.Check2(buf.WriteRune(' '))
		c.stringHelper(buf)
	}
	x.Check2(buf.WriteRune(')'))
}
