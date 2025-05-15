/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package alpha

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	_ "net/http/pprof" // http profiler
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/mark3labs/mcp-go/server"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"go.opencensus.io/plugin/ocgrpc"
	"go.opentelemetry.io/contrib/zpages"
	"golang.org/x/net/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	_ "google.golang.org/grpc/encoding/gzip" // grpc compression
	"google.golang.org/grpc/health"
	hapi "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/dgo/v250/protos/api"
	apiv25 "github.com/dgraph-io/dgo/v250/protos/api.v25"
	_ "github.com/dgraph-io/gqlparser/v2/validator/rules" // make gql validator init() all rules
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/audit"
	"github.com/hypermodeinc/dgraph/v25/edgraph"
	"github.com/hypermodeinc/dgraph/v25/graphql/admin"
	dgraphmcp "github.com/hypermodeinc/dgraph/v25/mcp"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/tok"
	"github.com/hypermodeinc/dgraph/v25/worker"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var (
	bindall bool

	// used for computing uptime
	startTime = time.Now()

	// Alpha is the sub-command invoked when running "dgraph alpha".
	Alpha x.SubCommand

	// need this here to refer it in admin_backup.go
	adminServer admin.IServeGraphQL
	initDone    uint32
)

func init() {
	Alpha.Cmd = &cobra.Command{
		Use:   "alpha",
		Short: "Run Dgraph Alpha database server",
		Long: `
A Dgraph Alpha instance stores the data. Each Dgraph Alpha is responsible for
storing and serving one data group. If multiple Alphas serve the same group,
they form a Raft group and provide synchronous replication.
`,
		Run: func(cmd *cobra.Command, args []string) {
			defer x.StartProfile(Alpha.Conf).Stop()
			run()
		},
		Annotations: map[string]string{"group": "core"},
	}
	Alpha.EnvPrefix = "DGRAPH_ALPHA"
	Alpha.Cmd.SetHelpTemplate(x.NonRootTemplate)

	// If you change any of the flags below, you must also update run() to call Alpha.Conf.Get
	// with the flag name so that the values are picked up by Cobra/Viper's various config inputs
	// (e.g, config file, env vars, cli flags, etc.)
	flag := Alpha.Cmd.Flags()

	// common
	x.FillCommonFlags(flag)
	// --tls SuperFlag
	x.RegisterServerTLSFlags(flag)
	// --encryption and --vault Superflag
	x.RegisterAclAndEncFlags(flag)

	flag.StringP("postings", "p", "p", "Directory to store posting lists.")
	flag.String("tmp", "t", "Directory to store temporary buffers.")

	flag.StringP("wal", "w", "w", "Directory to store raft write-ahead logs.")
	flag.String("export", "export", "Folder in which to store exports.")
	flag.StringP("zero", "z", fmt.Sprintf("localhost:%d", x.PortZeroGrpc),
		"Comma separated list of Dgraph Zero addresses of the form IP_ADDRESS:PORT.")

	// Useful for running multiple servers on the same machine.
	flag.IntP("port_offset", "o", 0,
		"Value added to all listening port numbers. [Internal=7080, HTTP=8080, Grpc=9080]")

	// Custom plugins.
	flag.String("custom_tokenizers", "",
		"Comma separated list of tokenizer plugins for custom indices.")

	// By default Go GRPC traces all requests.
	grpc.EnableTracing = false

	flag.String("badger", worker.BadgerDefaults, z.NewSuperFlagHelp(worker.BadgerDefaults).
		Head("Badger options (Refer to badger documentation for all possible options)").
		Flag("compression",
			`[none, zstd:level, snappy] Specifies the compression algorithm and
			compression level (if applicable) for the postings directory."none" would disable
			compression, while "zstd:1" would set zstd compression at level 1.`).
		Flag("numgoroutines",
			"The number of goroutines to use in badger.Stream.").
		String())

	// Cache flags.
	flag.String("cache", worker.CacheDefaults, z.NewSuperFlagHelp(worker.CacheDefaults).
		Head("Cache options").
		Flag("size-mb",
			"Total size of cache (in MB) to be used in Dgraph.").
		Flag("percentage",
			"Cache percentages summing up to 100 for various caches (FORMAT: PostingListCache,"+
				"PstoreBlockCache,PstoreIndexCache)").
		Flag("remove-on-update",
			"Dgraph implements a read-through cache (using Ristretto) to optimize query performance. On "+
				"mutation, the new value is first written to disk and then updated in the cache "+
				"before the transaction completes to ensure there are no stale reads during parallel "+
				"transactions. In mutation-heavy use cases, it may be more advantageous to remove "+
				"the cache entry on mutation instead of updating the value, as this executes ~14\\%"+
				"faster on write. The new value will be added to the cache the first time it is "+
				"queried, slightly delaying that read. To use this approach, set the --cache "+
				"remove-on-update flag.").
		String())

	flag.String("raft", worker.RaftDefaults, z.NewSuperFlagHelp(worker.RaftDefaults).
		Head("Raft options").
		Flag("idx",
			"Provides an optional Raft ID that this Alpha would use to join Raft groups.").
		Flag("group",
			"Provides an optional Raft Group ID that this Alpha would indicate to Zero to join.").
		Flag("learner",
			`Make this Alpha a "learner" node. In learner mode, this Alpha will not participate `+
				"in Raft elections. This can be used to achieve a read-only replica.").
		Flag("snapshot-after-entries",
			"Create a new Raft snapshot after N number of Raft entries. The lower this number, "+
				"the more frequent snapshot creation will be. Snapshots are created only if both "+
				"snapshot-after-duration and snapshot-after-entries threshold are crossed.").
		Flag("snapshot-after-duration",
			"Frequency at which we should create a new raft snapshots. Set "+
				"to 0 to disable duration based snapshot.").
		Flag("pending-proposals",
			"Number of pending mutation proposals. Useful for rate limiting.").
		String())

	flag.String("security", worker.SecurityDefaults, z.NewSuperFlagHelp(worker.SecurityDefaults).
		Head("Security options").
		Flag("token",
			"If set, all Admin requests to Dgraph will need to have this token. The token can be "+
				"passed as follows: for HTTP requests, in the X-Dgraph-AuthToken header. For Grpc, "+
				"in auth-token key in the context.").
		Flag("whitelist",
			"A comma separated list of IP addresses, IP ranges, CIDR blocks, or hostnames you wish "+
				"to whitelist for performing admin actions (i.e., --security "+
				`"whitelist=144.142.126.254,127.0.0.1:127.0.0.3,192.168.0.0/16,host.docker.`+
				`internal").`).
		String())

	flag.String("limit", worker.LimitDefaults, z.NewSuperFlagHelp(worker.LimitDefaults).
		Head("Limit options").
		Flag("query-edge",
			"The maximum number of edges that can be returned in a query. This applies to shortest "+
				"path and recursive queries.").
		Flag("normalize-node",
			"The maximum number of nodes that can be returned in a query that uses the normalize "+
				"directive.").
		Flag("mutations",
			"[allow, disallow, strict] The mutations mode to use.").
		Flag("mutations-nquad",
			"The maximum number of nquads that can be inserted in a mutation request.").
		Flag("disallow-drop",
			"Set disallow-drop to true to block drop-all and drop-data operation. It still"+
				" allows dropping attributes and types.").
		Flag("max-pending-queries",
			"Number of maximum pending queries before we reject them as too many requests.").
		Flag("query-timeout",
			"Maximum time after which a query execution will fail. If set to"+
				" 0, the timeout is infinite.").
		Flag("max-retries",
			"Commits to disk will give up after these number of retries to prevent locking the "+
				"worker in a failed state. Use -1 to retry infinitely.").
		Flag("txn-abort-after", "Abort any pending transactions older than this duration."+
			" The liveness of a transaction is determined by its last mutation.").
		Flag("shared-instance", "When set to true, it disables ACLs for non-galaxy users. "+
			"It expects the access JWT to be constructed outside dgraph for non-galaxy users as "+
			"login is denied to them. Additionally, this disables access to environment variables for minio, aws, etc.").
		Flag("type-filter-uid-limit", "TypeFilterUidLimit decides how many elements would be searched directly"+
			" vs searched via type index. If the number of elements are too low, then querying the"+
			" index might be slower. This would allow people to set their limit according to"+
			" their use case.").
		String())

	flag.String("graphql", worker.GraphQLDefaults, z.NewSuperFlagHelp(worker.GraphQLDefaults).
		Head("GraphQL options").
		Flag("introspection",
			"Enables GraphQL schema introspection.").
		Flag("debug",
			"Enables debug mode in GraphQL. This returns auth errors to clients, and we do not "+
				"recommend turning it on for production.").
		Flag("extensions",
			"Enables extensions in GraphQL response body.").
		Flag("poll-interval",
			"The polling interval for GraphQL subscription.").
		Flag("lambda-url",
			"The URL of a lambda server that implements custom GraphQL Javascript resolvers.").
		String())

	flag.String("cdc", worker.CDCDefaults, z.NewSuperFlagHelp(worker.CDCDefaults).
		Head("Change Data Capture options").
		Flag("file",
			"The path where audit logs will be stored.").
		Flag("kafka",
			"A comma separated list of Kafka hosts.").
		Flag("sasl-user",
			"The SASL username for Kafka.").
		Flag("sasl-password",
			"The SASL password for Kafka.").
		Flag("sasl-mechanism",
			"The SASL mechanism for Kafka (PLAIN, SCRAM-SHA-256 or SCRAM-SHA-512)").
		Flag("ca-cert",
			"The path to CA cert file for TLS encryption.").
		Flag("client-cert",
			"The path to client cert file for TLS encryption.").
		Flag("client-key",
			"The path to client key file for TLS encryption.").
		String())

	flag.String("audit", worker.AuditDefaults, z.NewSuperFlagHelp(worker.AuditDefaults).
		Head("Audit options").
		Flag("output",
			`[stdout, /path/to/dir] This specifies where audit logs should be output to.
			"stdout" is for standard output. You can also specify the directory where audit logs
			will be saved. When stdout is specified as output other fields will be ignored.`).
		Flag("compress",
			"Enables the compression of old audit logs.").
		Flag("encrypt-file",
			"The path to the key file to be used for audit log encryption.").
		Flag("days",
			"The number of days audit logs will be preserved.").
		Flag("size",
			"The audit log max size in MB after which it will be rolled over.").
		String())

	flag.String("feature-flags", worker.FeatureFlagsDefaults, z.NewSuperFlagHelp(worker.FeatureFlagsDefaults).
		Head("Feature flags to enable various experimental features").
		Flag("normalize-compatibility-mode", "configure @normalize response formatting."+
			" 'v20': returns values with repeated key for fields with same alias (same as v20.11)."+
			" For more details, see https://github.com/hypermodeinc/dgraph/pull/7639").
		Flag("enable-detailed-metrics", "Enable metrics about disk reads and cache per predicate").
		String())
}

func setupCustomTokenizers() {
	customTokenizers := Alpha.Conf.GetString("custom_tokenizers")
	if customTokenizers == "" {
		return
	}
	for _, soFile := range strings.Split(customTokenizers, ",") {
		tok.LoadCustomTokenizer(soFile)
	}
}

// Parses a comma-delimited list of IP addresses, IP ranges, CIDR blocks, or hostnames
// and returns a slice of []IPRange.
//
// e.g. "144.142.126.222:144.142.126.244,144.142.126.254,192.168.0.0/16,host.docker.internal"
func getIPsFromString(str string) ([]x.IPRange, error) {
	if str == "" {
		return []x.IPRange{}, nil
	}

	var ipRanges []x.IPRange
	rangeStrings := strings.Split(str, ",")

	for _, s := range rangeStrings {
		isIPv6 := strings.Contains(s, "::")
		tuple := strings.Split(s, ":")
		switch {
		case isIPv6 || len(tuple) == 1:
			if !strings.Contains(s, "/") {
				// string is hostname like host.docker.internal,
				// or IPv4 address like 144.124.126.254,
				// or IPv6 address like fd03:b188:0f3c:9ec4::babe:face
				ipAddr := net.ParseIP(s)
				if ipAddr != nil {
					ipRanges = append(ipRanges, x.IPRange{Lower: ipAddr, Upper: ipAddr})
				} else {
					ipAddrs, err := net.LookupIP(s)
					if err != nil {
						return nil, errors.Errorf("invalid IP address or hostname: %s", s)
					}

					for _, addr := range ipAddrs {
						ipRanges = append(ipRanges, x.IPRange{Lower: addr, Upper: addr})
					}
				}
			} else {
				// string is CIDR block like 192.168.0.0/16 or fd03:b188:0f3c:9ec4::/64
				rangeLo, network, err := net.ParseCIDR(s)
				if err != nil {
					return nil, errors.Errorf("invalid CIDR block: %s", s)
				}

				addrLen, maskLen := len(rangeLo), len(network.Mask)
				rangeHi := make(net.IP, len(rangeLo))
				copy(rangeHi, rangeLo)
				for i := 1; i <= maskLen; i++ {
					rangeHi[addrLen-i] |= ^network.Mask[maskLen-i]
				}

				ipRanges = append(ipRanges, x.IPRange{Lower: rangeLo, Upper: rangeHi})
			}
		case len(tuple) == 2:
			// string is range like a.b.c.d:w.x.y.z
			rangeLo := net.ParseIP(tuple[0])
			rangeHi := net.ParseIP(tuple[1])
			switch {
			case rangeLo == nil:
				return nil, errors.Errorf("invalid IP address: %s", tuple[0])
			case rangeHi == nil:
				return nil, errors.Errorf("invalid IP address: %s", tuple[1])
			case bytes.Compare(rangeLo, rangeHi) > 0:
				return nil, errors.Errorf("inverted IP address range: %s", s)
			}
			ipRanges = append(ipRanges, x.IPRange{Lower: rangeLo, Upper: rangeHi})
		default:
			return nil, errors.Errorf("invalid IP address range: %s", s)
		}
	}

	return ipRanges, nil
}

func httpPort() int {
	return x.Config.PortOffset + x.PortHTTP
}

func grpcPort() int {
	return x.Config.PortOffset + x.PortGrpc
}

func healthCheck(w http.ResponseWriter, r *http.Request) {
	x.AddCorsHeaders(w)
	var err error

	if _, ok := r.URL.Query()["all"]; ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		ctx := x.AttachAccessJwt(context.Background(), r)
		var resp *api.Response
		if resp, err = (&edgraph.Server{}).Health(ctx, true); err != nil {
			x.SetStatus(w, x.Error, err.Error())
			return
		}
		if resp == nil {
			x.SetStatus(w, x.ErrorNoData, "No health information available.")
			return
		}
		_, _ = w.Write(resp.Json)
		return
	}

	_, ok := r.URL.Query()["live"]
	if !ok {
		if err := x.HealthCheck(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, err = w.Write([]byte(err.Error()))
			if err != nil {
				glog.V(2).Infof("Error while writing health check response: %v", err)
			}
			return
		}
	}

	var resp *api.Response
	if resp, err = (&edgraph.Server{}).Health(context.Background(), false); err != nil {
		x.SetStatus(w, x.Error, err.Error())
		return
	}
	if resp == nil {
		x.SetStatus(w, x.ErrorNoData, "No health information available.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Json)
}

func stateHandler(w http.ResponseWriter, r *http.Request) {
	var err error
	x.AddCorsHeaders(w)
	w.Header().Set("Content-Type", "application/json")

	ctx := context.Background()
	ctx = x.AttachAccessJwt(ctx, r)

	var aResp *api.Response
	if aResp, err = (&edgraph.Server{}).State(ctx); err != nil {
		x.SetStatus(w, x.Error, err.Error())
		return
	}
	if aResp == nil {
		x.SetStatus(w, x.ErrorNoData, "No state information available.")
		return
	}

	if _, err = w.Write(aResp.Json); err != nil {
		x.SetStatus(w, x.Error, err.Error())
		return
	}
}

// storeStatsHandler outputs some basic stats for data store.
func storeStatsHandler(w http.ResponseWriter, r *http.Request) {
	x.AddCorsHeaders(w)
	w.Header().Set("Content-Type", "text/html")
	x.Check2(w.Write([]byte("<pre>")))
	x.Check2(w.Write([]byte(worker.StoreStats())))
	x.Check2(w.Write([]byte("</pre>")))
}

func setupListener(addr string, port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("%s:%d", addr, port))
}

func serveGRPC(l net.Listener, tlsCfg *tls.Config, closer *z.Closer) {
	defer closer.Done()

	x.RegisterExporters(Alpha.Conf, "dgraph.alpha")

	opt := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(x.GrpcMaxSize),
		grpc.MaxSendMsgSize(x.GrpcMaxSize),
		grpc.MaxConcurrentStreams(1000),
		grpc.StatsHandler(&ocgrpc.ServerHandler{}),
		grpc.UnaryInterceptor(audit.AuditRequestGRPC),
	}
	if tlsCfg != nil {
		tlsCfg.NextProtos = []string{"h2"}
		opt = append(opt, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}

	s := grpc.NewServer(opt...)
	api.RegisterDgraphServer(s, &edgraph.Server{})
	apiv25.RegisterDgraphServer(s, &edgraph.ServerV25{})
	hapi.RegisterHealthServer(s, health.NewServer())
	worker.RegisterZeroProxyServer(s)

	err := s.Serve(l)
	glog.Errorf("GRPC listener canceled: %v\n", err)
	s.Stop()
}

func setupMcp(baseMux *http.ServeMux, connectionString, readOnly string) error {
	s, err := dgraphmcp.NewMCPServer(connectionString, readOnly)
	if err != nil {
		glog.Errorf("Failed to initialize MCPServer: %v\n", err)
		return err
	}

	sse := server.NewSSEServer(s,
		server.WithBasePath("/mcp"),
	)
	baseMux.HandleFunc("/mcp", sse.ServeHTTP)
	baseMux.HandleFunc("/mcp/", sse.ServeHTTP)
	return nil
}

func buildConnectionString(addr string, port int) string {
	return fmt.Sprintf("dgraph://%s:%d", addr, port)
}

func setupServer(closer *z.Closer) {
	go worker.RunServer(bindall) // For pb.communication.
	laddr := "localhost"
	if bindall {
		laddr = "0.0.0.0"
	}

	tlsCfg, err := x.LoadServerTLSConfig(Alpha.Conf)
	if err != nil {
		log.Fatalf("Failed to setup TLS: %v\n", err)
	}

	httpListener, err := setupListener(laddr, httpPort())
	if err != nil {
		log.Fatal(err)
	}

	grpcListener, err := setupListener(laddr, grpcPort())
	if err != nil {
		log.Fatal(err)
	}

	baseMux := http.NewServeMux()
	http.Handle("/", audit.AuditRequestHttp(baseMux))

	http.HandleFunc("/login", loginHandler)
	baseMux.HandleFunc("/query", queryHandler)
	baseMux.HandleFunc("/query/", queryHandler)
	baseMux.HandleFunc("/mutate", mutationHandler)
	baseMux.HandleFunc("/mutate/", mutationHandler)
	baseMux.HandleFunc("/commit", commitHandler)
	baseMux.HandleFunc("/alter", alterHandler)
	baseMux.HandleFunc("/health", healthCheck)
	baseMux.HandleFunc("/state", stateHandler)
	baseMux.HandleFunc("/debug/jemalloc", x.JemallocHandler)
	http.DefaultServeMux.Handle("/debug/z", zpages.NewTracezHandler(zpages.NewSpanProcessor()))

	// TODO: Figure out what this is for?
	http.HandleFunc("/debug/store", storeStatsHandler)

	introspection := x.Config.GraphQL.GetBool("introspection")

	// Global Epoch is a lockless synchronization mechanism for graphql service.
	// It's is just an atomic counter used by the graphql subscription to update its state.
	// It's is used to detect the schema changes and server exit.
	// It is also reported by /probe/graphql endpoint as the schemaUpdateCounter.

	// Implementation for schema change:
	// The global epoch is incremented when there is a schema change.
	// Polling goroutine acquires the current epoch count as a local epoch.
	// The local epoch count is checked against the global epoch,
	// If there is change then we terminate the subscription.

	// Implementation for server exit:
	// The global epoch is set to maxUint64 while exiting the server.
	// By using this information polling goroutine terminates the subscription.
	globalEpoch := make(map[uint64]*uint64)
	e := new(uint64)
	atomic.StoreUint64(e, 0)
	globalEpoch[x.RootNamespace] = e
	var mainServer admin.IServeGraphQL
	var gqlHealthStore *admin.GraphQLHealthStore
	// Do not use := notation here because adminServer is a global variable.
	mainServer, adminServer, gqlHealthStore = admin.NewServers(introspection,
		globalEpoch, closer)
	baseMux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		namespace := x.ExtractNamespaceHTTP(r)
		r.Header.Set("resolver", strconv.FormatUint(namespace, 10))
		if err := admin.LazyLoadSchema(namespace); err != nil {
			admin.WriteErrorResponse(w, r, err)
			return
		}
		mainServer.HTTPHandler().ServeHTTP(w, r)
	})

	baseMux.Handle("/probe/graphql", graphqlProbeHandler(gqlHealthStore, globalEpoch))

	baseMux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("resolver", "0")
		// We don't need to load the schema for all the admin operations.
		// Only a few like getUser, queryGroup require this. So, this can be optimized.
		if err := admin.LazyLoadSchema(x.ExtractNamespaceHTTP(r)); err != nil {
			admin.WriteErrorResponse(w, r, err)
			return
		}
		allowedMethodsHandler(allowedMethods{
			http.MethodGet:     true,
			http.MethodPost:    true,
			http.MethodOptions: true,
		}, adminAuthHandler(adminServer.HTTPHandler())).ServeHTTP(w, r)
	})
	baseMux.Handle("/admin/", getAdminMux())

	addr := fmt.Sprintf("%s:%d", laddr, httpPort())
	glog.Infof("Bringing up GraphQL HTTP API at %s/graphql", addr)
	glog.Infof("Bringing up GraphQL HTTP admin API at %s/admin", addr)

	baseMux.Handle("/", http.HandlerFunc(homeHandler))
	baseMux.Handle("/ui/keywords", http.HandlerFunc(keywordHandler))

	// Initialize the servers.
	x.ServerCloser.AddRunning(3)
	go serveGRPC(grpcListener, tlsCfg, x.ServerCloser)

	if err := setupMcp(baseMux, buildConnectionString(laddr, grpcPort()), "true"); err != nil {
		log.Fatal(err)
	}
	go x.StartListenHttpAndHttps(httpListener, tlsCfg, x.ServerCloser)

	go func() {
		defer x.ServerCloser.Done()

		<-x.ServerCloser.HasBeenClosed()
		// TODO - Verify why do we do this and does it have to be done for all namespaces.
		e = globalEpoch[x.RootNamespace]
		atomic.StoreUint64(e, math.MaxUint64)

		// Stops grpc/http servers; Already accepted connections are not closed.
		if err := grpcListener.Close(); err != nil {
			glog.Warningf("Error while closing gRPC listener: %s", err)
		}
		if err := httpListener.Close(); err != nil {
			glog.Warningf("Error while closing HTTP listener: %s", err)
		}
	}()

	glog.Infoln("gRPC server started.  Listening on port", grpcPort())
	glog.Infoln("HTTP server started.  Listening on port", httpPort())

	atomic.AddUint32(&initDone, 1)
	// Audit needs groupId and nodeId to initialize audit files
	// Therefore we wait for the cluster initialization to be done.
	for {
		if x.HealthCheck() == nil {
			x.Check(audit.InitAuditorIfNecessary(worker.Config.Audit))
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	x.ServerCloser.Wait()
}

func run() {
	// keeping this flag for backward compatibility
	_ = z.NewSuperFlag(Alpha.Conf.GetString("telemetry")).
		MergeAndCheckDefault(x.TelemetryDefaults)

	bindall = Alpha.Conf.GetBool("bindall")
	cache := z.NewSuperFlag(Alpha.Conf.GetString("cache")).MergeAndCheckDefault(
		worker.CacheDefaults)
	totalCache := cache.GetInt64("size-mb")
	x.AssertTruef(totalCache >= 0, "ERROR: Cache size must be non-negative")

	cachePercentage := cache.GetString("percentage")
	removeOnUpdate := cache.GetBool("remove-on-update")
	cachePercent, err := x.GetCachePercentages(cachePercentage, 3)
	x.Check(err)
	postingListCacheSize := (cachePercent[0] * (totalCache << 20)) / 100
	pstoreBlockCacheSize := (cachePercent[1] * (totalCache << 20)) / 100
	pstoreIndexCacheSize := (cachePercent[2] * (totalCache << 20)) / 100

	cacheOpts := fmt.Sprintf("blockcachesize=%d; indexcachesize=%d; ",
		pstoreBlockCacheSize, pstoreIndexCacheSize)
	bopts := badger.DefaultOptions("").FromSuperFlag(worker.BadgerDefaults + cacheOpts).
		FromSuperFlag(Alpha.Conf.GetString("badger"))
	security := z.NewSuperFlag(Alpha.Conf.GetString("security")).MergeAndCheckDefault(
		worker.SecurityDefaults)
	conf := audit.GetAuditConf(Alpha.Conf.GetString("audit"))

	x.Config.Limit = z.NewSuperFlag(Alpha.Conf.GetString("limit")).MergeAndCheckDefault(
		worker.LimitDefaults)

	opts := worker.Options{
		PostingDir:      Alpha.Conf.GetString("postings"),
		WALDir:          Alpha.Conf.GetString("wal"),
		CacheMb:         totalCache,
		CachePercentage: cachePercentage,
		RemoveOnUpdate:  removeOnUpdate,

		MutationsMode:      worker.AllowMutations,
		AuthToken:          security.GetString("token"),
		Audit:              conf,
		ChangeDataConf:     Alpha.Conf.GetString("cdc"),
		TypeFilterUidLimit: x.Config.Limit.GetUint64("type-filter-uid-limit"),
	}

	keys, err := x.GetEncAclKeys(Alpha.Conf)
	x.Check(err)

	if keys.AclSecretKey != nil {
		opts.AclJwtAlg = keys.AclJwtAlg
		opts.AclSecretKey = keys.AclSecretKey
		opts.AclSecretKeyBytes = keys.AclSecretKeyBytes
		opts.AccessJwtTtl = keys.AclAccessTtl
		opts.RefreshJwtTtl = keys.AclRefreshTtl
		glog.Info("ACL secret key loaded successfully.")
	}

	abortDur := x.Config.Limit.GetDuration("txn-abort-after")
	switch strings.ToLower(x.Config.Limit.GetString("mutations")) {
	case "allow":
		opts.MutationsMode = worker.AllowMutations
	case "disallow":
		opts.MutationsMode = worker.DisallowMutations
	case "strict":
		opts.MutationsMode = worker.StrictMutations
	default:
		glog.Error(`--limit "mutations=<mode>;" must be one of allow, disallow, or strict`)
		os.Exit(1)
	}

	worker.SetConfiguration(&opts)

	ips, err := getIPsFromString(security.GetString("whitelist"))
	x.Check(err)

	tlsClientConf, err := x.LoadClientTLSConfigForInternalPort(Alpha.Conf)
	x.Check(err)
	tlsServerConf, err := x.LoadServerTLSConfigForInternalPort(Alpha.Conf)
	x.Check(err)

	raft := z.NewSuperFlag(Alpha.Conf.GetString("raft")).MergeAndCheckDefault(worker.RaftDefaults)
	x.WorkerConfig = x.WorkerOptions{
		TmpDir:              Alpha.Conf.GetString("tmp"),
		ExportPath:          Alpha.Conf.GetString("export"),
		ZeroAddr:            strings.Split(Alpha.Conf.GetString("zero"), ","),
		Raft:                raft,
		WhiteListedIPRanges: ips,
		StrictMutations:     opts.MutationsMode == worker.StrictMutations,
		AclEnabled:          keys.AclSecretKey != nil,
		AbortOlderThan:      abortDur,
		StartTime:           startTime,
		Security:            security,
		TLSClientConfig:     tlsClientConf,
		TLSServerConfig:     tlsServerConf,
		AclJwtAlg:           keys.AclJwtAlg,
		AclPublicKey:        keys.AclPublicKey,
		Audit:               opts.Audit != nil,
		Badger:              bopts,
	}
	x.WorkerConfig.Parse(Alpha.Conf)

	// Set the directory for temporary buffers.
	z.SetTmpDir(x.WorkerConfig.TmpDir)

	x.WorkerConfig.EncryptionKey = keys.EncKey

	setupCustomTokenizers()
	x.Config.PortOffset = Alpha.Conf.GetInt("port_offset")
	x.Config.LimitMutationsNquad = int(x.Config.Limit.GetInt64("mutations-nquad"))
	x.Config.LimitQueryEdge = x.Config.Limit.GetUint64("query-edge")
	x.Config.BlockClusterWideDrop = x.Config.Limit.GetBool("disallow-drop")
	x.Config.LimitNormalizeNode = int(x.Config.Limit.GetInt64("normalize-node"))
	x.Config.QueryTimeout = x.Config.Limit.GetDuration("query-timeout")
	x.Config.MaxRetries = x.Config.Limit.GetInt64("max-retries")
	x.Config.SharedInstance = x.Config.Limit.GetBool("shared-instance")

	x.Config.GraphQL = z.NewSuperFlag(Alpha.Conf.GetString("graphql")).MergeAndCheckDefault(
		worker.GraphQLDefaults)
	x.Config.GraphQLDebug = x.Config.GraphQL.GetBool("debug")
	if x.Config.GraphQL.GetString("lambda-url") != "" {
		graphqlLambdaUrl, err := url.Parse(x.Config.GraphQL.GetString("lambda-url"))
		if err != nil {
			glog.Errorf("unable to parse --graphql lambda-url: %v", err)
			return
		}
		if !graphqlLambdaUrl.IsAbs() {
			glog.Errorf("expecting --graphql lambda-url to be an absolute URL, got: %s",
				graphqlLambdaUrl.String())
			return
		}
	}
	edgraph.Init()

	// feature flags
	featureFlagsConf := z.NewSuperFlag(Alpha.Conf.GetString("feature-flags")).MergeAndCheckDefault(
		worker.FeatureFlagsDefaults)
	x.Config.NormalizeCompatibilityMode = featureFlagsConf.GetString("normalize-compatibility-mode")
	enableDetailedMetrics := featureFlagsConf.GetBool("enable-detailed-metrics")

	x.PrintVersion()
	glog.Infof("x.Config: %+v", x.Config)
	glog.Infof("x.WorkerConfig: %+v", x.WorkerConfig)
	glog.Infof("worker.Config: %+v", worker.Config)

	worker.InitServerState()
	worker.InitTasks()

	if Alpha.Conf.GetBool("expose_trace") {
		// TODO: Remove this once we get rid of event logs.
		trace.AuthRequest = func(req *http.Request) (any, sensitive bool) {
			return true, true
		}
	}

	// Posting will initialize index which requires schema. Hence, initialize
	// schema before calling posting.Init().
	schema.Init(worker.State.Pstore)
	posting.Init(worker.State.Pstore, postingListCacheSize, removeOnUpdate)
	posting.SetEnabledDetailedMetrics(enableDetailedMetrics)
	defer posting.Cleanup()
	worker.Init(worker.State.Pstore)

	// setup shutdown os signal handler
	sdCh := make(chan os.Signal, 3)

	defer func() {
		signal.Stop(sdCh)
		close(sdCh)
	}()
	// sigint : Ctrl-C, sigterm : kill command.
	signal.Notify(sdCh, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		var numShutDownSig int
		for range sdCh {
			closer := x.ServerCloser
			select {
			case <-closer.HasBeenClosed():
			default:
				closer.Signal()
			}
			numShutDownSig++
			glog.Infoln("Caught Ctrl-C. Terminating now (this may take a few seconds)...")

			switch {
			case atomic.LoadUint32(&initDone) < 2:
				// Forcefully kill alpha if we haven't finish server initialization.
				glog.Infoln("Stopped before initialization completed")
				os.Exit(1)
			case numShutDownSig == 3:
				glog.Infoln("Signaled thrice. Aborting!")
				os.Exit(1)
			}
		}
	}()

	updaters := z.NewCloser(2)
	go func() {
		worker.StartRaftNodes(worker.State.WALstore, bindall)
		atomic.AddUint32(&initDone, 1)

		// initialization of the admin account can only be done after raft nodes are running
		// and health check passes
		edgraph.InitializeAcl(updaters)
		edgraph.RefreshACLs(updaters.Ctx())
		edgraph.SubscribeForAclUpdates(updaters)
	}()

	// Graphql subscribes to alpha to get schema updates. We need to close that before we
	// close alpha. This closer is for closing and waiting that subscription.
	adminCloser := z.NewCloser(1)

	setupServer(adminCloser)
	glog.Infoln("GRPC and HTTP stopped.")

	// This might not close until group is given the signal to close. So, only signal here,
	// wait for it after group is closed.
	updaters.Signal()

	worker.BlockingStop()
	glog.Infoln("worker stopped.")

	adminCloser.SignalAndWait()
	glog.Infoln("adminCloser closed.")

	audit.Close()

	worker.State.Dispose()
	glog.Info("worker.State disposed.")

	updaters.Wait()
	glog.Infoln("updaters closed.")

	glog.Infoln("Server shutdown. Bye!")
}
