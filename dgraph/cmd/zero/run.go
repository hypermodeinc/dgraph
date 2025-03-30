/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package zero

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/spf13/cobra"
	otrace "go.opencensus.io/trace"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/zpages"
	"golang.org/x/net/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v24/conn"
	"github.com/hypermodeinc/dgraph/v24/ee/audit"
	"github.com/hypermodeinc/dgraph/v24/ee/enc"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/raftwal"
	"github.com/hypermodeinc/dgraph/v24/worker"
	"github.com/hypermodeinc/dgraph/v24/x"
)

type options struct {
	raft              *z.SuperFlag
	telemetry         *z.SuperFlag
	limit             *z.SuperFlag
	bindall           bool
	portOffset        int
	numReplicas       int
	peer              string
	w                 string
	rebalanceInterval time.Duration
	tlsClientConfig   *tls.Config
	audit             *x.LoggerConf
	limiterConfig     *x.LimiterConf
}

var opts options

// Zero is the sub-command used to start Zero servers.
var Zero x.SubCommand

func init() {
	Zero.Cmd = &cobra.Command{
		Use:   "zero",
		Short: "Run Dgraph Zero management server ",
		Long: `
A Dgraph Zero instance manages the Dgraph cluster.  Typically, a single Zero
instance is sufficient for the cluster; however, one can run multiple Zero
instances to achieve high-availability.
`,
		Run: func(cmd *cobra.Command, args []string) {
			defer x.StartProfile(Zero.Conf).Stop()
			run()
		},
		Annotations: map[string]string{"group": "core"},
	}
	Zero.EnvPrefix = "DGRAPH_ZERO"
	Zero.Cmd.SetHelpTemplate(x.NonRootTemplate)

	flag := Zero.Cmd.Flags()
	x.FillCommonFlags(flag)
	// --tls SuperFlag
	x.RegisterServerTLSFlags(flag)

	flag.IntP("port_offset", "o", 0,
		"Value added to all listening port numbers. [Grpc=5080, HTTP=6080]")
	flag.Int("replicas", 1, "How many Dgraph Alpha replicas to run per data shard group."+
		" The count includes the original shard.")
	flag.String("peer", "", "Address of another dgraphzero server.")
	flag.StringP("wal", "w", "zw", "Directory storing WAL.")
	flag.Duration("rebalance_interval", 8*time.Minute, "Interval for trying a predicate move.")
	flag.String("enterprise_license", "", "Path to the enterprise license file.")
	flag.String("cid", "", "Cluster ID")

	flag.String("limit", worker.ZeroLimitsDefaults, z.NewSuperFlagHelp(worker.ZeroLimitsDefaults).
		Head("Limit options").
		Flag("uid-lease",
			`The maximum number of UIDs that can be leased by namespace (except default namespace)
			in an interval specified by refill-interval. Set it to 0 to remove limiting.`).
		Flag("refill-interval",
			"The interval after which the tokens for UID lease are replenished.").
		Flag("disable-admin-http",
			"Turn on/off the administrative endpoints exposed over Zero's HTTP port.").
		String())

	flag.String("raft", raftDefaults, z.NewSuperFlagHelp(raftDefaults).
		Head("Raft options").
		Flag("idx",
			"Provides an optional Raft ID that this Alpha would use to join Raft groups.").
		Flag("learner",
			`Make this Zero a "learner" node. In learner mode, this Zero will not participate `+
				"in Raft elections. This can be used to achieve a read-only replica.").
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
}

func setupListener(addr string, port int, kind string) (listener net.Listener, err error) {
	laddr := fmt.Sprintf("%s:%d", addr, port)
	glog.Infof("Setting up %s listener at: %v\n", kind, laddr)
	return net.Listen("tcp", laddr)
}

type state struct {
	node *node
	rs   *conn.RaftServer
	zero *Server
}

func (st *state) serveGRPC(l net.Listener, store *raftwal.DiskStorage) {
	x.RegisterExporters(Zero.Conf, "dgraph.zero")
	grpcOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(x.GrpcMaxSize),
		grpc.MaxSendMsgSize(x.GrpcMaxSize),
		grpc.MaxConcurrentStreams(1000),
		grpc.StatsHandler(otelgrpc.NewClientHandler()),
		grpc.UnaryInterceptor(audit.AuditRequestGRPC),
	}

	tlsConf, err := x.LoadServerTLSConfigForInternalPort(Zero.Conf)
	x.Check(err)
	if tlsConf != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsConf)))
	}
	s := grpc.NewServer(grpcOpts...)

	nodeId := opts.raft.GetUint64("idx")
	rc := pb.RaftContext{
		Id:        nodeId,
		Addr:      x.WorkerConfig.MyAddr,
		Group:     0,
		IsLearner: opts.raft.GetBool("learner"),
	}
	m := conn.NewNode(&rc, store, opts.tlsClientConfig)

	// Zero followers should not be forwarding proposals to the leader, to avoid txn commits which
	// were calculated in a previous Zero leader.
	m.Cfg.DisableProposalForwarding = true
	st.rs = conn.NewRaftServer(m)

	st.node = &node{Node: m, ctx: context.Background(), closer: z.NewCloser(1)}
	st.zero = &Server{NumReplicas: opts.numReplicas, Node: st.node, tlsClientConfig: opts.tlsClientConfig}
	st.zero.Init()
	st.node.server = st.zero

	pb.RegisterZeroServer(s, st.zero)
	pb.RegisterRaftServer(s, st.rs)

	go func() {
		defer st.zero.closer.Done()
		err := s.Serve(l)
		glog.Infof("gRPC server stopped : %v", err)

		// Attempt graceful stop (waits for pending RPCs), but force a stop if
		// it doesn't happen in a reasonable amount of time.
		done := make(chan struct{})
		const timeout = 5 * time.Second
		go func() {
			s.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(timeout):
			glog.Infof("Stopping grpc gracefully is taking longer than %v."+
				" Force stopping now. Pending RPCs will be abandoned.", timeout)
			s.Stop()
		}
	}()
}

func run() {
	// keeping this flag for backward compatibility
	telemetry := z.NewSuperFlag(Zero.Conf.GetString("telemetry")).
		MergeAndCheckDefault(x.TelemetryDefaults)

	x.PrintVersion()
	tlsConf, err := x.LoadClientTLSConfigForInternalPort(Zero.Conf)
	x.Check(err)

	raft := z.NewSuperFlag(Zero.Conf.GetString("raft")).MergeAndCheckDefault(
		raftDefaults)
	auditConf := audit.GetAuditConf(Zero.Conf.GetString("audit"))
	limit := z.NewSuperFlag(Zero.Conf.GetString("limit")).MergeAndCheckDefault(
		worker.ZeroLimitsDefaults)
	limitConf := &x.LimiterConf{
		UidLeaseLimit: limit.GetUint64("uid-lease"),
		RefillAfter:   limit.GetDuration("refill-interval"),
	}
	opts = options{
		telemetry:         telemetry,
		raft:              raft,
		limit:             limit,
		bindall:           Zero.Conf.GetBool("bindall"),
		portOffset:        Zero.Conf.GetInt("port_offset"),
		numReplicas:       Zero.Conf.GetInt("replicas"),
		peer:              Zero.Conf.GetString("peer"),
		w:                 Zero.Conf.GetString("wal"),
		rebalanceInterval: Zero.Conf.GetDuration("rebalance_interval"),
		tlsClientConfig:   tlsConf,
		audit:             auditConf,
		limiterConfig:     limitConf,
	}
	glog.Infof("Setting Config to: %+v", opts)
	x.WorkerConfig.Parse(Zero.Conf)

	if !enc.EeBuild && Zero.Conf.GetString("enterprise_license") != "" {
		log.Fatalf("ERROR: enterprise_license option cannot be applied to OSS builds. ")
	}

	if opts.numReplicas < 0 || opts.numReplicas%2 == 0 {
		log.Fatalf("ERROR: Number of replicas must be odd for consensus. Found: %d",
			opts.numReplicas)
	}

	if Zero.Conf.GetBool("expose_trace") {
		// TODO: Remove this once we get rid of event logs.
		trace.AuthRequest = func(req *http.Request) (any, sensitive bool) {
			return true, true
		}
	}

	if opts.audit != nil {
		wd, err := filepath.Abs(opts.w)
		x.Check(err)
		ad, err := filepath.Abs(opts.audit.Output)
		x.Check(err)
		x.AssertTruef(ad != wd,
			"WAL directory and Audit output cannot be the same ('%s').", opts.audit.Output)
	}

	if opts.rebalanceInterval <= 0 {
		log.Fatalf("ERROR: Rebalance interval must be greater than zero. Found: %d",
			opts.rebalanceInterval)
	}

	grpc.EnableTracing = false
	otrace.ApplyConfig(otrace.Config{
		DefaultSampler: otrace.ProbabilitySampler(Zero.Conf.GetFloat64("trace"))})

	addr := "localhost"
	if opts.bindall {
		addr = "0.0.0.0"
	}
	if x.WorkerConfig.MyAddr == "" {
		x.WorkerConfig.MyAddr = fmt.Sprintf("localhost:%d", x.PortZeroGrpc+opts.portOffset)
	}

	nodeId := opts.raft.GetUint64("idx")
	if nodeId == 0 {
		log.Fatalf("ERROR: raft.idx flag cannot be 0. Please set idx to a unique positive integer.")
	}
	grpcListener, err := setupListener(addr, x.PortZeroGrpc+opts.portOffset, "grpc")
	x.Check(err)
	httpListener, err := setupListener(addr, x.PortZeroHTTP+opts.portOffset, "http")
	x.Check(err)

	// Create and initialize write-ahead log.
	x.Checkf(os.MkdirAll(opts.w, 0700), "Error while creating WAL dir.")
	store := raftwal.Init(opts.w)
	store.SetUint(raftwal.RaftId, nodeId)
	store.SetUint(raftwal.GroupId, 0) // All zeros have group zero.

	// Initialize the servers.
	var st state
	st.serveGRPC(grpcListener, store)

	tlsCfg, err := x.LoadServerTLSConfig(Zero.Conf)
	x.Check(err)
	go x.StartListenHttpAndHttps(httpListener, tlsCfg, st.zero.closer)

	baseMux := http.NewServeMux()
	http.Handle("/", audit.AuditRequestHttp(baseMux))

	baseMux.HandleFunc("/health", st.pingResponse)
	// the following endpoints are disabled only if the flag is explicitly set to true
	if !limit.GetBool("disable-admin-http") {
		baseMux.HandleFunc("/state", st.getState)
		baseMux.HandleFunc("/removeNode", st.removeNode)
		baseMux.HandleFunc("/moveTablet", st.moveTablet)
		baseMux.HandleFunc("/assign", st.assign)
		baseMux.HandleFunc("/enterpriseLicense", st.applyEnterpriseLicense)
	}
	baseMux.HandleFunc("/debug/jemalloc", x.JemallocHandler)
	http.DefaultServeMux.Handle("/debug/z", zpages.NewTracezHandler(zpages.NewSpanProcessor()))

	// This must be here. It does not work if placed before Grpc init.
	x.Check(st.node.initAndStartNode())

	if opts.telemetry.GetBool("reports") {
		go st.zero.periodicallyPostTelemetry()
	}

	sdCh := make(chan os.Signal, 1)
	signal.Notify(sdCh, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	// handle signals
	go func() {
		var sigCnt int
		for sig := range sdCh {
			glog.Infof("--- Received %s signal", sig)
			sigCnt++
			if sigCnt == 1 {
				signal.Stop(sdCh)
				st.zero.closer.Signal()
			} else if sigCnt == 3 {
				glog.Infof("--- Got interrupt signal 3rd time. Aborting now.")
				os.Exit(1)
			} else {
				glog.Infof("--- Ignoring interrupt signal.")
			}
		}
	}()

	st.zero.closer.AddRunning(1)

	go func() {
		defer st.zero.closer.Done()
		<-st.zero.closer.HasBeenClosed()
		glog.Infoln("Shutting down...")
		close(sdCh)
		// Close doesn't close already opened connections.

		// Stop all HTTP requests.
		_ = httpListener.Close()
		// Stop Raft.
		st.node.closer.SignalAndWait()
		// Stop all internal requests.
		_ = grpcListener.Close()
	}()

	st.zero.closer.AddRunning(2)
	go x.MonitorMemoryMetrics(st.zero.closer)
	go x.MonitorDiskMetrics("wal_fs", opts.w, st.zero.closer)

	glog.Infoln("Running Dgraph Zero...")
	st.zero.closer.Wait()
	glog.Infoln("Closer closed.")

	err = store.Close()
	glog.Infof("Raft WAL closed with err: %v\n", err)

	audit.Close()

	st.zero.orc.close()
	glog.Infoln("All done. Goodbye!")
}
