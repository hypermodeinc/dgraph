/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/worker"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func TestEncodeMemory(t *testing.T) {
	//	if testing.Short() {
	t.Skip("Skipping TestEncodeMemory")
	//	}
	var wg sync.WaitGroup

	for range runtime.NumCPU() {
		enc := newEncoder()
		n := enc.newNode(0)
		require.NotNil(t, n)
		for i := range 15000 {
			require.NoError(t, enc.AddValue(n, enc.idForAttr(fmt.Sprintf("very long attr name %06d", i)),
				types.ValueForType(types.StringID)))
			enc.AddListChild(n,
				enc.newNode(enc.idForAttr(fmt.Sprintf("another long child %06d", i))))
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				enc.buf.Reset()
				require.NoError(t, enc.encode(n))
			}
		}()
	}

	wg.Wait()
}

func TestNormalizeJSONLimit(t *testing.T) {
	// Set default normalize limit.
	x.Config.Limit = z.NewSuperFlag("normalize-node=10000;").MergeAndCheckDefault(worker.LimitDefaults)

	if testing.Short() {
		t.Skip("Skipping TestNormalizeJSONLimit")
	}

	enc := newEncoder()
	n := enc.newNode(enc.idForAttr("root"))
	require.NotNil(t, n)
	for i := range 1000 {
		require.NoError(t, enc.AddValue(n, enc.idForAttr(fmt.Sprintf("very long attr name %06d", i)),
			types.ValueForType(types.StringID)))
		child1 := enc.newNode(enc.idForAttr("child1"))
		enc.AddListChild(n, child1)
		for j := range 100 {
			require.NoError(t, enc.AddValue(child1, enc.idForAttr(fmt.Sprintf("long child1 attr %06d", j)),
				types.ValueForType(types.StringID)))
		}
		child2 := enc.newNode(enc.idForAttr("child2"))
		enc.AddListChild(n, child2)
		for j := range 100 {
			require.NoError(t, enc.AddValue(child2, enc.idForAttr(fmt.Sprintf("long child2 attr %06d", j)),
				types.ValueForType(types.StringID)))
		}
		child3 := enc.newNode(enc.idForAttr("child3"))
		enc.AddListChild(n, child3)
		for j := range 100 {
			require.NoError(t, enc.AddValue(child3, enc.idForAttr(fmt.Sprintf("long child3 attr %06d", j)),
				types.ValueForType(types.StringID)))
		}
	}
	_, err := enc.normalize(n)
	require.Error(t, err, "Couldn't evaluate @normalize directive - too many results")
}

func BenchmarkJsonMarshal(b *testing.B) {
	inputStrings := [][]string{
		{"largestring", strings.Repeat("a", 1024)},
		{"smallstring", "abcdef"},
		{"specialchars", "<><>^)(*&(%*&%&^$*&%)(*&)^)"},
	}

	var result []byte

	for _, input := range inputStrings {
		b.Run(fmt.Sprintf("STDJsonMarshal-%s", input[0]), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				result, _ = json.Marshal(input[1])
			}
		})

		b.Run(fmt.Sprintf("stringJsonMarshal-%s", input[0]), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				result = stringJsonMarshal(input[1])
			}
		})
	}

	_ = result
}

func TestStringJsonMarshal(t *testing.T) {
	inputs := []string{
		"",
		"0",
		"true",
		"1.909045927350",
		"nil",
		"null",
		"<&>",
		`quoted"str"ing`,
	}

	for _, input := range inputs {
		gm, err := json.Marshal(input)
		require.NoError(t, err)

		sm := stringJsonMarshal(input)

		require.Equal(t, gm, sm)
	}
}

func TestFastJsonNode(t *testing.T) {
	attrId := uint16(20)
	scalarVal := bytes.Repeat([]byte("a"), 160)
	list := true

	enc := newEncoder()
	fj := enc.newNode(attrId)
	require.NoError(t, enc.setScalarVal(fj, scalarVal))
	enc.setList(fj, list)

	require.Equal(t, attrId, enc.getAttr(fj))
	sv, err := enc.getScalarVal(fj)
	require.NoError(t, err)
	require.Equal(t, scalarVal, sv)
	require.Equal(t, list, enc.getList(fj))

	fj2 := enc.newNode(attrId)
	require.NoError(t, enc.setScalarVal(fj2, scalarVal))
	enc.setList(fj2, list)

	sv, err = enc.getScalarVal(fj2)
	require.NoError(t, err)
	require.Equal(t, scalarVal, sv)
	require.Equal(t, list, enc.getList(fj2))

	enc.appendAttrs(fj, fj2)
	require.Equal(t, fj2, enc.children(fj))
}

func BenchmarkFastJsonNodeEmpty(b *testing.B) {
	for i := 0; i < b.N; i++ {
		enc := newEncoder()
		var fj fastJsonNode
		for range 2000000 {
			fj = enc.newNode(0)
		}
		_ = fj
	}
}

var (
	testAttr = "abcdefghijklmnop"
	testVal  = types.Val{Tid: types.DefaultID, Value: []byte(testAttr)}
)

func buildTestTree(b *testing.B, enc *encoder, level, maxlevel int, fj fastJsonNode) {
	if level >= maxlevel {
		return
	}

	// Add only two children for now.
	for range 2 {
		var ch fastJsonNode
		if level == maxlevel-1 {
			val, err := valToBytes(testVal)
			x.Panic(err)

			ch, err = enc.makeScalarNode(enc.idForAttr(testAttr), val, false)
			require.NoError(b, err)
		} else {
			ch := enc.newNode(enc.idForAttr(testAttr))
			buildTestTree(b, enc, level+1, maxlevel, ch)
		}
		enc.appendAttrs(fj, ch)
	}
}

func BenchmarkFastJsonNode2Chilren(b *testing.B) {
	for i := 0; i < b.N; i++ {
		enc := newEncoder()
		root := enc.newNode(enc.idForAttr(testAttr))
		buildTestTree(b, enc, 1, 20, root)
	}
}

func TestChildrenOrder(t *testing.T) {
	enc := newEncoder()
	root := enc.newNode(1)
	root.meta = 0
	for i := 1; i <= 10; i++ {
		n := enc.newNode(1)
		n.meta = uint64(i)
		enc.addChildren(root, n)
	}

	stepMom := enc.newNode(1)
	stepMom.meta = 100
	for i := 11; i <= 20; i++ {
		n := enc.newNode(1)
		n.meta = uint64(i)
		enc.addChildren(stepMom, n)
	}
	enc.addChildren(root, stepMom.child)

	stepDad := enc.newNode(1)
	stepDad.meta = 101
	{
		n := enc.newNode(1)
		n.meta = uint64(21)
		enc.addChildren(stepDad, n)
	}
	enc.addChildren(root, stepDad.child)

	enc.fixOrder(root)
	enc.fixOrder(root) // Another time just to ensure it still works.

	child := root.child
	for i := 1; i <= 21; i++ {
		require.Equal(t, uint64(i), child.meta&^visitedBit)
		child = child.next
	}
	require.Nil(t, child)
}

func TestMarshalTimeJson(t *testing.T) {
	var timesToMarshal = []struct {
		in  time.Time
		out string
	}{
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", -6*60*60)),
			out: "\"2018-05-30T09:30:10-06:00\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 500000000, time.UTC),
			out: "\"2018-05-30T09:30:10.5Z\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", -23*60*60)),
			out: "\"2018-05-30T09:30:10-23:00\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", -24*60*60)),
			out: "\"2018-05-30T09:30:10-24:00\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", 23*60*60)),
			out: "\"2018-05-30T09:30:10+23:00\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", 23*60*60+58*60)),
			out: "\"2018-05-30T09:30:10+23:58\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", 24*60*60)),
			out: "\"2018-05-30T09:30:10+24:00\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", -30*60*60)),
			out: "\"2018-05-30T09:30:10-30:00\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", 30*60*60)),
			out: "\"2018-05-30T09:30:10+30:00\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", 45*60*60)),
			out: "\"2018-05-30T09:30:10+45:00\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", 45*60*60+34*60)),
			out: "\"2018-05-30T09:30:10+45:34\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", 49*60*60+59*60)),
			out: "\"2018-05-30T09:30:10+49:59\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", -99*60*60)),
			out: "\"2018-05-30T09:30:10-99:00\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", 99*60*60)),
			out: "\"2018-05-30T09:30:10+99:00\""},
		{in: time.Date(2018, 5, 30, 9, 30, 10, 0, time.FixedZone("", 100*60*60+23*60)),
			out: "\"2018-05-30T09:30:10+100:23\""},
	}

	for _, tc := range timesToMarshal {
		out, err := marshalTimeJson(tc.in)
		require.NoError(t, err)
		require.Equal(t, tc.out, string(out))
	}
}

func TestMarshalFloat(t *testing.T) {
	var (
		in  = types.Val{Tid: types.FloatID, Value: 0.123456789012345}
		out = "0.123456789012345"
	)
	result, err := valToBytes(in)
	require.NoError(t, err)
	require.Equal(t, out, string(result))
}
