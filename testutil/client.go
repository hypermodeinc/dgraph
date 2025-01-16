/*
 * Copyright 2025 Hypermode Inc. and Contributors
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

package testutil

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/dql"
	"github.com/hypermodeinc/dgraph/v24/x"
)

// socket addr = IP address and port number
var (
	// Instance is the instance name of the Alpha.
	DockerPrefix string
	// Global test data directory used to store resources
	TestDataDirectory string
	Instance          string
	MinioInstance     string
	// SockAddr is the address to the gRPC endpoint of the alpha used during tests with localhost
	SockAddrLocalhost string
	// SockAddr is the address to the gRPC endpoint of the alpha used during tests.
	SockAddr string
	// SockAddrHttp is the address to the HTTP of alpha used during tests.
	SockAddrHttp          string
	SockAddrHttpLocalhost string
	// SockAddrZero is the address to the gRPC endpoint of the zero used during tests.
	SockAddrZero string
	// SockAddrZeroHttp is the address to the HTTP endpoint of the zero used during tests.
	SockAddrZeroHttp      string
	SockAddrZeroLocalhost string

	// SockAddrAlpha4 is the address to the gRPC endpoint of the alpha4 used during restore tests.
	SockAddrAlpha4 string
	// SockAddrAlpha4Http is the address to the HTTP of alpha4 used during restore tests.
	SockAddrAlpha4Http string
	// SockAddrZero4 is the address to the gRPC endpoint of the zero4 used during restore tests.
	SockAddrZero4 string
	// SockAddrZero4Http is the address to the HTTP endpoint of the zero4 used during restore tests.
	SockAddrZero4Http string
	// SockAddrAlpha7 is the address to the gRPC endpoint of the alpha4 used during restore tests.
	SockAddrAlpha7 string
	// SockAddrAlpha7Http is the address to the HTTP of alpha7 used during restore tests.
	SockAddrAlpha7Http string
	// SockAddrZero7 is the address to the gRPC endpoint of the zero7 used during restore tests.
	SockAddrZero7 string
	// SockAddrZero7Http is the address to the HTTP endpoint of the zero7 used during restore tests.
	SockAddrZero7Http string
	// SockAddrAlpha8 is the address to the gRPC endpoint of the alpha8 used during restore tests.
	SockAddrAlpha8 string
	// SockAddrAlpha8Http is the address to the HTTP of alpha8 used during restore tests.
	SockAddrAlpha8Http string
	// SockAddrZero8 is the address to the gRPC endpoint of the zero8 used during restore tests.
	SockAddrZero8 string
	// SockAddrZero8Http is the address to the HTTP endpoint of the zero8 used during restore tests.
	SockAddrZero8Http string
)

func AdminUrlHttps() string {
	return "https://" + SockAddrHttp + "/admin"
}

func AdminUrl() string {
	return "http://" + SockAddrHttp + "/admin"
}

// This allows running (most) tests against dgraph running on the default ports, for example.
// Only the GRPC ports are needed and the others are deduced.
func init() {
	DockerPrefix = os.Getenv("TEST_DOCKER_PREFIX")
	TestDataDirectory = os.Getenv("TEST_DATA_DIRECTORY")
	MinioInstance = ContainerAddr("minio", 9001)
	Instance = fmt.Sprintf("%s_%s_1", DockerPrefix, "alpha1")
	SockAddrLocalhost = ContainerAddrLocalhost("alpha1", 9080)
	SockAddr = ContainerAddr("alpha1", 9080)
	SockAddrHttp = ContainerAddr("alpha1", 8080)
	SockAddrHttpLocalhost = ContainerAddrLocalhost("alpha1", 8080)

	SockAddrZero = ContainerAddr("zero1", 5080)
	SockAddrZeroLocalhost = ContainerAddrLocalhost("zero1", 5080)
	SockAddrZeroHttp = ContainerAddr("zero1", 6080)

	SockAddrAlpha4 = ContainerAddr("alpha4", 9080)
	SockAddrAlpha4Http = ContainerAddr("alpha4", 8080)
	SockAddrAlpha7 = ContainerAddr("alpha7", 9080)
	SockAddrAlpha7Http = ContainerAddr("alpha7", 8080)
	SockAddrZero7 = ContainerAddr("zero7", 5080)
	SockAddrZero7Http = ContainerAddr("zero7", 6080)
	SockAddrAlpha8 = ContainerAddr("alpha8", 9080)
	SockAddrAlpha8Http = ContainerAddr("alpha8", 8080)
	SockAddrZero8 = ContainerAddr("zero8", 5080)
	SockAddrZero8Http = ContainerAddr("zero8", 6080)

	SockAddrZero4 = ContainerAddr("zero2", 5080)
	SockAddrZero4Http = ContainerAddr("zero2", 6080)

	fmt.Printf("testutil: %q %s %s\n", DockerPrefix, SockAddr, SockAddrZero)
}

// DgraphClientDropAll creates a Dgraph client and drops all existing data.
// It is intended to be called from TestMain() to establish a Dgraph connection shared
// by all tests, so there is no testing.T instance for it to use.
func DgraphClientDropAll(serviceAddr string) (*dgo.Dgraph, error) {
	dg, err := DgraphClient(serviceAddr)
	if err != nil {
		return nil, err
	}

	for {
		// keep retrying until we succeed or receive a non-retriable error
		err = dg.Alter(context.Background(), &api.Operation{DropAll: true})
		if err == nil || !strings.Contains(err.Error(), "Please retry") {
			break
		}
		time.Sleep(time.Second)
	}

	return dg, err
}

// DgraphClientWithGroot creates a Dgraph client with groot permissions set up.
// It is intended to be called from TestMain() to establish a Dgraph connection shared
// by all tests, so there is no testing.T instance for it to use.
func DgraphClientWithGroot(serviceAddr string) (*dgo.Dgraph, error) {
	dg, err := DgraphClient(serviceAddr)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	for {
		// keep retrying until we succeed or receive a non-retriable error
		err = dg.LoginIntoNamespace(ctx, x.GrootId, "password", x.GalaxyNamespace)
		if err == nil || !(strings.Contains(err.Error(), "Please retry") ||
			strings.Contains(err.Error(), "user not found")) {

			break
		}
		time.Sleep(time.Second)
	}

	return dg, err
}

// DgraphClient creates a Dgraph client.
// It is intended to be called from TestMain() to establish a Dgraph connection shared
// by all tests, so there is no testing.T instance for it to use.
func DgraphClient(serviceAddr string) (*dgo.Dgraph, error) {
	conn, err := grpc.Dial(serviceAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}

	dg := dgo.NewDgraphClient(api.NewDgraphClient(conn))
	return dg, nil
}

// DgraphClientWithCerts creates a Dgraph client with TLS configured using the given
// viper configuration.
// It is intended to be called from TestMain() to establish a Dgraph connection shared
// by all tests, so there is no testing.T instance for it to use.
func DgraphClientWithCerts(serviceAddr string, conf *viper.Viper) (*dgo.Dgraph, error) {
	tlsCfg, err := x.LoadClientTLSConfig(conf)
	if err != nil {
		return nil, err
	}

	dialOpts := []grpc.DialOption{}
	if tlsCfg != nil {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	conn, err := grpc.Dial(serviceAddr, dialOpts...)
	if err != nil {
		return nil, err
	}
	dg := dgo.NewDgraphClient(api.NewDgraphClient(conn))
	return dg, nil
}

// DropAll drops all the data in the Dgraph instance associated with the given client.
func DropAll(t *testing.T, dg *dgo.Dgraph) {
	err := dg.Alter(context.Background(), &api.Operation{DropAll: true})
	require.NoError(t, err)
}

// VerifyQueryResponse executes the given query and verifies that the response of the query is
// same as the expected response.
func VerifyQueryResponse(t *testing.T, dg *dgo.Dgraph, query, expectedResponse string) {
	resp, err := dg.NewReadOnlyTxn().Query(context.Background(), query)
	require.NoError(t, err)
	CompareJSON(t, expectedResponse, string(resp.GetJson()))
}

// RetryQuery will retry a query until it succeeds or a non-retryable error is received.
func RetryQuery(dg *dgo.Dgraph, q string) (*api.Response, error) {
	for {
		resp, err := dg.NewTxn().Query(context.Background(), q)
		if err != nil && (strings.Contains(err.Error(), "Please retry") ||
			strings.Contains(err.Error(), "connection closed") ||
			strings.Contains(err.Error(), "broken pipe")) {
			// Retry connection issues because some tests (e.g TestSnapshot) are stopping and
			// starting alphas.
			time.Sleep(10 * time.Millisecond)
			continue
		}

		return resp, err
	}
}

func RetryAlter(dg *dgo.Dgraph, op *api.Operation) error {
	var err error
	for range 10 {
		err = dg.Alter(context.Background(), op)
		if err == nil || !strings.Contains(err.Error(), "opIndexing is already running") {
			return err
		}
		time.Sleep(time.Second)
	}
	return err
}

// RetryBadQuery will retry a query until it failse with a non-retryable error.
func RetryBadQuery(dg *dgo.Dgraph, q string) (*api.Response, error) {
	for {
		txn := dg.NewTxn()
		ctx := context.Background()
		resp, err := txn.Query(ctx, q)
		if err == nil || strings.Contains(err.Error(), "Please retry") {
			time.Sleep(10 * time.Millisecond)
			_ = txn.Discard(ctx)
			continue
		}

		_ = txn.Discard(ctx)
		return resp, err
	}
}

// RetryMutation will retry a mutation until it succeeds or a non-retryable error is received.
// The mutation should have CommitNow set to true.
func RetryMutation(dg *dgo.Dgraph, mu *api.Mutation) error {
	for {
		_, err := dg.NewTxn().Mutate(context.Background(), mu)
		if err != nil && (strings.Contains(err.Error(), "Please retry") ||
			strings.Contains(err.Error(), "Tablet isn't being served by this instance") ||
			strings.Contains(err.Error(), "connection closed")) {
			// Retry connection issues because some tests (e.g TestSnapshot) are stopping and
			// starting alphas.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		return err
	}
}

// LoginParams stores the information needed to perform a login request.
type LoginParams struct {
	Endpoint   string
	UserID     string
	Passwd     string
	Namespace  uint64
	RefreshJwt string
}

// HttpLogin sends a HTTP request to the server
// and returns the access JWT and refresh JWT extracted from
// the HTTP response
func HttpLogin(params *LoginParams) (*HttpToken, error) {
	loginPayload := api.LoginRequest{}
	if len(params.RefreshJwt) > 0 {
		loginPayload.RefreshToken = params.RefreshJwt
	} else {
		loginPayload.Userid = params.UserID
		loginPayload.Password = params.Passwd
	}

	login := `mutation login($userId: String, $password: String, $namespace: Int, $refreshToken: String) {
		login(userId: $userId, password: $password, namespace: $namespace, refreshToken: $refreshToken) {
			response {
				accessJWT
				refreshJWT
			}
		}
	}`

	gqlParams := GraphQLParams{
		Query: login,
		Variables: map[string]interface{}{
			"userId":       params.UserID,
			"password":     params.Passwd,
			"namespace":    params.Namespace,
			"refreshToken": params.RefreshJwt,
		},
	}
	body, err := json.Marshal(gqlParams)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to marshal body")
	}

	req, err := http.NewRequest("POST", params.Endpoint, bytes.NewBuffer(body))
	if err != nil {
		return nil, errors.Wrapf(err, "unable to create request")
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Wrapf(err, "login through curl failed")
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			glog.Warningf("error closing body: %v", err)
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to read from response")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(fmt.Sprintf("got non 200 response from the server with %s ",
			string(respBody)))
	}

	var gqlResp GraphQLResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, err
	}

	if len(gqlResp.Errors) > 0 {
		return nil, errors.Errorf(gqlResp.Errors.Error())
	}

	if gqlResp.Data == nil {
		return nil, errors.Wrapf(err, "data entry found in the output")
	}

	type Response struct {
		Login struct {
			Response struct {
				AccessJWT  string
				RefreshJwt string
			}
		}
	}
	var r Response
	if err := json.Unmarshal(gqlResp.Data, &r); err != nil {
		return nil, err
	}

	if r.Login.Response.AccessJWT == "" {
		return nil, errors.Errorf("no access JWT found in the output")
	}
	if r.Login.Response.RefreshJwt == "" {
		return nil, errors.Errorf("no refresh JWT found in the output")
	}

	return &HttpToken{
		UserId:       params.UserID,
		Password:     params.Passwd,
		AccessJwt:    r.Login.Response.AccessJWT,
		RefreshToken: r.Login.Response.RefreshJwt,
	}, nil
}

type HttpToken struct {
	UserId       string
	Password     string
	AccessJwt    string
	RefreshToken string
}

// GrootHttpLogin logins using the groot account with the default password
// and returns the access JWT
func GrootHttpLogin(endpoint string) *HttpToken {
	return GrootHttpLoginNamespace(endpoint, 0)
}

func GrootHttpLoginNamespace(endpoint string, namespace uint64) *HttpToken {
	token, err := HttpLogin(&LoginParams{
		Endpoint:  endpoint,
		UserID:    x.GrootId,
		Passwd:    "password",
		Namespace: namespace,
	})
	x.Check(err)
	return token
}

// CurlFailureConfig stores information about the expected failure of a curl test.
type CurlFailureConfig struct {
	ShouldFail   bool
	CurlErrMsg   string
	DgraphErrMsg string
}

type curlErrorEntry struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type curlOutput struct {
	Data   map[string]interface{} `json:"data"`
	Errors []curlErrorEntry       `json:"errors"`
}

func verifyOutput(t *testing.T, bytes []byte, failureConfig *CurlFailureConfig) error {
	output := curlOutput{}
	require.NoError(t, json.Unmarshal(bytes, &output),
		"unable to unmarshal the curl output")
	for _, e := range output.Errors {
		if strings.Contains(e.Message, "is already running") ||
			strings.Contains(e.Message, "errIndexingInProgress") {
			return errRetryCurl
		}
	}

	if failureConfig.ShouldFail {
		require.True(t, len(output.Errors) > 0, "no error entry found")
		if len(failureConfig.DgraphErrMsg) > 0 {
			errorEntry := output.Errors[0]
			require.True(t, strings.Contains(errorEntry.Message, failureConfig.DgraphErrMsg),
				fmt.Sprintf("the failure msg\n%s\nis not part of the curl error output:%s\n",
					failureConfig.DgraphErrMsg, errorEntry.Message))
		}
	} else {
		require.True(t, output.Data != nil,
			fmt.Sprintf("no data entry found in the output:%+v", output))
	}
	return nil
}

var errRetryCurl = errors.New("retry the curl command")

// VerifyCurlCmd executes the curl command with the given arguments and verifies
// the result against the expected output.
func VerifyCurlCmd(t *testing.T, args []string, failureConfig *CurlFailureConfig) {
top:
	queryCmd := exec.Command("curl", args...)
	output, err := queryCmd.Output()
	if len(failureConfig.CurlErrMsg) > 0 {
		// the curl command should have returned an non-zero code
		require.Error(t, err, "the curl command should have failed")
		if ee, ok := err.(*exec.ExitError); ok {
			require.Contains(t, string(ee.Stderr), failureConfig.CurlErrMsg,
				"the curl output does not contain the expected output")
		}
	} else {
		require.NoError(t, err, "the curl command should have succeeded")
		if err := verifyOutput(t, output, failureConfig); err == errRetryCurl {
			goto top
		} else {
			require.NoError(t, err)
		}
	}
}

// AssignUids talks to zero to assign the given number of uids.
func AssignUids(num uint64) error {
	resp, err := http.Get(fmt.Sprintf("http://"+SockAddrZeroHttp+"/assign?what=uids&num=%d", num))
	type assignResp struct {
		Errors []struct {
			Message string
			Code    string
		}
	}
	var data assignResp
	if err == nil && resp != nil && resp.Body != nil {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if err := resp.Body.Close(); err != nil {
			return err
		}
		if err := json.Unmarshal(body, &data); err != nil {
			return err
		}
		if len(data.Errors) > 0 {
			return errors.New(data.Errors[0].Message)
		}
	}
	return err
}

func RequireUid(t *testing.T, uid string) {
	_, err := dql.ParseUid(uid)
	require.NoErrorf(t, err, "expecting a uid, got: %s", uid)
}

func CheckForGraphQLEndpointToReady(t *testing.T) error {
	var err error
	retries := 6
	sleep := 10 * time.Second

	// Because of how GraphQL starts (it needs to read the schema from Dgraph),
	// there's no guarantee that GraphQL is available by now.  So we
	// need to try and connect and potentially retry a few times.
	for retries > 0 {
		retries--

		_, err = hasAdminGraphQLSchema(t)
		if err == nil {
			return nil
		}
		time.Sleep(sleep)
	}
	return err
}

func hasAdminGraphQLSchema(t *testing.T) (bool, error) {
	schemaQry := &GraphQLParams{
		Query: `query { getGQLSchema { schema } }`,
	}

	result := MakeGQLRequest(t, schemaQry)
	if len(result.Errors) > 0 {
		return false, result.Errors
	}
	var sch struct {
		GetGQLSchema struct {
			Schema string
		}
	}

	err := json.Unmarshal(result.Data, &sch)
	if err != nil {
		return false, errors.Wrap(err, "error trying to unmarshal GraphQL query result")
	}

	if sch.GetGQLSchema.Schema == "" {
		return false, nil
	}

	return true, nil
}

func GetHttpsClient(t *testing.T) http.Client {
	tlsConf := GetAlphaClientConfig(t)
	return http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConf,
		},
	}
}

func GetAlphaClientConfig(t *testing.T) *tls.Config {
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	tlsDir := filepath.Join(filepath.Dir(filename), "../tlstest/mtls_internal/tls/live")
	c := &x.TLSHelperConfig{
		CertRequired:     true,
		Cert:             tlsDir + "/client.liveclient.crt",
		Key:              tlsDir + "/client.liveclient.key",
		ServerName:       "alpha1",
		RootCACert:       tlsDir + "/ca.crt",
		UseSystemCACerts: true,
	}
	tlsConf, err := x.GenerateClientTLSConfig(c)
	require.NoError(t, err)
	return tlsConf
}
