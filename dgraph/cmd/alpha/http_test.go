//go:build integration

/*
 * Copyright 2017-2025 Hypermode Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package alpha

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/query"
	"github.com/hypermodeinc/dgraph/v24/x"
)

type res struct {
	Data       json.RawMessage   `json:"data"`
	Extensions *query.Extensions `json:"extensions,omitempty"`
	Errors     []x.GqlError      `json:"errors,omitempty"`
}

type params struct {
	Query     string            `json:"query"`
	Variables map[string]string `json:"variables"`
}

// runGzipWithRetry makes request gzip compressed request. If access token is expired,
// it will try to refresh access token.
func runGzipWithRetry(contentType, url string, buf io.Reader, gzReq, gzResp bool) (
	*http.Response, error) {

	client := &http.Client{}
	numRetries := 2

	var resp *http.Response
	var err error
	for i := 0; i < numRetries; i++ {
		req, err := http.NewRequest("POST", url, buf)
		if err != nil {
			return nil, err
		}
		req.Header.Add("Content-Type", contentType)
		req.Header.Set("X-Dgraph-AccessToken", token.getAccessJWTToken())

		if gzReq {
			req.Header.Set("Content-Encoding", "gzip")
		}

		if gzResp {
			req.Header.Set("Accept-Encoding", "gzip")
		}

		resp, err = client.Do(req)
		if err != nil && strings.Contains(err.Error(), "Token is expired") {
			err := token.refreshToken()
			if err != nil {
				return nil, err
			}
			continue
		} else if err != nil {
			return nil, err
		}
		break
	}

	return resp, err
}

func queryWithGz(queryText, contentType, debug, timeout string, gzReq, gzResp bool) (
	string, *http.Response, error) {

	params := make([]string, 0, 2)
	if debug != "" {
		params = append(params, "debug="+debug)
	}
	if timeout != "" {
		params = append(params, fmt.Sprintf("timeout=%v", timeout))
	}
	url := addr + "/query?" + strings.Join(params, "&")

	var buf *bytes.Buffer
	if gzReq {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		_, _ = gz.Write([]byte(queryText))
		_ = gz.Close()
		buf = &b
	} else {
		buf = bytes.NewBufferString(queryText)
	}

	resp, err := runGzipWithRetry(contentType, url, buf, gzReq, gzResp)
	if err != nil {
		return "", nil, err
	}

	defer resp.Body.Close()
	rd := resp.Body
	if err != nil {
		return "", nil, err
	}

	if gzResp {
		if strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") {
			rd, err = gzip.NewReader(rd)
			if err != nil {
				return "", nil, err
			}
			defer rd.Close()
		} else {
			return "", resp, errors.Errorf("Response not compressed")
		}
	}
	body, err := io.ReadAll(rd)
	if err != nil {
		return "", nil, err
	}

	var r res
	if err := json.Unmarshal(body, &r); err != nil {
		return "", nil, err
	}

	// Check for errors
	if len(r.Errors) != 0 {
		return "", nil, errors.New(r.Errors[0].Message)
	}

	// Remove the extensions.
	r2 := res{
		Data: r.Data,
	}
	output, err := json.Marshal(r2)

	return string(output), resp, err
}

type queryInp struct {
	body    string
	typ     string
	debug   string
	ts      uint64
	hash    string
	respFmt string
}

type tsInfo struct {
	ts   uint64
	hash string
}

func queryWithTs(inp queryInp) (string, *tsInfo, error) {
	out, tsInfo, _, err := queryWithTsForResp(inp)
	return out, tsInfo, err
}

// queryWithTsForResp query the dgraph and returns it's http response and result.
func queryWithTsForResp(inp queryInp) (string, *tsInfo, *http.Response, error) {
	params := make([]string, 0, 3)
	if inp.debug != "" {
		params = append(params, "debug="+inp.debug)
	}
	if inp.ts != 0 {
		params = append(params, fmt.Sprintf("startTs=%v", strconv.FormatUint(inp.ts, 10)))
		params = append(params, fmt.Sprintf("hash=%s", inp.hash))
	}
	if inp.respFmt != "" {
		params = append(params, fmt.Sprintf("respFormat=%s", inp.respFmt))
	}
	url := addr + "/query?" + strings.Join(params, "&")

	_, body, resp, err := runWithRetriesForResp("POST", inp.typ, url, inp.body)
	if err != nil {
		return "", nil, resp, err
	}

	var r res
	if err := json.Unmarshal(body, &r); err != nil {
		return "", nil, resp, err
	}
	startTs := r.Extensions.Txn.StartTs
	hash := r.Extensions.Txn.Hash

	// Remove the extensions
	r2 := res{Data: r.Data}
	output, err := json.Marshal(r2)
	return string(output), &tsInfo{ts: startTs, hash: hash}, resp, err
}

type mutationResponse struct {
	keys    []string
	preds   []string
	startTs uint64
	hash    string
	data    json.RawMessage
	cost    string
}

type mutationInp struct {
	body      string
	typ       string
	isJson    bool
	commitNow bool
	ts        uint64
	hash      string
}

func mutationWithTs(inp mutationInp) (mutationResponse, error) {
	params := make([]string, 0, 3)
	if inp.ts != 0 {
		params = append(params, "startTs="+strconv.FormatUint(inp.ts, 10))
		params = append(params, "hash="+inp.hash)
	}

	var mr mutationResponse
	if inp.commitNow {
		params = append(params, "commitNow=true")
	}
	url := addr + "/mutate?" + strings.Join(params, "&")
	_, body, resp, err := runWithRetriesForResp("POST", inp.typ, url, inp.body)
	if err != nil {
		return mr, err
	}
	mr.cost = resp.Header.Get(x.DgraphCostHeader)

	var r res
	if err := json.Unmarshal(body, &r); err != nil {
		return mr, err
	}

	mr.keys = r.Extensions.Txn.Keys
	mr.preds = r.Extensions.Txn.Preds
	mr.startTs = r.Extensions.Txn.StartTs
	mr.hash = r.Extensions.Txn.Hash
	sort.Strings(mr.preds)

	var d map[string]interface{}
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return mr, err
	}
	delete(d, "code")
	delete(d, "message")
	delete(d, "uids")
	mr.data, err = json.Marshal(d)
	if err != nil {
		return mr, err
	}
	return mr, nil
}

func createRequest(method, contentType, url string, body string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		return nil, err
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	return req, nil
}

func runWithRetries(method, contentType, url string, body string) (
	*x.QueryResWithData, []byte, error) {
	qr, respBody, _, err := runWithRetriesForResp(method, contentType, url, body)
	return qr, respBody, err
}

// attach the grootAccessJWT to the request and sends the http request
func runRequest(req *http.Request) (*x.QueryResWithData, []byte, *http.Response, error) {
	client := &http.Client{}
	req.Header.Set("X-Dgraph-AccessToken", token.getAccessJWTToken())
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, resp, err
	}
	if status := resp.StatusCode; status != http.StatusOK {
		return nil, nil, resp, errors.Errorf("Unexpected status code: %v", status)
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, resp, errors.Errorf("unable to read from body: %v", err)
	}

	qr := new(x.QueryResWithData)
	_ = json.Unmarshal(body, qr) // Don't check error.
	if len(qr.Errors) > 0 {
		return nil, nil, resp, errors.New(qr.Errors[0].Message)
	}
	return qr, body, resp, nil
}

func runWithRetriesForResp(method, contentType, url string, body string) (
	*x.QueryResWithData, []byte, *http.Response, error) {

	req, err := createRequest(method, contentType, url, body)
	if err != nil {
		return nil, nil, nil, err
	}

	qr, respBody, resp, err := runRequest(req)
	if err != nil && strings.Contains(err.Error(), "Token is expired") {
		if err := token.refreshToken(); err != nil {
			return nil, nil, nil, err
		}

		// create a new request since the previous request would have been closed upon the err
		retryReq, err := createRequest(method, contentType, url, body)
		if err != nil {
			return nil, nil, resp, err
		}

		return runRequest(retryReq)
	}
	return qr, respBody, resp, err
}

func commitWithTs(mr mutationResponse, abort bool) error {
	url := addr + "/commit"
	if mr.startTs != 0 {
		url += "?startTs=" + strconv.FormatUint(mr.startTs, 10)
		url += "&hash=" + mr.hash
	}
	if abort {
		if mr.startTs != 0 {
			url += "&abort=true"
		} else {
			url += "?abort=true"
		}
	}

	m := make(map[string]interface{})
	m["keys"] = mr.keys
	m["preds"] = mr.preds
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	_, _, _, err = runRequest(req)
	return err
}

func commitWithTsKeysOnly(keys []string, ts uint64, hash string) error {
	url := addr + "/commit"
	if ts != 0 {
		url += "?startTs=" + strconv.FormatUint(ts, 10)
		url += "&hash=" + hash
	}

	b, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	_, _, _, err = runRequest(req)
	return err
}

func TestTransactionBasic(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`name: string .`))
	require.NoError(t, alterSchema(`name: string @index(term) .`))

	q1 := `
	{
	  balances(func: anyofterms(name, "Alice Bob")) {
	    name
	    balance
	  }
	}
	`
	_, tsInfo, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	ts := tsInfo.ts
	hash := tsInfo.hash

	m1 := `
    {
	  set {
		_:alice <name> "Bob" .
		_:alice <balance> "110" .
		_:bob <balance> "60" .
	  }
	}
	`

	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", ts: ts, hash: hash})
	require.NoError(t, err)
	require.Equal(t, mr.startTs, ts)
	require.Equal(t, 4, len(mr.keys))
	require.Equal(t, 2, len(mr.preds))
	var parsedPreds []string
	for _, pred := range mr.preds {
		p := strings.SplitN(pred, "-", 2)[1]
		parsedPreds = append(parsedPreds, x.ParseAttr(p))
	}
	sort.Strings(parsedPreds)
	require.Equal(t, "balance", parsedPreds[0])
	require.Equal(t, "name", parsedPreds[1])

	data, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Equal(t, `{"data":{"balances":[]}}`, data)

	// Query with same timestamp.
	data, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql", ts: ts, hash: hash})
	require.NoError(t, err)
	require.Equal(t, `{"data":{"balances":[{"name":"Bob","balance":"110"}]}}`, data)

	// Commit and query.
	require.NoError(t, commitWithTs(mr, false))
	data, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Equal(t, `{"data":{"balances":[{"name":"Bob","balance":"110"}]}}`, data)
}

func TestTransactionBasicNoPreds(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`name: string @index(term) .`))

	q1 := `
	{
	  balances(func: anyofterms(name, "Alice Bob")) {
	    name
	    balance
	  }
	}
	`
	_, tsInfo, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	ts := tsInfo.ts
	hash := tsInfo.hash

	m1 := `
    {
	  set {
		_:alice <name> "Bob" .
		_:alice <balance> "110" .
		_:bob <balance> "60" .
	  }
	}
	`

	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", ts: ts, hash: hash})
	require.NoError(t, err)
	require.Equal(t, mr.startTs, ts)
	require.Equal(t, 4, len(mr.keys))

	data, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Equal(t, `{"data":{"balances":[]}}`, data)

	// Query with same timestamp.
	data, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql", ts: ts, hash: hash})
	require.NoError(t, err)
	require.Equal(t, `{"data":{"balances":[{"name":"Bob","balance":"110"}]}}`, data)

	// Commit and query.
	require.NoError(t, commitWithTs(mr, false))
	data, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Equal(t, `{"data":{"balances":[{"name":"Bob","balance":"110"}]}}`, data)
}
func TestTransactionForCost(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`name: string @index(term) .`))

	q1 := `
	{
	  balances(func: anyofterms(name, "Alice Bob")) {
	    name
	    balance
	  }
	}
	`
	_, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)

	m1 := `
    {
	  set {
		_:alice <name> "Bob" .
		_:alice <balance> "110" .
		_:bob <balance> "60" .
	  }
	}
	`

	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.Equal(t, "5", mr.cost)

	_, _, resp, err := queryWithTsForResp(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Equal(t, "2", resp.Header.Get(x.DgraphCostHeader))
}

func TestTransactionBasicOldCommitFormat(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`name: string @index(term) .`))

	q1 := `
	{
	  balances(func: anyofterms(name, "Alice Bob")) {
	    name
	    balance
	  }
	}
	`
	_, tsInfo, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	ts := tsInfo.ts
	hash := tsInfo.hash

	m1 := `
    {
	  set {
		_:alice <name> "Bob" .
		_:alice <balance> "110" .
		_:bob <balance> "60" .
	  }
	}
	`

	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", ts: ts, hash: hash})
	require.NoError(t, err)
	require.Equal(t, mr.startTs, ts)
	require.Equal(t, 4, len(mr.keys))

	data, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Equal(t, `{"data":{"balances":[]}}`, data)

	// Query with same timestamp.
	data, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql", ts: ts, hash: hash})
	require.NoError(t, err)
	require.Equal(t, `{"data":{"balances":[{"name":"Bob","balance":"110"}]}}`, data)

	// One more time, with json body this time.
	d1, err := json.Marshal(params{Query: q1})
	require.NoError(t, err)
	data, _, err = queryWithTs(
		queryInp{body: string(d1), typ: "application/json", ts: ts, hash: hash})
	require.NoError(t, err)
	require.Equal(t, `{"data":{"balances":[{"name":"Bob","balance":"110"}]}}`, data)

	// Commit (using a list of keys instead of a map) and query.
	require.NoError(t, commitWithTsKeysOnly(mr.keys, ts, mr.hash))
	data, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Equal(t, `{"data":{"balances":[{"name":"Bob","balance":"110"}]}}`, data)

	// Aborting a transaction
	url := fmt.Sprintf("%s/commit?startTs=%d&abort=true&hash=%s", addr, ts, mr.hash)
	req, err := http.NewRequest("POST", url, nil)
	require.NoError(t, err)
	_, _, _, err = runRequest(req)
	require.NoError(t, err)
}

func TestAlterAllFieldsShouldBeSet(t *testing.T) {
	req, err := http.NewRequest("PUT", "/alter", bytes.NewBufferString(
		`{"dropall":true}`, // "dropall" is spelt incorrect - should be "drop_all"
	))
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(alterHandler)
	handler.ServeHTTP(rr, req)

	require.Equal(t, rr.Code, http.StatusOK)
	var qr x.QueryResWithData
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &qr))
	require.Len(t, qr.Errors, 1)
	require.Equal(t, "Error", qr.Errors[0].Extensions["code"])
}

// This test is a basic sanity test to check nothing breaks in the alter API.
func TestAlterSanity(t *testing.T) {
	ops := []string{`{"drop_attr": "name"}`,
		`{"drop_op": "TYPE", "drop_value": "Film"}`,
		`{"drop_op": "DATA"}`,
		`{"drop_all":true}`}

	for _, op := range ops {
		qr, _, err := runWithRetries("PUT", "", addr+"/alter", op)
		require.NoError(t, err)
		require.Len(t, qr.Errors, 0)
	}
}

func TestHttpCompressionSupport(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`name: string .`))
	require.NoError(t, alterSchema(`name: string @index(term) .`))

	q1 := `
	{
	  names(func: has(name), orderasc: name) {
	    name
	  }
	}
	`

	q2 := `
	query all($name: string) {
	  names(func: eq(name, $name)) {
	    name
	  }
	}
	`

	m1 := `
	{
	  set {
		_:a <name> "Alice" .
		_:b <name> "Bob" .
		_:c <name> "Charlie" .
		_:d <name> "David" .
		_:e <name> "Emily" .
		_:f <name> "Frank" .
		_:g <name> "Gloria" .
		_:h <name> "Hannah" .
		_:i <name> "Ian" .
		_:j <name> "Judy" .
		_:k <name> "Kevin" .
		_:l <name> "Linda" .
		_:m <name> "Michael" .
	  }
	}
	`

	r1 := `{"data":{"names":[{"name":"Alice"},{"name":"Bob"},{"name":"Charlie"},{"name":"David"},` +
		`{"name":"Emily"},{"name":"Frank"},{"name":"Gloria"},{"name":"Hannah"},{"name":"Ian"},` +
		`{"name":"Judy"},{"name":"Kevin"},{"name":"Linda"},{"name":"Michael"}]}}`
	require.NoError(t, runMutation(m1))

	data, resp, err := queryWithGz(q1, "application/dql", "false", "", false, false)
	require.NoError(t, err)
	require.Equal(t, r1, data)
	require.Empty(t, resp.Header.Get("Content-Encoding"))

	data, resp, err = queryWithGz(q1, "application/dql", "", "", false, true)
	require.NoError(t, err)
	require.Equal(t, r1, data)
	require.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))

	data, resp, err = queryWithGz(q1, "application/dql", "", "", true, false)
	require.NoError(t, err)
	require.Equal(t, r1, data)
	require.Empty(t, resp.Header.Get("Content-Encoding"))

	data, resp, err = queryWithGz(q1, "application/dql", "", "", true, true)
	require.NoError(t, err)
	require.Equal(t, r1, data)
	require.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))

	// query with timeout
	data, _, err = queryWithGz(q1, "application/dql", "", "100us", false, false)
	requireDeadline(t, err)
	require.Equal(t, "", data)

	data, resp, err = queryWithGz(q1, "application/dql", "", "1s", false, false)
	require.NoError(t, err)
	require.Equal(t, r1, data)
	require.Empty(t, resp.Header.Get("Content-Encoding"))

	d1, err := json.Marshal(params{Query: q1})
	require.NoError(t, err)
	data, resp, err = queryWithGz(string(d1), "application/json", "", "1s", false, false)
	require.NoError(t, err)
	require.Equal(t, r1, data)
	require.Empty(t, resp.Header.Get("Content-Encoding"))

	d2, err := json.Marshal(params{
		Query: q2,
		Variables: map[string]string{
			"$name": "Alice",
		},
	})
	require.NoError(t, err)
	data, resp, err = queryWithGz(string(d2), "application/json", "", "1s", false, false)
	require.NoError(t, err)
	require.Equal(t, `{"data":{"names":[{"name":"Alice"}]}}`, data)
	require.Empty(t, resp.Header.Get("Content-Encoding"))
}

func requireDeadline(t *testing.T, err error) {
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Logf("Got error: %v when expecting context deadline exceeded", err)
		t.Fail()
	}
}

func TestDebugSupport(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`name: string @index(term) .`))

	m1 := `
	{
	  set {
		_:a <name> "Alice" .
		_:b <name> "Bob" .
		_:c <name> "Charlie" .
		_:d <name> "David" .
		_:e <name> "Emily" .
		_:f <name> "Frank" .
		_:g <name> "Gloria" .
	  }
	}
	`
	require.NoError(t, runMutation(m1))

	q1 := `
	{
	  users(func: has(name), orderasc: name) {
	    name
	  }
	}
	`

	requireEqual := func(t *testing.T, data string) {
		var r struct {
			Data struct {
				Users []struct {
					Name string `json:"name"`
					UID  string `json:"uid"`
				} `json:"users"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			require.NoError(t, err)
		}

		exp := []string{"Alice", "Bob", "Charlie", "David", "Emily", "Frank", "Gloria"}
		actual := make([]string, 0, len(exp))
		for _, u := range r.Data.Users {
			actual = append(actual, u.Name)
			require.NotEmpty(t, u.UID, "uid should be nonempty in debug mode")
		}
		sort.Strings(actual)
		require.Equal(t, exp, actual)
	}

	data, resp, err := queryWithGz(q1, "application/dql", "true", "", false, false)
	require.NoError(t, err)
	requireEqual(t, data)
	require.Empty(t, resp.Header.Get("Content-Encoding"))

	data, resp, err = queryWithGz(q1, "application/dql", "true", "", false, true)
	require.NoError(t, err)
	requireEqual(t, data)
	require.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))

	data, resp, err = queryWithGz(q1, "application/dql", "true", "", true, false)
	require.NoError(t, err)
	requireEqual(t, data)
	require.Empty(t, resp.Header.Get("Content-Encoding"))

	data, resp, err = queryWithGz(q1, "application/dql", "true", "", true, true)
	require.NoError(t, err)
	requireEqual(t, data)
	require.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))

	// query with timeout
	data, _, err = queryWithGz(q1, "application/dql", "true", "100us", false, false)
	requireDeadline(t, err)
	require.Equal(t, "", data)

	data, resp, err = queryWithGz(q1, "application/dql", "true", "3s", false, false)
	require.NoError(t, err)
	requireEqual(t, data)
	require.Empty(t, resp.Header.Get("Content-Encoding"))

	d1, err := json.Marshal(params{Query: q1})
	require.NoError(t, err)
	data, resp, err = queryWithGz(string(d1), "application/json", "true", "3s", false, false)
	require.NoError(t, err)
	requireEqual(t, data)
	require.Empty(t, resp.Header.Get("Content-Encoding"))

	// This test passes access token along with debug flag
	data, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql", debug: "true"})
	require.NoError(t, err)
	requireEqual(t, data)
	require.Empty(t, resp.Header.Get("Content-Encoding"))
}

func TestHealth(t *testing.T) {
	url := fmt.Sprintf("%s/health", addr)
	resp, err := http.Get(url)
	require.NoError(t, err)

	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var info []pb.HealthInfo
	require.NoError(t, json.Unmarshal(data, &info))
	require.Equal(t, "alpha", info[0].Instance)
	require.True(t, info[0].Uptime > int64(time.Duration(1)))
}

func setDrainingMode(t *testing.T, enable bool, accessJwt string) {
	drainingRequest := `mutation drain($enable: Boolean) {
		draining(enable: $enable) {
			response {
				code
			}
		}
	}`
	params := dgraphapi.GraphQLParams{
		Query:     drainingRequest,
		Variables: map[string]interface{}{"enable": enable},
	}
	resp, err := hc.RunGraphqlQuery(params, true)
	require.NoError(t, err)
	require.JSONEq(t, `{"draining":{"response":{"code":"Success"}}}`, string(resp))
}

func TestDrainingMode(t *testing.T) {
	runRequests := func(expectErr bool) {
		q1 := `
	{
	  alice(func: has(name)) {
	    name
	  }
	}
	`
		_, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
		if expectErr {
			require.True(t, err != nil && strings.Contains(err.Error(), "the server is in draining mode"))
		} else {
			require.NoError(t, err, "Got error while running query: %v", err)
		}

		m1 := `
    {
	  set {
		_:alice <name> "Alice" .
	  }
	}
	`
		_, err = mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true, ts: ts})
		if expectErr {
			require.True(t, err != nil && strings.Contains(err.Error(), "the server is in draining mode"))
		} else {
			require.NoError(t, err, "Got error while running mutation: %v", err)
		}

		err = alterSchema(`name: string @index(term) .`)
		if expectErr {
			require.True(t, err != nil && strings.Contains(err.Error(), "the server is in draining mode"))
		} else {
			require.NoError(t, err, "Got error while running alter: %v", err)
		}

	}

	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, 0))

	setDrainingMode(t, true, hc.AccessJwt)
	runRequests(true)

	setDrainingMode(t, false, hc.AccessJwt)
	runRequests(false)
}

func TestOptionsForUiKeywords(t *testing.T) {
	req, err := http.NewRequest(http.MethodOptions, fmt.Sprintf("%s/ui/keywords", addr), nil)
	require.NoError(t, err)

	client := &http.Client{}

	resp, err := client.Do(req)
	require.NoError(t, err)
	require.True(t, resp.StatusCode >= 200 && resp.StatusCode < 300)
}

func TestNonExistentPath(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/non-existent-url", addr), nil)
	require.NoError(t, err)

	client := &http.Client{}

	resp, err := client.Do(req)
	require.NoError(t, err)
	require.Equal(t, resp.StatusCode, 404)
	require.Equal(t, resp.Status, "404 Not Found")
}

func TestUrl(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, addr, nil)
	require.NoError(t, err)

	client := &http.Client{}

	resp, err := client.Do(req)
	require.NoError(t, err)
	require.True(t, resp.StatusCode >= 200 && resp.StatusCode < 300)
}

func TestContentTypeCharset(t *testing.T) {
	_, _, err := queryWithGz(`{"query": "schema {}"}`, "application/json; charset=utf-8", "false", "", false, false)
	require.NoError(t, err)

	_, _, err = queryWithGz(`{"query": "schema {}"}`, "application/json; charset=latin1", "false", "", false, false)
	require.True(t, err != nil && strings.Contains(err.Error(), "Unsupported charset"))

	_, err = mutationWithTs(
		mutationInp{body: `{}`, typ: "application/rdf; charset=utf-8", commitNow: true})
	require.NoError(t, err)

	_, err = mutationWithTs(
		mutationInp{body: `{}`, typ: "application/rdf; charset=latin1", commitNow: true})
	require.True(t, err != nil && strings.Contains(err.Error(), "Unsupported charset"))
}

func TestQueryBackwardCompatibleWithGraphqlPlusMinusHeader(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`name: string @index(term) .`))

	q1 := `
	{
	  balances(func: anyofterms(name, "Alice Bob")) {
	    name
	    balance
	  }
	}
	`
	_, _, err := queryWithTs(queryInp{body: q1, typ: "application/graphql+-"})
	require.NoError(t, err)

	m1 := `
    {
	  set {
		_:alice <name> "Bob" .
		_:alice <balance> "110" .
		_:bob <balance> "60" .
	  }
	}
	`

	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.Equal(t, "5", mr.cost)

	_, _, resp, err := queryWithTsForResp(queryInp{body: q1, typ: "application/graphql+-"})
	require.NoError(t, err)
	require.Equal(t, "2", resp.Header.Get(x.DgraphCostHeader))
}

func TestQueryRdfData(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`name: string @index(term) .`))

	// empty RDF response
	q1 := `
	{
	  balances(func: anyofterms(name, "Alice Bob")) {
	    name
	    balance
	  }
	}`
	resp, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql", respFmt: "rdf"})
	require.NoError(t, err)
	require.Equal(t, `{"data":""}`, resp)

	m1 := `
    {
	  set {
		_:bob <name> "Bob \"<the builder>\"" .
		_:bob <balance> "110" .
		_:alice <balance> "60" .
	  }
	}`
	_, err = mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	// json query
	resp, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Equal(t, `{"data":{"balances":[{"name":"Bob \"\u003cthe builder\u003e\"","balance":"110"}]}}`, resp)

	// rdf query
	resp, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql", respFmt: "rdf"})
	require.NoError(t, err)
	var r2 struct {
		Data string `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp), &r2))

	// we apply the same RDF mutation and it should work
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`name: string @index(term) .`))
	_, err = mutationWithTs(mutationInp{body: `{set {` + r2.Data + `}}`, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	resp, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Equal(t, `{"data":{"balances":[{"name":"Bob \"\u003cthe builder\u003e\"","balance":"110"}]}}`, resp)
}
