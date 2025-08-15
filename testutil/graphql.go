/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

//nolint:lll
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
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	"github.com/hypermodeinc/dgraph/v25/x"
)

const ExportRequest = `mutation {
	export(input: {format: "json"}) {
		response {
			code
			message
		}
	}
}`

type GraphQLParams struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

type GraphQLError struct {
	Message string
}

type GraphQLResponse struct {
	Data       json.RawMessage        `json:"data,omitempty"`
	Errors     x.GqlErrorList         `json:"errors,omitempty"`
	Extensions map[string]interface{} `json:"extensions,omitempty"`
}

func (resp *GraphQLResponse) RequireNoGraphQLErrors(t *testing.T) {
	if resp == nil {
		require.Fail(t, "got nil response")
	} else {
		require.Nil(t, resp.Errors, "required no GraphQL errors, but received :\n%s",
			resp.Errors.Error())
	}
}

func RequireNoGraphQLErrors(t *testing.T, resp *http.Response) {
	defer func() {
		if err := resp.Body.Close(); err != nil {
			glog.Warningf("error closing body: %v", err)
		}
	}()

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var result *GraphQLResponse
	err = json.Unmarshal(b, &result)
	require.NoError(t, err)
	require.Nil(t, result.Errors, "Recieved GQL Errors: %+v", result.Errors)
}

func MakeGQLRequest(t *testing.T, params *GraphQLParams) *GraphQLResponse {
	return MakeGQLRequestWithAccessJwt(t, params, "")
}

func MakeGQLRequestWithTLS(t *testing.T, params *GraphQLParams, tls *tls.Config) *GraphQLResponse {
	return MakeGQLRequestWithAccessJwtAndTLS(t, params, tls, "")
}

func MakeGQLRequestWithAccessJwt(t *testing.T, params *GraphQLParams, accessToken string) *GraphQLResponse {
	return MakeGQLRequestWithAccessJwtAndTLS(t, params, nil, accessToken)
}

func MakeGQLRequestWithAccessJwtAndTLS(t *testing.T, params *GraphQLParams,
	tls *tls.Config, accessToken string) *GraphQLResponse {

	ensureAddressesInitialized()
	var adminUrl string
	if tls != nil {
		adminUrl = "https://" + GetSockAddrHttp() + "/admin"
	} else {
		adminUrl = "http://" + GetSockAddrHttp() + "/admin"
	}

	b, err := json.Marshal(params)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, adminUrl, bytes.NewBuffer(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if accessToken != "" {
		req.Header.Set("X-Dgraph-AccessToken", accessToken)
	}
	client := &http.Client{}
	if tls != nil {
		client.Transport = &http.Transport{
			TLSClientConfig: tls,
		}
	}
	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() {
		if err := resp.Body.Close(); err != nil {
			glog.Warningf("error closing body: %v", err)
		}
	}()

	b, err = io.ReadAll(resp.Body)
	require.NoError(t, err)

	var gqlResp GraphQLResponse
	err = json.Unmarshal(b, &gqlResp)
	require.NoError(t, err)

	return &gqlResp
}

func AssertRemoveNode(t *testing.T, nodeId uint64, groupId uint32) {
	params := &GraphQLParams{
		Query: `mutation ($nodeId: UInt64!, $groupId: UInt64!) {
		  removeNode(input: {nodeId: $nodeId, groupId: $groupId}) {
			response {
				code
			}
		  }
		}`,
		Variables: map[string]interface{}{
			"nodeId":  nodeId,
			"groupId": groupId,
		},
	}
	resp := MakeGQLRequest(t, params)
	resp.RequireNoGraphQLErrors(t)
	CompareJSON(t, `{"removeNode":{"response":{"code":"Success"}}}`, string(resp.Data))
}

func AssertMoveTablet(t *testing.T, tablet string, groupId uint32) {
	params := &GraphQLParams{
		Query: `mutation ($tablet: String!, $groupId: UInt64!) {
		  moveTablet(input: {tablet: $tablet, groupId: $groupId}) {
			response {
				code
			}
		  }
		}`,
		Variables: map[string]interface{}{
			"tablet":  tablet,
			"groupId": groupId,
		},
	}
	resp := MakeGQLRequest(t, params)
	resp.RequireNoGraphQLErrors(t)
	CompareJSON(t, `{"moveTablet":{"response":{"code":"Success"}}}`, string(resp.Data))
}

type clientCustomClaims struct {
	Namespace     string
	AuthVariables map[string]interface{}
	jwt.RegisteredClaims
}

func (c clientCustomClaims) MarshalJSON() ([]byte, error) {
	// Encode the original
	m, err := json.Marshal(c.RegisteredClaims)
	if err != nil {
		return nil, err
	}

	// Decode it back to get a map
	var a interface{}
	err = json.Unmarshal(m, &a)
	if err != nil {
		return nil, err
	}

	b, ok := a.(map[string]interface{})
	if !ok {
		return nil, errors.Errorf("error while marshalling custom claim json.")
	}

	// Set the proper namespace and delete additional data.
	b[c.Namespace] = c.AuthVariables
	delete(b, "AuthVariables")
	delete(b, "Namespace")

	// Return encoding of the map
	return json.Marshal(b)
}

type AuthMeta struct {
	PublicKey       string
	Namespace       string
	Algo            string
	Header          string
	AuthVars        map[string]interface{}
	PrivateKeyPath  string
	ClosedByDefault bool
}

func (a *AuthMeta) GetSignedToken(privateKeyFile string,
	expireAfter time.Duration) (string, error) {
	claims := clientCustomClaims{
		a.Namespace,
		a.AuthVars,
		jwt.RegisteredClaims{
			Issuer: "test",
		},
	}
	if expireAfter != -1 {
		claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(expireAfter))
	}

	var signedString string
	var err error
	if a.Algo == "HS256" {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		signedString, err = token.SignedString([]byte(a.PublicKey))
		return signedString, err
	}
	if a.Algo != "RS256" {
		return signedString, err

	}
	keyData, err := os.ReadFile(privateKeyFile)
	if err != nil {
		return signedString, errors.Errorf("unable to read private key file: %v", err)
	}

	privateKey, err := jwt.ParseRSAPrivateKeyFromPEM(keyData)
	if err != nil {
		return signedString, errors.Errorf("unable to parse private key: %v", err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signedString, err = token.SignedString(privateKey)
	return signedString, err
}

func (a *AuthMeta) AddClaimsToContext(ctx context.Context) (context.Context, error) {
	token, err := a.GetSignedToken("../e2e/auth/sample_private_key.pem", 5*time.Minute)
	if err != nil {
		return ctx, err
	}

	md := metadata.New(nil)
	md.Append("authorizationJwt", token)
	return metadata.NewIncomingContext(ctx, md), nil
}

func AppendAuthInfo(schema []byte, algo, publicKeyFile string, closedByDefault bool) ([]byte, error) {
	authInfo := `# Dgraph.Authorization {"VerificationKey":"%s","Header":"X-Test-Auth","Namespace":"https://xyz.io/jwt/claims","Algo":"%s","Audience":["aud1","63do0q16n6ebjgkumu05kkeian","aud5"],"ClosedByDefault":%s}`

	closedByDefaultStr := "false"
	if closedByDefault {
		closedByDefaultStr = "true"
	}

	var verificationKey string
	switch algo {
	case "HS256":
		verificationKey = "secretkey"
	case "RS256":
		keyData, err := os.ReadFile(publicKeyFile)
		if err != nil {
			return nil, err
		}
		// Replacing ASCII newline with "\n" as the authorization information in the schema
		// should be present in a single line.
		verificationKey = string(bytes.ReplaceAll(keyData, []byte{10}, []byte{92, 110}))
	}

	authInfo = fmt.Sprintf(authInfo, verificationKey, algo, closedByDefaultStr)
	return append(schema, []byte(authInfo)...), nil
}

func AppendAuthInfoWithJWKUrl(schema []byte) ([]byte, error) {
	authInfo := `#   Dgraph.Authorization {"VerificationKey":"","Header":"X-Test-Auth","jwkurl":"https://dev-hr2kugfp.us.auth0.com/.well-known/jwks.json", "Namespace":"https://xyz.io/jwt/claims","Algo":"","Audience":[ "HhaXkQVRBn5e0K3DmMp2zbjI8i1wcv2e"]}`
	return append(schema, []byte(authInfo)...), nil
}

func AppendAuthInfoWithMultipleJWKUrls(schema []byte) ([]byte, error) {
	authInfo := `#   Dgraph.Authorization {"VerificationKey":"","Header":"X-Test-Auth","jwkurls":["https://www.googleapis.com/service_accounts/v1/jwk/securetoken@system.gserviceaccount.com","https://dev-hr2kugfp.us.auth0.com/.well-known/jwks.json"], "Namespace":"https://xyz.io/jwt/claims","Algo":"","Audience":["fir-project1-259e7", "HhaXkQVRBn5e0K3DmMp2zbjI8i1wcv2e"]}`
	return append(schema, []byte(authInfo)...), nil
}

func AppendAuthInfoWithJWKUrlAndWithoutAudience(schema []byte) ([]byte, error) {
	authInfo := `# Dgraph.Authorization {"VerificationKey":"","Header":"X-Test-Auth","jwkurl":"https://www.googleapis.com/service_accounts/v1/jwk/securetoken@system.gserviceaccount.com", "Namespace":"https://xyz.io/jwt/claims","Algo":"","Audience":[]}`
	return append(schema, []byte(authInfo)...), nil
}

// Add JWKUrl and (VerificationKey, Algo) in the same Authorization JSON
// Adding Dummy values as this should result in validation error
func AppendJWKAndVerificationKey(schema []byte) ([]byte, error) {
	authInfo := `#Dgraph.Authorization{"VerificationKey":"some-key","Header":"X-Test-Auth","jwkurl":"some-url", "Namespace":"https://xyz.io/jwt/claims","Algo":"algo","Audience":["fir-project1-259e7"]}`
	return append(schema, []byte(authInfo)...), nil
}
