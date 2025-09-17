/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package audit

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/dgraph-io/gqlparser/v2/ast"
	"github.com/dgraph-io/gqlparser/v2/parser"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/x"

	"github.com/golang/glog"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	maxReqLength = 4 << 10 // 4 KB
)

var skipApis = map[string]bool{
	// raft server
	"Heartbeat":   true,
	"RaftMessage": true,
	"JoinCluster": true,
	"IsPeer":      true,
	// zero server
	"StreamMembership": true,
	"UpdateMembership": true,
	"Oracle":           true,
	"Timestamps":       true,
	"ShouldServe":      true,
	"Connect":          true,
	// health server
	"Check": true,
	"Watch": true,
}

var skipEPs = map[string]bool{
	// list of endpoints that needs to be skipped
	"/health":        true,
	"/state":         true,
	"/probe/graphql": true,
}

func AuditRequestGRPC(ctx context.Context, req interface{},
	info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	skip := func(method string) bool {
		return skipApis[info.FullMethod[strings.LastIndex(info.FullMethod, "/")+1:]]
	}

	if auditEnabled.Load() == 0 || skip(info.FullMethod) {
		return handler(ctx, req)
	}
	response, err := handler(ctx, req)
	auditGrpc(ctx, req, info)
	return response, err
}

func AuditRequestHttp(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		skip := func(method string) bool {
			return skipEPs[r.URL.Path]
		}

		if auditEnabled.Load() == 0 || skip(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// Websocket connection in graphQl happens differently. We only get access tokens and
		// metadata in payload later once the connection is upgraded to correct protocol.
		// Doc: https://github.com/apollographql/subscriptions-transport-ws/blob/v0.9.4/PROTOCOL.md
		//
		// Auditing for websocket connections will be handled by graphql/admin/http.go:154#Subscribe
		for _, subprotocol := range websocket.Subprotocols(r) {
			if subprotocol == "graphql-ws" {
				next.ServeHTTP(w, r)
				return
			}
		}

		rw := NewResponseWriter(w)
		var buf bytes.Buffer
		tee := io.TeeReader(r.Body, &buf)
		r.Body = io.NopCloser(tee)
		next.ServeHTTP(rw, r)
		r.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
		auditHttp(rw, r)
	})
}

func AuditWebSockets(ctx context.Context, req *schema.Request) {
	if auditEnabled.Load() == 0 {
		return
	}

	namespace := uint64(0)
	var err error
	var user string
	// TODO(anurag): X-Dgraph-AccessToken should be exported as a constant
	if token := req.Header.Get("X-Dgraph-AccessToken"); token != "" {
		user = getUser(token, false)
		namespace, err = x.ExtractNamespaceFromJwt(token)
		if err != nil {
			glog.Warningf("Error while auditing websockets: %s", err)
		}
	} else if token := req.Header.Get("X-Dgraph-AuthToken"); token != "" {
		user = getUser(token, true)
	} else {
		user = getUser("", false)
	}

	ip := ""
	if peerInfo, ok := peer.FromContext(ctx); ok {
		ip, _, _ = net.SplitHostPort(peerInfo.Addr.String())
	}

	auditor.Audit(&AuditEvent{
		User:        user,
		Namespace:   namespace,
		ServerHost:  x.WorkerConfig.MyAddr,
		ClientHost:  ip,
		Endpoint:    "/graphql",
		ReqType:     WebSocket,
		Req:         truncate(req.Query, maxReqLength),
		Status:      http.StatusText(http.StatusOK),
		QueryParams: nil,
	})
}

func auditGrpc(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo) {
	clientHost := ""
	if p, ok := peer.FromContext(ctx); ok {
		clientHost = p.Addr.String()
	}
	var user string
	var namespace uint64
	var err error
	extractUser := func(md metadata.MD) {
		if t := md.Get("accessJwt"); len(t) > 0 {
			user = getUser(t[0], false)
		} else if t := md.Get("auth-token"); len(t) > 0 {
			user = getUser(t[0], true)
		} else {
			user = getUser("", false)
		}
	}

	extractNamespace := func(md metadata.MD) {
		ns := md.Get("namespace")
		if len(ns) == 0 {
			namespace = UnknownNamespace
		} else {
			if namespace, err = strconv.ParseUint(ns[0], 10, 64); err != nil {
				namespace = UnknownNamespace
			}
		}
	}

	if md, ok := metadata.FromIncomingContext(ctx); ok {
		extractUser(md)
		extractNamespace(md)
	}

	cd := codes.Unknown
	if serr, ok := status.FromError(err); ok {
		cd = serr.Code()
	}

	reqBody := checkRequestBody(Grpc, info.FullMethod[strings.LastIndex(info.FullMethod,
		"/")+1:], fmt.Sprintf("%+v", req))
	auditor.Audit(&AuditEvent{
		User:       user,
		Namespace:  namespace,
		ServerHost: x.WorkerConfig.MyAddr,
		ClientHost: clientHost,
		Endpoint:   info.FullMethod,
		ReqType:    Grpc,
		Req:        truncate(reqBody, maxReqLength),
		Status:     cd.String(),
	})
}

func auditHttp(w *ResponseWriter, r *http.Request) {
	body := getRequestBody(r)
	var user string
	if token := r.Header.Get("X-Dgraph-AccessToken"); token != "" {
		user = getUser(token, false)
	} else if token := r.Header.Get("X-Dgraph-AuthToken"); token != "" {
		user = getUser(token, true)
	} else {
		user = getUser("", false)
	}

	auditor.Audit(&AuditEvent{
		User:        user,
		Namespace:   x.ExtractNamespaceHTTP(r),
		ServerHost:  x.WorkerConfig.MyAddr,
		ClientHost:  r.RemoteAddr,
		Endpoint:    r.URL.Path,
		ReqType:     Http,
		Req:         truncate(checkRequestBody(Http, r.URL.Path, string(body)), maxReqLength),
		Status:      http.StatusText(w.statusCode),
		QueryParams: r.URL.Query(),
	})
}

// password fields are accessible only via /admin endpoint hence,
// this will be only called with /admin endpoint
func maskPasswordFieldsInGQL(req string) string {
	var gqlReq schema.Request
	err := json.Unmarshal([]byte(req), &gqlReq)
	if err != nil {
		glog.Errorf("unable to unmarshal gql request %v", err)
		return req
	}
	query, gErr := parser.ParseQuery(&ast.Source{
		Input: gqlReq.Query,
	})
	if gErr != nil {
		glog.Errorf("unable to parse gql request %+v", gErr)
		return req
	}
	if len(query.Operations) == 0 {
		return req
	}
	var variableName string
	for _, op := range query.Operations {
		if op.Operation != ast.Mutation || len(op.SelectionSet) == 0 {
			continue
		}

		for _, ss := range op.SelectionSet {
			if f, ok := ss.(*ast.Field); ok && len(f.Arguments) > 0 {
				variableName = getMaskedFieldVarName(f)
			}
		}
	}

	// no variable present
	if variableName == "" {
		regex, err := regexp.Compile(
			`password[\s]?(.*?)[\s]?:[\s]?(.*?)[\s]?"[\s]?(.*?)[\s]?"`)
		if err != nil {
			return req
		}
		return regex.ReplaceAllString(req, "*******")
	}
	regex, err := regexp.Compile(
		fmt.Sprintf(`"%s[\s]?(.*?)[\s]?"[\s]?(.*?)[\s]?:[\s]?(.*?)[\s]?"[\s]?(.*?)[\s]?"`,
			variableName[1:]))
	if err != nil {
		return req
	}
	return regex.ReplaceAllString(req, "*******")
}

func getMaskedFieldVarName(f *ast.Field) string {
	switch f.Name {
	case "resetPassword":
		for _, a := range f.Arguments {
			if a.Name != "input" || a.Value == nil || a.Value.Children == nil {
				continue
			}

			for _, c := range a.Value.Children {
				if c.Name == "password" && c.Value.Kind == ast.Variable {
					return c.Value.String()
				}
			}
		}
	case "login":
		for _, a := range f.Arguments {
			if a.Name == "password" && a.Value.Kind == ast.Variable {
				return a.Value.String()
			}
		}
	}
	return ""
}

var skipReqBodyGrpc = map[string]bool{
	"Login": true,
}

func checkRequestBody(reqType string, path string, body string) string {
	switch reqType {
	case Grpc:
		if skipReqBodyGrpc[path] {
			regex, err := regexp.Compile(
				`password[\s]?(.*?)[\s]?:[\s]?(.*?)[\s]?"[\s]?(.*?)[\s]?"`)
			if err != nil {
				return body
			}
			body = regex.ReplaceAllString(body, "*******")
		}
	case Http:
		if path == "/admin" {
			return maskPasswordFieldsInGQL(body)
		} else if path == "/grapqhl" {
			regex, err := regexp.Compile(
				`check[\s]?(.*?)[\s]?Password[\s]?(.*?)[\s]?:[\s]?(.*?)[\s]?"[\s]?(.*?)[\s]?"`)
			if err != nil {
				return body
			}
			body = regex.ReplaceAllString(body, "*******")
		}
	}
	return body
}

func getRequestBody(r *http.Request) []byte {
	var in io.Reader = r.Body
	if enc := r.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		if enc == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				return []byte(err.Error())
			}
			defer gz.Close()
			in = gz
		} else {
			return []byte("unknown encoding")
		}
	}

	body, err := io.ReadAll(in)
	if err != nil {
		return []byte(err.Error())
	}
	return body
}

func getUser(token string, poorman bool) string {
	if poorman {
		return PoorManAuth
	}
	var user string
	var err error
	if token == "" {
		if x.WorkerConfig.AclEnabled {
			user = UnauthorisedUser
		}
	} else {
		if user, err = x.ExtractUserName(token); err != nil {
			user = UnknownUser
		}
	}
	return user
}

type ResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func NewResponseWriter(w http.ResponseWriter) *ResponseWriter {
	// WriteHeader(int) is not called if our response implicitly returns 200 OK, so
	// we default to that status code.
	return &ResponseWriter{w, http.StatusOK}
}

func (rw *ResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func truncate(s string, l int) string {
	if len(s) > l {
		return s[:l]
	}
	return s
}
