/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dgraphapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/pkg/errors"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/graphql/schema"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/x"
)

const (
	localVersion    = "local"
	DefaultUser     = "groot"
	DefaultPassword = "password"
)

type Cluster interface {
	Client() (*GrpcClient, func(), error)
	HTTPClient() (*HTTPClient, error)
	AlphasHealth() ([]string, error)
	AlphasLogs() ([]string, error)
	AssignUids(gc *dgo.Dgraph, num uint64) error
	GetVersion() string
	GetEncKeyPath() (string, error)
	GetRepoDir() (string, error)
}

type GrpcClient struct {
	*dgo.Dgraph
}

// HttpToken stores credentials for making HTTP request
type HttpToken struct {
	UserId       string
	Password     string
	AccessJwt    string
	RefreshToken string
}

// HTTPClient allows doing operations on Dgraph over http
type HTTPClient struct {
	*HttpToken
	adminURL      string
	graphqlURL    string
	licenseURL    string
	stateURL      string
	dqlURL        string
	dqlMutateUrl  string
	alphaStateURL string
}

// GraphQLParams are used for making graphql requests to dgraph
type GraphQLParams struct {
	Query      string                    `json:"query"`
	Variables  map[string]interface{}    `json:"variables"`
	Extensions *schema.RequestExtensions `json:"extensions,omitempty"`
}

type GraphQLResponse struct {
	Data       json.RawMessage        `json:"data,omitempty"`
	Errors     x.GqlErrorList         `json:"errors,omitempty"`
	Code       string                 `json:"code"`
	Extensions map[string]interface{} `json:"extensions,omitempty"`
}

type LicenseResponse struct {
	Data       json.RawMessage        `json:"data,omitempty"`
	Errors     x.GqlErrorList         `json:"errors,omitempty"`
	Code       string                 `json:"code"`
	Extensions map[string]interface{} `json:"license,omitempty"`
}

var client *http.Client = &http.Client{
	Timeout: requestTimeout,
}

func DoReq(req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "error performing HTTP request")
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("[WARNING] error closing response body: %v", err)
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "error reading response body: url: [%v], err: [%v]", req.URL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("got non 200 resp: %v", string(respBody))
	}
	return respBody, nil
}

func (hc *HTTPClient) LoginUsingToken(ns uint64) error {
	q := `mutation login( $namespace: Int, $refreshToken:String) {
 		login(namespace: $namespace, refreshToken: $refreshToken) {
 			response {
 				accessJWT
 				refreshJWT
 			}
 		}
 	}`
	params := GraphQLParams{
		Query: q,
		Variables: map[string]interface{}{
			"namespace":    ns,
			"refreshToken": hc.RefreshToken,
		},
	}

	return hc.doLogin(params, true)
}

func (hc *HTTPClient) LoginIntoNamespace(user, password string, ns uint64) error {
	// This is useful for v20.11 which does not support multi-tenancy
	if ns == 0 {
		return hc.LoginIntoNamespaceV20(user, password)
	}

	q := `mutation login($userId: String, $password: String, $namespace: Int) {
		login(userId: $userId, password: $password, namespace: $namespace) {
			response {
				accessJWT
				refreshJWT
			}
		}
	}`
	params := GraphQLParams{
		Query: q,
		Variables: map[string]interface{}{
			"userId":    user,
			"password":  password,
			"namespace": ns,
		},
	}

	hc.HttpToken = &HttpToken{
		UserId:   user,
		Password: password,
	}
	return hc.doLogin(params, true)
}

func (hc *HTTPClient) LoginIntoNamespaceV20(user, password string) error {
	q := `mutation login($userId: String, $password: String) {
		login(userId: $userId, password: $password) {
			response {
				accessJWT
				refreshJWT
			}
		}
	}`
	params := GraphQLParams{
		Query: q,
		Variables: map[string]interface{}{
			"userId":   user,
			"password": password,
		},
	}

	hc.HttpToken = &HttpToken{
		UserId:   user,
		Password: password,
	}
	return hc.doLogin(params, true)
}

func (hc *HTTPClient) doLogin(params GraphQLParams, isAdmin bool) error {
	resp, err := hc.RunGraphqlQuery(params, isAdmin)
	if err != nil {
		return err
	}
	var r struct {
		Login struct {
			Response struct {
				AccessJWT  string
				RefreshJwt string
			}
		}
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		return errors.Wrap(err, "error unmarshalling response into object")
	}
	if r.Login.Response.AccessJWT == "" {
		return errors.Errorf("no access JWT found in the response")
	}

	hc.HttpToken.AccessJwt = r.Login.Response.AccessJWT
	hc.HttpToken.RefreshToken = r.Login.Response.RefreshJwt
	return nil
}

func (hc *HTTPClient) ResetPassword(userID, newPass string, nsID uint64) (string, error) {
	const query = `mutation resetPassword($userID: String!, $newpass: String!, $namespaceId: Int!){
		resetPassword(input: {userId: $userID, password: $newpass, namespace: $namespaceId}) {
			userId
			message
		}
	}`
	params := GraphQLParams{
		Query: query,
		Variables: map[string]interface{}{
			"namespaceId": nsID,
			"userID":      userID,
			"newpass":     newPass,
		},
	}
	resp, err := hc.RunGraphqlQuery(params, true)
	if err != nil {
		return "", err
	}

	var result struct {
		ResetPassword struct {
			UserId  string `json:"userId"`
			Message string `json:"message"`
		}
	}
	if err = json.Unmarshal(resp, &result); err != nil {
		return "", errors.Wrap(err, "error unmarshalling response")
	}
	if userID != result.ResetPassword.UserId {
		return "", fmt.Errorf("unexpected error while resetting password for user [%s]", userID)
	}
	if result.ResetPassword.Message == "Reset password is successful" {
		return result.ResetPassword.UserId, nil
	}
	return "", errors.New(result.ResetPassword.Message)
}

// doPost makes a post request to the 'url' endpoint
func (hc *HTTPClient) doPost(body []byte, url string, contentType string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, errors.Wrapf(err, "error building req for endpoint [%v]", url)
	}
	req.Header.Add("Content-Type", contentType)

	if hc.HttpToken != nil {
		req.Header.Add("X-Dgraph-AccessToken", hc.AccessJwt)
	}

	return DoReq(req)
}

// RunGraphqlQuery makes a query to graphql (or admin) endpoint
func (hc *HTTPClient) RunGraphqlQuery(params GraphQLParams, admin bool) ([]byte, error) {
	reqBody, err := json.Marshal(params)
	if err != nil {
		return nil, errors.Wrap(err, "error while marshalling params")
	}

	url := hc.graphqlURL
	if admin {
		url = hc.adminURL
	}

	respBody, err := hc.doPost(reqBody, url, "application/json")
	if err != nil {
		return nil, errors.Wrap(err, "error while running graphql query")
	}

	var gqlResp GraphQLResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, errors.Wrap(err, "error unmarshalling GQL response")
	}
	if len(gqlResp.Errors) > 0 {
		return nil, errors.Wrapf(gqlResp.Errors, "error while running graphql query, resp: %v",
			string(gqlResp.Data))
	}
	return gqlResp.Data, nil
}

func (hc *HTTPClient) HealthForInstance() ([]byte, error) {
	const query = `query {
		health {
			instance
			address
			lastEcho
			status
			version
			uptime
			group
		}
	}`
	params := GraphQLParams{Query: query}
	return hc.RunGraphqlQuery(params, true)
}

// Backup creates a backup of dgraph at a given path
func (hc *HTTPClient) Backup(c Cluster, forceFull bool, backupPath string) error {
	repoDir, err := c.GetRepoDir()
	if err != nil {
		return errors.Wrapf(err, "error getting repo directory")
	}

	// backup API was made async in the commit d3bf7b7b2786bcb99f02e1641f3b656d0a98f7f4
	asyncAPI, err := IsHigherVersion(c.GetVersion(), "d3bf7b7b2786bcb99f02e1641f3b656d0a98f7f4", repoDir)
	if err != nil {
		return errors.Wrapf(err, "error checking incremental restore support")
	}

	var taskPart string
	if asyncAPI {
		taskPart = "taskId"
	}

	const queryFmt = `mutation backup($dest: String!, $ff: Boolean!) {
		backup(input: {destination: $dest, forceFull: $ff}) {
			response {
				code
			}
			%v
		}
	}`
	params := GraphQLParams{
		Query:     fmt.Sprintf(queryFmt, taskPart),
		Variables: map[string]interface{}{"dest": backupPath, "ff": forceFull},
	}
	resp, err := hc.RunGraphqlQuery(params, true)
	if err != nil {
		return err
	}

	var backupResp struct {
		Backup struct {
			Response struct {
				Code string `json:"code,omitempty"`
			} `json:"response,omitempty"`
			TaskID string `json:"taskId,omitempty"`
		} `json:"backup,omitempty"`
	}
	if err := json.Unmarshal(resp, &backupResp); err != nil {
		return errors.Wrap(err, "error unmarshalling backup response")
	}
	if backupResp.Backup.Response.Code != "Success" {
		return fmt.Errorf("backup failed")
	}

	return hc.WaitForTask(backupResp.Backup.TaskID)
}

// WaitForTask waits for the task to finish
func (hc *HTTPClient) WaitForTask(taskId string) error {
	// This can happen if the backup API is a sync API
	if taskId == "" {
		return nil
	}

	const query = `query task($id: String!) {
		task(input: {id: $id}) {
			status
		}
	}`
	params := GraphQLParams{
		Query:     query,
		Variables: map[string]interface{}{"id": taskId},
	}

	for {
		time.Sleep(waitDurBeforeRetry)

		resp, err := hc.RunGraphqlQuery(params, true)
		if err != nil {
			return err
		}

		var statusResp struct {
			Task struct {
				Status string `json:"status,omitempty"`
			} `json:"task,omitempty"`
		}
		if err := json.Unmarshal(resp, &statusResp); err != nil {
			return errors.Wrap(err, "error unmarshalling status response")
		}
		switch statusResp.Task.Status {
		case "Success":
			return nil
		case "Failed", "Unknown":
			return fmt.Errorf("task failed with status: %s", statusResp.Task.Status)
		}
	}
}

// Restore performs restore on Dgraph cluster from the given path to backup
func (hc *HTTPClient) Restore(c Cluster, backupPath string,
	backupId string, incrFrom, backupNum int) error {
	repoDir, err := c.GetRepoDir()
	if err != nil {
		return errors.Wrapf(err, "error getting repo directory")
	}

	// incremental restore was introduced in commit 8b3712e93ed2435bea52d957f7b69976c6cfc55b
	incrRestoreSupported, err := IsHigherVersion(c.GetVersion(), "8b3712e93ed2435bea52d957f7b69976c6cfc55b", repoDir)
	if err != nil {
		return errors.Wrapf(err, "error checking incremental restore support")
	}
	if !incrRestoreSupported && incrFrom != 0 {
		return errors.New("incremental restore is not supported by the cluster")
	}

	encKey, err := c.GetEncKeyPath()
	if err != nil {
		return errors.Wrapf(err, "error getting encryption key path")
	}

	var varPart, queryPart string
	if incrRestoreSupported {
		varPart = "$incrFrom: Int, "
		queryPart = " incrementalFrom: $incrFrom,"
	}
	query := fmt.Sprintf(`mutation restore($location: String!, $backupId: String,
			%v$backupNum: Int, $encKey: String) {
		restore(input: {location: $location, backupId: $backupId,%v
				backupNum: $backupNum, encryptionKeyFile: $encKey}) {
			code
			message
		}
	}`, varPart, queryPart)
	vars := map[string]interface{}{"location": backupPath, "backupId": backupId,
		"backupNum": backupNum, "encKey": encKey}
	if incrRestoreSupported {
		vars["incrFrom"] = incrFrom
	}

	params := GraphQLParams{
		Query:     query,
		Variables: vars,
	}
	resp, err := hc.RunGraphqlQuery(params, true)
	if err != nil {
		return err
	}

	var restoreResp struct {
		Restore struct {
			Code    string
			Message string
		}
	}
	if err := json.Unmarshal(resp, &restoreResp); err != nil {
		return errors.Wrap(err, "error unmarshalling restore response")
	}
	if restoreResp.Restore.Code != "Success" {
		return fmt.Errorf("restore failed, response: %+v", restoreResp.Restore)
	}
	return nil
}

// RestoreTenant restore specific namespace
func (hc *HTTPClient) RestoreTenant(c Cluster, backupPath string, backupId string,
	incrFrom, backupNum int, fromNamespace uint64) error {

	encKey, err := c.GetEncKeyPath()
	if err != nil {
		return errors.Wrapf(err, "error getting encryption key path")
	}

	query := `mutation restoreTenant( $location: String!, $backupId: String,
		$incrFrom: Int, $backupNum: Int, $encKey: String,$fromNamespace: Int! ) {
				restoreTenant(input: {restoreInput: { location: $location, backupId: $backupId, 
				incrementalFrom: $incrFrom, backupNum: $backupNum,
			encryptionKeyFile: $encKey },fromNamespace:$fromNamespace}) {
			code
			message
		}
	}`
	vars := map[string]interface{}{"location": backupPath, "backupId": backupId, "backupNum": backupNum,
		"encKey": encKey, "fromNamespace": fromNamespace, "incrFrom": incrFrom}

	params := GraphQLParams{
		Query:     query,
		Variables: vars,
	}
	resp, err := hc.RunGraphqlQuery(params, true)
	if err != nil {
		return err
	}

	var restoreResp struct {
		RestoreTenant struct {
			Code    string
			Message string
		}
	}
	if err := json.Unmarshal(resp, &restoreResp); err != nil {
		return errors.Wrap(err, "error unmarshalling restore response")
	}
	if restoreResp.RestoreTenant.Code != "Success" {
		return fmt.Errorf("restoreTenant failed, response: %+v", restoreResp.RestoreTenant)
	}
	return nil
}

// WaitForRestore waits for restore to complete on all alphas
func WaitForRestore(c Cluster) error {
loop:
	for {
		time.Sleep(waitDurBeforeRetry)

		resp, err := c.AlphasHealth()
		if err != nil && strings.Contains(err.Error(), "the server is in draining mode") {
			continue loop
		} else if err != nil {
			return err
		}
		for _, hr := range resp {
			if strings.Contains(hr, "opRestore") {
				continue loop
			}
		}
		break
	}

	// sometimes it is possible that one alpha hasn't even started the restore process.
	// In this case, the alpha will not show "opRestore" in the response to health endpoint
	// and the "loop" will not be able to detect this case. The "loop2" below ensures
	// by looking into the logs that the restore process has in fact been started and completed.
loop2:
	for range 60 {
		time.Sleep(waitDurBeforeRetry)

		alphasLogs, err := c.AlphasLogs()
		if err != nil {
			return err
		}

		for _, alphaLogs := range alphasLogs {
			// It is possible that the alpha didn't do a restore but streamed snapshot from any other alpha
			if !strings.Contains(alphaLogs, "Operation completed with id: opRestore") &&
				!strings.Contains(alphaLogs, "Operation completed with id: opSnapshot") {

				continue loop2
			}
		}
		return nil
	}

	return errors.Errorf("restore wasn't started on at least 1 alpha")
}

func (hc *HTTPClient) Export(dest, format string, namespace int) error {
	const exportRequest = `mutation export($dest: String!, $f: String!, $ns: Int) {
		export(input: {destination: $dest, format: $f, namespace: $ns}) {
			response {
				message
			}
		}
	}`
	params := GraphQLParams{
		Query: exportRequest,
		Variables: map[string]interface{}{
			"dest": dest,
			"f":    format,
			"ns":   namespace,
		},
	}

	resp, err := hc.RunGraphqlQuery(params, true)
	if err != nil {
		return err
	}
	var r struct {
		Export struct {
			Response struct {
				Message string
			}
		}
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		return errors.Wrap(err, "error unmarshalling export response")
	}
	return nil
}

// UpdateGQLSchema updates graphql schema for a given HTTP client
func (hc *HTTPClient) UpdateGQLSchema(sch string) error {
	const query = `mutation updateGQLSchema($sch: String!) {
		updateGQLSchema(input: { set: { schema: $sch }}) {
			gqlSchema {
				id
			}
		}
	}`
	params := GraphQLParams{
		Query: query,
		Variables: map[string]interface{}{
			"sch": sch,
		},
	}
	if _, err := hc.RunGraphqlQuery(params, true); err != nil {
		return err
	}
	return nil
}

// PostPersistentQuery stores a persisted query
func (hc *HTTPClient) PostPersistentQuery(query, sha string) ([]byte, error) {
	params := GraphQLParams{
		Query: query,
		Extensions: &schema.RequestExtensions{PersistedQuery: schema.PersistedQuery{
			Sha256Hash: sha,
		}},
	}
	return hc.RunGraphqlQuery(params, false)
}

// Apply license using http endpoint
func (hc *HTTPClient) ApplyLicenseHTTP(licenseKey []byte) (*LicenseResponse, error) {
	respBody, err := hc.doPost(licenseKey, hc.licenseURL, "application/text")
	if err != nil {
		return nil, errors.Wrap(err, "error applying license")
	}
	var enterpriseResponse LicenseResponse
	if err = json.Unmarshal(respBody, &enterpriseResponse); err != nil {
		return nil, errors.Wrap(err, "error unmarshaling the license response")
	}

	return &enterpriseResponse, nil
}

// Apply license using graphql endpoint
func (hc *HTTPClient) ApplyLicenseGraphQL(license []byte) ([]byte, error) {
	params := GraphQLParams{
		Query: `mutation ($license: String!) {
			enterpriseLicense(input: {license: $license}) {
				response {
					code
				}
			}
		}`,
		Variables: map[string]interface{}{
			"license": string(license),
		},
	}
	return hc.RunGraphqlQuery(params, true)
}

func (hc *HTTPClient) GetLicenseInfo() (*LicenseResponse, error) {
	response, err := http.Get(hc.stateURL)
	if err != nil {
		return nil, errors.Wrap(err, "error getting zero state http response")
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			log.Printf("[WARNING] error closing body: %v", err)
		}
	}()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "error reading zero state response body")
	}
	var stateResponse LicenseResponse
	if err := json.Unmarshal(body, &stateResponse); err != nil {
		return nil, errors.Wrapf(err, "error unmarshaling zero state response")
	}

	return &stateResponse, nil
}

func (hc *HTTPClient) GetAlphaState() (*pb.MembershipState, error) {
	req, err := http.NewRequest(http.MethodGet, hc.alphaStateURL, nil)
	if err != nil {
		return nil, err
	}
	if hc.HttpToken != nil {
		req.Header.Add("X-Dgraph-AccessToken", hc.AccessJwt)
	}

	resp, err := DoReq(req)
	if err != nil {
		return nil, err
	}

	var state pb.MembershipState
	if err = protojson.Unmarshal(resp, &state); err != nil {
		return nil, err
	}
	return &state, err
}

func (hc *HTTPClient) PostDqlQuery(query string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, hc.dqlURL, bytes.NewBufferString(query))
	if err != nil {
		return nil, errors.Wrapf(err, "error building req for endpoint [%v]", hc.dqlURL)
	}
	req.Header.Add("Content-Type", "application/dql")
	if hc.HttpToken != nil {
		req.Header.Add("X-Dgraph-AccessToken", hc.AccessJwt)
	}
	return DoReq(req)
}

func (hc *HTTPClient) Mutate(mutation string, commitNow bool) ([]byte, error) {
	url := hc.dqlMutateUrl
	if commitNow {
		url = hc.dqlMutateUrl + "?commitNow=true"
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(mutation))
	if err != nil {
		return nil, errors.Wrapf(err, "error building req for endpoint [%v]", url)
	}
	req.Header.Add("Content-Type", "application/rdf")
	if hc.HttpToken != nil {
		req.Header.Add("X-Dgraph-AccessToken", hc.AccessJwt)
	}
	return DoReq(req)
}

// SetupSchema sets up DQL schema
func (gc *GrpcClient) SetupSchema(dbSchema string) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	return gc.Alter(ctx, &api.Operation{Schema: dbSchema})
}

// DropAll drops all the data in the db
func (gc *GrpcClient) DropAll() error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	return gc.Alter(ctx, &api.Operation{DropAll: true})
}

// DropPredicate drops the predicate from the data in the db
func (gc *GrpcClient) DropPredicate(pred string) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	return gc.Alter(ctx, &api.Operation{DropAttr: pred})
}

// Mutate performs a given mutation in a txn
func (gc *GrpcClient) Mutate(mu *api.Mutation) (*api.Response, error) {
	txn := gc.NewTxn()
	defer func() { _ = txn.Discard(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	return txn.Mutate(ctx, mu)
}

func (gc *GrpcClient) Upsert(query string, mu *api.Mutation) (*api.Response, error) {
	txn := gc.NewTxn()
	defer func() { _ = txn.Discard(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}
	return txn.Do(ctx, req)
}

// Query performa a given query in a new txn
func (gc *GrpcClient) Query(query string) (*api.Response, error) {
	txn := gc.NewTxn()
	defer func() { _ = txn.Discard(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	return txn.Query(ctx, query)
}

// IsHigherVersion checks whether "higher" is the higher version compared to "lower"
func IsHigherVersion(higher, lower, repoDir string) (bool, error) {
	// the order of if conditions matters here
	if lower == localVersion {
		return false, nil
	}
	if higher == localVersion {
		return true, nil
	}

	// An older commit is usually the ancestor of a newer commit which is a descendant commit
	cmd := exec.Command("git", "merge-base", "--is-ancestor", lower, higher)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			return exitError.ExitCode() == 0, nil
		}

		return false, errors.Wrapf(err, "error checking if [%v] is ancestor of [%v]\noutput:%v",
			higher, lower, string(out))
	}

	return true, nil
}

func GetHttpClient(alphaUrl, zeroUrl string) (*HTTPClient, error) {
	adminUrl := "http://" + alphaUrl + "/admin"
	graphQLUrl := "http://" + alphaUrl + "/graphql"
	licenseUrl := "http://" + zeroUrl + "/enterpriseLicense"
	stateUrl := "http://" + zeroUrl + "/state"
	dqlUrl := "http://" + alphaUrl + "/query"
	dqlMutateUrl := "http://" + alphaUrl + "/mutate"
	alphaStateUrl := "http://" + alphaUrl + "/state"
	return &HTTPClient{
		adminURL:      adminUrl,
		graphqlURL:    graphQLUrl,
		licenseURL:    licenseUrl,
		stateURL:      stateUrl,
		dqlURL:        dqlUrl,
		dqlMutateUrl:  dqlMutateUrl,
		alphaStateURL: alphaStateUrl,
		HttpToken:     &HttpToken{},
	}, nil
}
