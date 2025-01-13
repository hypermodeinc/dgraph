/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package x

import (
	"crypto/tls"
	"net"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/viper"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/ristretto/v2/z"
)

// Options stores the options for this package.
type Options struct {
	// PortOffset will be used to determine the ports to use (port = default port + offset).
	PortOffset int
	// Limit options:
	//
	// query-edge uint64 - maximum number of edges that can be returned in a query
	// normalize-node int - maximum number of nodes that can be returned in a query that uses the
	//                      normalize directive
	// mutations-nquad int - maximum number of nquads that can be inserted in a mutation request
	// BlockDropAll bool - if set to true, the drop all operation will be rejected by the server.
	// query-timeout duration - Maximum time after which a query execution will fail.
	// max-retries int64 - maximum number of retries made by dgraph to commit a transaction to disk.
	// shared-instance bool - if set to true, ACLs will be disabled for non-galaxy users.
	Limit                *z.SuperFlag
	LimitMutationsNquad  int
	LimitQueryEdge       uint64
	BlockClusterWideDrop bool
	LimitNormalizeNode   int
	QueryTimeout         time.Duration
	MaxRetries           int64
	SharedInstance       bool

	// GraphQL options:
	//
	// extensions bool - Will be set to see extensions in GraphQL results
	// debug bool - Will enable debug mode in GraphQL.
	// lambda-url string - Stores the URL of lambda functions for custom GraphQL resolvers
	// 			The configured lambda-url can have a parameter `$ns`,
	//			which should be replaced with the correct namespace value at runtime.
	// 	===========================================================================================
	// 	|                lambda-url                | $ns |           namespacedLambdaUrl          |
	// 	|==========================================|=====|========================================|
	// 	| http://localhost:8686/graphql-worker/$ns |  1  | http://localhost:8686/graphql-worker/1 |
	// 	| http://localhost:8686/graphql-worker     |  1  | http://localhost:8686/graphql-worker   |
	// 	|=========================================================================================|
	//
	// poll-interval duration - The polling interval for graphql subscription.
	GraphQL      *z.SuperFlag
	GraphQLDebug bool

	// feature flags
	NormalizeCompatibilityMode string
}

// Config stores the global instance of this package's options.
var Config Options

// IPRange represents an IP range.
type IPRange struct {
	Lower, Upper net.IP
}

// WorkerOptions stores the options for the worker package. It's declared here
// since it's used by multiple packages.
type WorkerOptions struct {
	// TmpDir is a directory to store temporary buffers.
	TmpDir string
	// ExportPath indicates the folder to which exported data will be saved.
	ExportPath string
	// Trace options:
	//
	// ratio float64 - the ratio of queries to trace (must be between 0 and 1)
	// jaeger string - URL of Jaeger to send OpenCensus traces
	// datadog string - URL of Datadog to send OpenCensus traces
	Trace *z.SuperFlag
	// MyAddr stores the address and port for this alpha.
	MyAddr string
	// MyGrpcAddr stores public grpc address and port for this alpha.
	MyGrpcAddr string
	// ZeroAddr stores the list of address:port for the zero instances associated with this alpha.
	// Alpha would communicate via only one zero address from the list. All
	// the other addresses serve as fallback.
	ZeroAddr []string
	// TLS client config which will be used to connect with zero and alpha internally
	TLSClientConfig *tls.Config
	// TLS server config which will be used to initiate server internal port
	TLSServerConfig *tls.Config
	// Raft stores options related to Raft.
	Raft *z.SuperFlag
	// Badger stores the badger options.
	Badger badger.Options
	// WhiteListedIPRanges is a list of IP ranges from which requests will be allowed.
	WhiteListedIPRanges []IPRange
	// StrictMutations will cause mutations to unknown predicates to fail if set to true.
	StrictMutations bool
	// AclEnabled indicates whether the enterprise ACL feature is turned on.
	AclEnabled bool
	// AclJwtAlg stores the JWT signing algorithm.
	AclJwtAlg jwt.SigningMethod
	// AclPublicKey stores the public key used to verify JSON Web Tokens (JWT).
	// It could be a either a RSA or ECDSA PublicKey or HMAC symmetric key.
	// depending upon the JWT signing algorithm. Note that for symmetric algorithms,
	// this will contain the same key as the private key, needs to be used carefully.
	AclPublicKey interface{}
	// AbortOlderThan tells Dgraph to discard transactions that are older than this duration.
	AbortOlderThan time.Duration
	// ProposedGroupId will be used if there's a file in the p directory called group_id with the
	// proposed group ID for this server.
	ProposedGroupId uint32
	// StartTime is the start time of the alpha
	StartTime time.Time
	// Security options:
	//
	// whitelist string - comma separated IP addresses
	// token string - if set, all Admin requests to Dgraph will have this token.
	Security *z.SuperFlag
	// EncryptionKey is the key used for encryption at rest, backups, exports. Enterprise only feature.
	EncryptionKey Sensitive
	// LogDQLRequest indicates whether alpha should log all query/mutation requests coming to it.
	// Ideally LogDQLRequest should be a bool value. But we are reading it using atomics across
	// queries hence it has been kept as int32. LogDQLRequest value 1 enables logging of requests
	// coming to alphas and 0 disables it.
	LogDQLRequest int32
	// If true, we should call msync or fsync after every write to survive hard reboots.
	HardSync bool
	// Audit contains the audit flags that enables the audit.
	Audit bool
}

// WorkerConfig stores the global instance of the worker package's options.
var WorkerConfig WorkerOptions

func (w *WorkerOptions) Parse(conf *viper.Viper) {
	w.MyAddr = conf.GetString("my")
	w.Trace = z.NewSuperFlag(conf.GetString("trace")).MergeAndCheckDefault(TraceDefaults)

	survive := conf.GetString("survive")
	AssertTruef(survive == "process" || survive == "filesystem",
		"Invalid survival mode: %s", survive)
	w.HardSync = survive == "filesystem"
}
