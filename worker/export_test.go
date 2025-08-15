//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/chunker"
	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/lex"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/types/facets"
	"github.com/hypermodeinc/dgraph/v25/x"
)

const (
	gqlSchema = "type Example { name: String }"
)

var personType = &pb.TypeUpdate{
	TypeName: x.AttrInRootNamespace("Person"),
	Fields: []*pb.SchemaUpdate{
		{
			Predicate: x.AttrInRootNamespace("name"),
		},
		{
			Predicate: x.AttrInRootNamespace("friend"),
		},
		{
			Predicate: x.AttrInRootNamespace("~friend"),
		},
		{
			Predicate: x.AttrInRootNamespace("friend_not_served"),
		},
	},
}

func populateGraphExport(t *testing.T) {
	rdfEdges := []string{
		`<1> <friend> <5> .`,
		`<2> <friend> <5> .`,
		`<3> <friend> <5> .`,
		`<4> <friend> <5> (since=2005-05-02T15:04:05,close=true,` +
			`age=33,game="football",poem="roses are red\nviolets are blue") .`,
		`<1> <name> "pho\ton\u0000" .`,
		`<2> <name> "pho\ton"@en .`,
		`<3> <name> "First Line\nSecondLine" .`,
		"<1> <friend_not_served> <5> .",
		`<5> <name> "" .`,
		`<6> <name> "Ding!\u0007Ding!\u0007Ding!\u0007" .`,
		`<7> <name> "node_to_delete" .`,
		fmt.Sprintf("<8> <dgraph.graphql.schema> \"%s\" .", gqlSchema),
		`<8> <dgraph.graphql.xid> "dgraph.graphql.schema" .`,
		`<8> <dgraph.type> "dgraph.graphql" .`,
		`<9> <name> "ns2" <0x2> .`,
		`<10> <name> "ns2_node_to_delete" <0x2> .`,
	}
	// This triplet will be deleted to ensure deleted nodes do not affect the output of the export.
	edgesToDelete := []string{
		`<7> <name> "node_to_delete" .`,
		`<10> <name> "ns2_node_to_delete" <0x2> .`,
	}

	idMap := map[string]uint64{
		"1": 1,
		"2": 2,
		"3": 3,
		"4": 4,
		"5": 5,
		"6": 6,
		"7": 7,
	}

	l := &lex.Lexer{}
	processEdge := func(edge string, set bool) {
		nq, err := chunker.ParseRDF(edge, l)
		require.NoError(t, err)
		rnq := dql.NQuad{NQuad: &nq}
		require.NoError(t, facets.SortAndValidate(rnq.Facets))
		e, err := rnq.ToEdgeUsing(idMap)
		e.Attr = x.NamespaceAttr(nq.Namespace, e.Attr)
		require.NoError(t, err)
		if set {
			addEdge(t, e, getOrCreate(x.DataKey(e.Attr, e.Entity)))
		} else {
			delEdge(t, e, getOrCreate(x.DataKey(e.Attr, e.Entity)))
		}
	}

	for _, edge := range rdfEdges {
		processEdge(edge, true)
	}
	for _, edge := range edgesToDelete {
		processEdge(edge, false)
	}
}

func initTestExport(t *testing.T, schemaStr string) {
	require.NoError(t, schema.ParseBytes([]byte(schemaStr), 1))

	val, err := proto.Marshal(&pb.SchemaUpdate{ValueType: pb.Posting_UID})
	require.NoError(t, err)

	txn := pstore.NewTransactionAt(math.MaxUint64, true)
	require.NoError(t, txn.Set(testutil.RootNsSchemaKey("friend"), val))
	// Schema is always written at timestamp 1
	require.NoError(t, txn.CommitAt(1, nil))

	require.NoError(t, err)
	val, err = proto.Marshal(&pb.SchemaUpdate{ValueType: pb.Posting_UID})
	require.NoError(t, err)

	txn = pstore.NewTransactionAt(math.MaxUint64, true)
	require.NoError(t, txn.Set(testutil.RootNsSchemaKey("http://www.w3.org/2000/01/rdf-schema#range"), val))
	require.NoError(t, txn.Set(testutil.RootNsSchemaKey("friend_not_served"), val))
	require.NoError(t, txn.Set(testutil.RootNsSchemaKey("age"), val))
	require.NoError(t, txn.CommitAt(1, nil))

	val, err = proto.Marshal(personType)
	require.NoError(t, err)

	txn = pstore.NewTransactionAt(math.MaxUint64, true)
	require.NoError(t, txn.Set(testutil.RootNsTypeKey("Person"), val))
	require.NoError(t, txn.CommitAt(1, nil))

	populateGraphExport(t)

	// Drop age predicate after populating DB.
	// age should not exist in the exported schema.
	txn = pstore.NewTransactionAt(math.MaxUint64, true)
	require.NoError(t, txn.Delete(testutil.RootNsSchemaKey("age")))
	require.NoError(t, txn.CommitAt(1, nil))
}

func getExportFileList(t *testing.T, bdir string) (dataFiles, schemaFiles, gqlSchema []string) {
	searchDir := bdir
	err := filepath.Walk(searchDir, func(path string, f os.FileInfo, err error) error {
		if f.IsDir() {
			return nil
		}
		if path != bdir {
			switch {
			case strings.Contains(path, "gql_schema"):
				gqlSchema = append(gqlSchema, path)
			case strings.Contains(path, "schema"):
				schemaFiles = append(schemaFiles, path)
			default:
				dataFiles = append(dataFiles, path)
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, len(dataFiles), "filelist=%v", dataFiles)

	return
}

func checkExportSchema(t *testing.T, schemaFileList []string) {
	require.Equal(t, 1, len(schemaFileList))
	file := schemaFileList[0]
	f, err := os.Open(file)
	require.NoError(t, err)

	r, err := gzip.NewReader(f)
	require.NoError(t, err)
	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)

	result, err := schema.Parse(buf.String())
	require.NoError(t, err)

	require.Equal(t, 2, len(result.Preds))
	require.Equal(t, "uid", types.TypeID(result.Preds[0].ValueType).Name())
	require.Equal(t, x.AttrInRootNamespace("http://www.w3.org/2000/01/rdf-schema#range"),
		result.Preds[1].Predicate)
	require.Equal(t, "uid", types.TypeID(result.Preds[1].ValueType).Name())

	require.Equal(t, 1, len(result.Types))
	require.True(t, proto.Equal(result.Types[0], personType))
}

func checkExportGqlSchema(t *testing.T, gqlSchemaFiles []string) {
	require.Equal(t, 1, len(gqlSchemaFiles))
	file := gqlSchemaFiles[0]
	f, err := os.Open(file)
	require.NoError(t, err)

	r, err := gzip.NewReader(f)
	require.NoError(t, err)
	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	expected := []x.ExportedGQLSchema{{Namespace: x.RootNamespace, Schema: gqlSchema}}
	b, err := json.Marshal(expected)
	require.NoError(t, err)
	require.JSONEq(t, string(b), buf.String())
}

func TestExportRdf(t *testing.T) {
	// Index the name predicate. We ensure it doesn't show up on export.
	initTestExport(t, `
		name: string @index(exact) .
		age: int .
		[0x2] name: string @index(exact) .
		`)

	bdir := t.TempDir()
	time.Sleep(1 * time.Second)

	// We have 4 friend type edges. FP("friends")%10 = 2.
	x.WorkerConfig.ExportPath = bdir
	readTs := timestamp()
	// Do the following so export won't block forever for readTs.
	posting.Oracle().ProcessDelta(&pb.OracleDelta{MaxAssigned: readTs})
	files, err := export(context.Background(), &pb.ExportRequest{ReadTs: readTs, GroupId: 1,
		Namespace: math.MaxUint64, Format: "rdf"})
	require.NoError(t, err)

	fileList, schemaFileList, gqlSchema := getExportFileList(t, bdir)
	require.Equal(t, len(files), len(fileList)+len(schemaFileList)+len(gqlSchema))

	file := fileList[0]
	f, err := os.Open(file)
	require.NoError(t, err)

	r, err := gzip.NewReader(f)
	require.NoError(t, err)

	scanner := bufio.NewScanner(r)
	count := 0

	l := &lex.Lexer{}
	for scanner.Scan() {
		nq, err := chunker.ParseRDF(scanner.Text(), l)
		require.NoError(t, err)
		require.Contains(t, []string{"0x1", "0x2", "0x3", "0x4", "0x5", "0x6", "0x9"}, nq.Subject)
		if nq.ObjectValue != nil {
			switch nq.Subject {
			case "0x1", "0x2":
				require.Equal(t, &api.Value{Val: &api.Value_DefaultVal{DefaultVal: "pho\ton"}},
					nq.ObjectValue)
			case "0x3":
				require.Equal(t, &api.Value{Val: &api.Value_DefaultVal{DefaultVal: "First Line\nSecondLine"}},
					nq.ObjectValue)
			case "0x4":
			case "0x5":
				require.Equal(t, `<0x5> <name> "" <0x0> .`, scanner.Text())
			case "0x6":
				require.Equal(t, `<0x6> <name> "Ding!\u0007Ding!\u0007Ding!\u0007" <0x0> .`,
					scanner.Text())
			case "0x9":
				require.Equal(t, `<0x9> <name> "ns2" <0x2> .`, scanner.Text())
			default:
				t.Errorf("Unexpected subject: %v", nq.Subject)
			}
			if nq.Subject == "_:uid1" || nq.Subject == "0x2" {
				require.Equal(t, &api.Value{Val: &api.Value_DefaultVal{DefaultVal: "pho\ton"}},
					nq.ObjectValue)
			}
		}

		// The only objectId we set was uid 5.
		if nq.ObjectId != "" {
			require.Equal(t, "0x5", nq.ObjectId)
		}
		// Test lang.
		if nq.Subject == "0x2" && nq.Predicate == "name" {
			require.Equal(t, "en", nq.Lang)
		}
		// Test facets.
		if nq.Subject == "0x4" {
			require.Equal(t, "age", nq.Facets[0].Key)
			require.Equal(t, "close", nq.Facets[1].Key)
			require.Equal(t, "game", nq.Facets[2].Key)
			require.Equal(t, "poem", nq.Facets[3].Key)
			require.Equal(t, "since", nq.Facets[4].Key)
			// byte representation for facets.
			require.Equal(t, []byte{0x21, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0}, nq.Facets[0].Value)
			require.Equal(t, []byte{0x1}, nq.Facets[1].Value)
			require.Equal(t, []byte("football"), nq.Facets[2].Value)
			require.Equal(t, []byte("roses are red\nviolets are blue"), nq.Facets[3].Value)
			require.Equal(t, "\x01\x00\x00\x00\x0e\xba\b8e\x00\x00\x00\x00\xff\xff",
				string(nq.Facets[4].Value))
			// valtype for facets.
			require.Equal(t, 1, int(nq.Facets[0].ValType))
			require.Equal(t, 3, int(nq.Facets[1].ValType))
			require.Equal(t, 0, int(nq.Facets[2].ValType))
			require.Equal(t, 4, int(nq.Facets[4].ValType))
		}
		// Labels have been removed.
		count++
	}
	require.NoError(t, scanner.Err())
	// This order will be preserved due to file naming.
	require.Equal(t, 10, count)

	checkExportSchema(t, schemaFileList)
	checkExportGqlSchema(t, gqlSchema)
}

func TestExportJson(t *testing.T) {
	// Index the name predicate. We ensure it doesn't show up on export.
	initTestExport(t, `name: string @index(exact) .
				 [0x2] name: string @index(exact) .`)

	bdir := t.TempDir()
	time.Sleep(1 * time.Second)

	// We have 4 friend type edges. FP("friends")%10 = 2.
	x.WorkerConfig.ExportPath = bdir
	readTs := timestamp()
	// Do the following so export won't block forever for readTs.
	posting.Oracle().ProcessDelta(&pb.OracleDelta{MaxAssigned: readTs})
	req := pb.ExportRequest{ReadTs: readTs, GroupId: 1, Format: "json", Namespace: math.MaxUint64}
	files, err := export(context.Background(), &req)
	require.NoError(t, err)

	fileList, schemaFileList, gqlSchema := getExportFileList(t, bdir)
	require.Equal(t, len(files), len(fileList)+len(schemaFileList)+len(gqlSchema))

	file := fileList[0]
	f, err := os.Open(file)
	require.NoError(t, err)

	r, err := gzip.NewReader(f)
	require.NoError(t, err)

	wantJson := `
	[
		{"uid":"0x1","namespace":"0x0","name":"pho\ton"},
		{"uid":"0x2","namespace":"0x0","name@en":"pho\ton"},
		{"uid":"0x3","namespace":"0x0","name":"First Line\nSecondLine"},
		{"uid":"0x5","namespace":"0x0","name":""},
		{"uid":"0x6","namespace":"0x0","name":"Ding!\u0007Ding!\u0007Ding!\u0007"},
		{"uid":"0x1","namespace":"0x0","friend":[{"uid":"0x5"}]},
		{"uid":"0x2","namespace":"0x0","friend":[{"uid":"0x5"}]},
		{"uid":"0x3","namespace":"0x0","friend":[{"uid":"0x5"}]},
		{"uid":"0x4","namespace":"0x0","friend":[{"uid":"0x5","friend|age":33,
			"friend|close":"true","friend|game":"football",
			"friend|poem":"roses are red\nviolets are blue","friend|since":"2005-05-02T15:04:05Z"}]},
		{"uid":"0x9","namespace":"0x2","name":"ns2"}
	]
	`
	gotJson, err := io.ReadAll(r)
	require.NoError(t, err)
	var expected interface{}
	require.NoError(t, json.Unmarshal([]byte(wantJson), &expected))

	var actual interface{}
	require.NoError(t, json.Unmarshal(gotJson, &actual))
	require.ElementsMatch(t, expected, actual)

	checkExportSchema(t, schemaFileList)
	checkExportGqlSchema(t, gqlSchema)
}

const exportRequest = `mutation export($format: String!) {
	export(input: {format: $format}) {
		response { code }
		taskId
	}
}`

func TestExportFormat(t *testing.T) {
	adminUrl := "http://" + testutil.GetSockAddrHttp() + "/admin"
	require.NoError(t, testutil.CheckForGraphQLEndpointToReady(t))

	params := testutil.GraphQLParams{
		Query:     exportRequest,
		Variables: map[string]interface{}{"format": "json"},
	}
	b, err := json.Marshal(params)
	require.NoError(t, err)

	resp, err := http.Post(adminUrl, "application/json", bytes.NewBuffer(b))
	require.NoError(t, err)

	var data interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&data))
	require.Equal(t, "Success", testutil.JsonGet(data, "data", "export", "response", "code").(string))
	taskId := testutil.JsonGet(data, "data", "export", "taskId").(string)
	testutil.WaitForTask(t, taskId, false, testutil.GetSockAddrHttp())

	params.Variables["format"] = "rdf"
	b, err = json.Marshal(params)
	require.NoError(t, err)

	resp, err = http.Post(adminUrl, "application/json", bytes.NewBuffer(b))
	require.NoError(t, err)
	testutil.RequireNoGraphQLErrors(t, resp)

	params.Variables["format"] = "xml"
	b, err = json.Marshal(params)
	require.NoError(t, err)
	resp, err = http.Post(adminUrl, "application/json", bytes.NewBuffer(b))
	require.NoError(t, err)

	defer resp.Body.Close()
	b, err = io.ReadAll(resp.Body)
	require.NoError(t, err)

	var result *testutil.GraphQLResponse
	require.NoError(t, json.Unmarshal(b, &result))
	require.NotNil(t, result.Errors)
}

type skv struct {
	attr   string
	schema pb.SchemaUpdate
}

func TestToSchema(t *testing.T) {
	testCases := []struct {
		skv      *skv
		expected string
	}{
		{
			skv: &skv{
				attr: x.AttrInRootNamespace("Alice"),
				schema: pb.SchemaUpdate{
					Predicate: x.AttrInRootNamespace("mother"),
					ValueType: pb.Posting_STRING,
					Directive: pb.SchemaUpdate_REVERSE,
					List:      false,
					Count:     true,
					Upsert:    true,
					Lang:      true,
				},
			},
			expected: "[0x0] <Alice>:string @reverse @count @lang @upsert . \n",
		},
		{
			skv: &skv{
				attr: x.NamespaceAttr(0xf2, "Alice:best"),
				schema: pb.SchemaUpdate{
					Predicate: x.NamespaceAttr(0xf2, "mother"),
					ValueType: pb.Posting_STRING,
					Directive: pb.SchemaUpdate_REVERSE,
					List:      false,
					Count:     false,
					Upsert:    false,
					Lang:      true,
				},
			},
			expected: "[0xf2] <Alice:best>:string @reverse @lang . \n",
		},
		{
			skv: &skv{
				attr: x.AttrInRootNamespace("username/password"),
				schema: pb.SchemaUpdate{
					Predicate: x.AttrInRootNamespace(""),
					ValueType: pb.Posting_STRING,
					Directive: pb.SchemaUpdate_NONE,
					List:      false,
					Count:     false,
					Upsert:    false,
					Lang:      false,
				},
			},
			expected: "[0x0] <username/password>:string . \n",
		},
		{
			skv: &skv{
				attr: x.AttrInRootNamespace("B*-tree"),
				schema: pb.SchemaUpdate{
					Predicate: x.AttrInRootNamespace(""),
					ValueType: pb.Posting_UID,
					Directive: pb.SchemaUpdate_REVERSE,
					List:      true,
					Count:     false,
					Upsert:    false,
					Lang:      false,
				},
			},
			expected: "[0x0] <B*-tree>:[uid] @reverse . \n",
		},
		{
			skv: &skv{
				attr: x.AttrInRootNamespace("base_de_données"),
				schema: pb.SchemaUpdate{
					Predicate: x.AttrInRootNamespace(""),
					ValueType: pb.Posting_STRING,
					Directive: pb.SchemaUpdate_NONE,
					List:      false,
					Count:     false,
					Upsert:    false,
					Lang:      true,
				},
			},
			expected: "[0x0] <base_de_données>:string @lang . \n",
		},
		{
			skv: &skv{
				attr: x.AttrInRootNamespace("data_base"),
				schema: pb.SchemaUpdate{
					Predicate: x.AttrInRootNamespace(""),
					ValueType: pb.Posting_STRING,
					Directive: pb.SchemaUpdate_NONE,
					List:      false,
					Count:     false,
					Upsert:    false,
					Lang:      true,
				},
			},
			expected: "[0x0] <data_base>:string @lang . \n",
		},
		{
			skv: &skv{
				attr: x.AttrInRootNamespace("data.base"),
				schema: pb.SchemaUpdate{
					Predicate: x.AttrInRootNamespace(""),
					ValueType: pb.Posting_STRING,
					Directive: pb.SchemaUpdate_NONE,
					List:      false,
					Count:     false,
					Upsert:    false,
					Lang:      true,
				},
			},
			expected: "[0x0] <data.base>:string @lang . \n",
		},
	}
	for _, testCase := range testCases {
		kv := toSchema(testCase.skv.attr, &testCase.skv.schema)
		require.Equal(t, testCase.expected, string(kv.Value))
	}
}
