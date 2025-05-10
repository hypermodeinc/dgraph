/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package live

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	_ "net/http/pprof" // http profiler
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dgryski/go-farm"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/dgraph-io/badger/v4"
	bopt "github.com/dgraph-io/badger/v4/options"
	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/chunker"
	"github.com/hypermodeinc/dgraph/v25/enc"
	"github.com/hypermodeinc/dgraph/v25/filestore"
	schemapkg "github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
	"github.com/hypermodeinc/dgraph/v25/xidmap"
)

type options struct {
	dataFiles       string
	dataFormat      string
	schemaFile      string
	concurrent      int
	batchSize       int
	clientDir       string
	authToken       string
	useCompression  bool
	newUids         bool
	verbose         bool
	httpAddr        string
	bufferSize      int
	upsertPredicate string
	tmpDir          string
	key             x.Sensitive
	namespaceToLoad uint64
	preserveNs      bool
}

type Predicate struct {
	Predicate  string   `json:"predicate,omitempty"`
	Type       string   `json:"type,omitempty"`
	Tokenizer  []string `json:"tokenizer,omitempty"`
	Count      bool     `json:"count,omitempty"`
	List       bool     `json:"list,omitempty"`
	Lang       bool     `json:"lang,omitempty"`
	Index      bool     `json:"index,omitempty"`
	Upsert     bool     `json:"upsert,omitempty"`
	Reverse    bool     `json:"reverse,omitempty"`
	NoConflict bool     `json:"no_conflict,omitempty"`
	Unique     bool     `json:"unique,omitempty"`
	ValueType  types.TypeID
}

type Schema struct {
	Predicates []*Predicate `json:"schema,omitempty"`
	preds      map[string]*Predicate
}

type request struct {
	*api.Mutation
	conflicts []uint64
}

func (l *Schema) init(ns uint64, galaxyOperation bool) {
	l.preds = make(map[string]*Predicate)
	for _, i := range l.Predicates {
		i.ValueType, _ = types.TypeForName(i.Type)
		if !galaxyOperation {
			i.Predicate = x.NamespaceAttr(ns, i.Predicate)
		}
		l.preds[i.Predicate] = i
	}
}

var (
	opt options
	sch Schema

	// Live is the sub-command invoked when running "dgraph live".
	Live x.SubCommand
)

func init() {
	Live.Cmd = &cobra.Command{
		Use:   "live",
		Short: "Run Dgraph Live Loader",
		Run: func(cmd *cobra.Command, args []string) {
			defer x.StartProfile(Live.Conf).Stop()
			if err := run(); err != nil {
				x.Check2(fmt.Fprintf(os.Stderr, "%s", err.Error()))
				os.Exit(1)
			}
		},
		Annotations: map[string]string{"group": "data-load"},
	}
	Live.EnvPrefix = "DGRAPH_LIVE"
	Live.Cmd.SetHelpTemplate(x.NonRootTemplate)

	flag := Live.Cmd.Flags()
	// --vault SuperFlag and encryption flags
	x.RegisterEncFlag(flag)
	// --tls SuperFlag
	x.RegisterClientTLSFlags(flag)

	flag.StringP("files", "f", "", "Location of *.rdf(.gz) or *.json(.gz) file(s) to load")
	flag.StringP("schema", "s", "", "Location of schema file")
	flag.String("format", "", "Specify file format (rdf or json) instead of getting it "+
		"from filename")
	flag.StringP("alpha", "a", "127.0.0.1:9080",
		"Comma-separated list of Dgraph alpha gRPC server addresses")
	flag.StringP("zero", "z", "", "(deprecated) Dgraph zero gRPC server address")
	flag.IntP("conc", "c", 10,
		"Number of concurrent requests to make to Dgraph")
	flag.IntP("batch", "b", 1000,
		"Number of N-Quads to send as part of a mutation.")
	flag.StringP("xidmap", "x", "", "Directory to store xid to uid mapping")
	flag.StringP("auth_token", "t", "",
		"The auth token passed to the server for Alter operation of the schema file. "+
			"If used with --slash_grpc_endpoint, then this should be set to the API token issued"+
			"by Slash GraphQL")
	flag.String("slash_grpc_endpoint", "", "Path to Slash GraphQL GRPC endpoint. "+
		"If --slash_grpc_endpoint is set, all other TLS options and connection options will be"+
		"ignored")
	flag.BoolP("use_compression", "C", false,
		"Enable compression on connection to alpha server")
	flag.Bool("new_uids", false,
		"Ignore UIDs in load files and assign new ones.")
	flag.String("http", "localhost:6060", "Address to serve http (pprof).")
	flag.Bool("verbose", false, "Run the live loader in verbose mode")

	flag.String("creds", "",
		`Various login credentials if login is required.
	user defines the username to login.
	password defines the password of the user.
	namespace defines the namespace to log into.
	Sample flag could look like --creds user=username;password=mypass;namespace=2`)

	flag.StringP("bufferSize", "m", "100", "Buffer for each thread")
	flag.StringP("upsertPredicate", "U", "", "run in upsertPredicate mode. the value would "+
		"be used to store blank nodes as an xid")
	flag.String("tmp", "t", "Directory to store temporary buffers.")
	flag.Int64("force-namespace", 0, "Namespace onto which to load the data."+
		"Only superadmin should use this for loading data into multiple namespaces or some"+
		"specific namespace. Setting it to negative value will preserve the namespace.")
}

func getSchema(ctx context.Context, dgraphClient *dgo.Dgraph, galaxyOperation bool) (*Schema, error) {
	txn := dgraphClient.NewTxn()
	defer func() {
		if err := txn.Discard(ctx); err != nil {
			glog.Warningf("error in discarding txn: %v", err)
		}
	}()

	res, err := txn.Query(ctx, "schema {}")
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(res.GetJson(), &sch)
	if err != nil {
		return nil, err
	}
	// If we are not loading data across namespaces, the schema query result will not contain the
	// namespace information. Set it inside the init function.
	sch.init(opt.namespaceToLoad, galaxyOperation)
	return &sch, nil
}

// validate that the schema contains the predicates whose namespace exist.
func validateSchema(sch string, namespaces map[uint64]struct{}) error {
	result, err := schemapkg.Parse(sch)
	if err != nil {
		return err
	}
	for _, pred := range result.Preds {
		ns := x.ParseNamespace(pred.Predicate)
		if _, ok := namespaces[ns]; !ok {
			return errors.Errorf("Namespace %#x doesn't exist for pred %s.", ns, pred.Predicate)
		}
	}
	for _, typ := range result.Types {
		ns := x.ParseNamespace(typ.TypeName)
		if _, ok := namespaces[ns]; !ok {
			return errors.Errorf("Namespace %#x doesn't exist for type %s.", ns, typ.TypeName)
		}
	}
	return nil
}

// processSchemaFile process schema for a given gz file.
func (l *loader) processSchemaFile(ctx context.Context, file string, key x.Sensitive,
	dgraphClient *dgo.Dgraph) error {
	fmt.Printf("\nProcessing schema file %q\n", file)
	if len(opt.authToken) > 0 {
		md := metadata.New(nil)
		md.Append("auth-token", opt.authToken)
		ctx = metadata.NewOutgoingContext(ctx, md)
	}

	f, err := filestore.Open(file)
	x.CheckfNoTrace(err)
	defer func() {
		if err := f.Close(); err != nil {
			glog.Warningf("error while closing fd: %v", err)
		}
	}()

	reader, err := enc.GetReader(key, f)
	x.Check(err)
	if strings.HasSuffix(strings.ToLower(file), ".gz") {
		reader, err = gzip.NewReader(reader)
		x.Check(err)
	}

	b, err := io.ReadAll(reader)
	if err != nil {
		x.Checkf(err, "Error while reading file")
	}

	op := &api.Operation{}
	op.Schema = string(b)
	if opt.preserveNs {
		// Verify schema if we are loading into multiple namespaces.
		if err := validateSchema(op.Schema, l.namespaces); err != nil {
			return err
		}
	}
	return dgraphClient.Alter(ctx, op)
}

func (l *loader) uid(val string, ns uint64) string {
	// Attempt to parse as a UID (in the same format that dgraph outputs - a
	// hex number prefixed by "0x"). If parsing succeeds, then this is assumed
	// to be an existing node in the graph. There is limited protection against
	// a user selecting an unassigned UID in this way - it may be assigned
	// later to another node. It is up to the user to avoid this.
	if !opt.newUids {
		if uid, err := strconv.ParseUint(val, 0, 64); err == nil {
			return fmt.Sprintf("%#x", uid)
		}
	}

	// TODO(Naman): Do we still need this here? As xidmap which uses btree does not keep hold of this string.
	sb := strings.Builder{}
	x.Check2(sb.WriteString(x.NamespaceAttr(ns, val)))
	uid, _ := l.alloc.AssignUid(sb.String())

	return fmt.Sprintf("%#x", uid)
}

func generateBlankNode(val string) string {
	// generates "u_hash(val)"

	sb := strings.Builder{}
	x.Check2(sb.WriteString("u_"))
	x.Check2(sb.WriteString(strconv.FormatUint(farm.Fingerprint64([]byte(val)), 10)))
	return sb.String()
}

func generateUidFunc(val string) string {
	// generates "uid(val)"

	sb := strings.Builder{}
	sb.WriteString("uid(")
	sb.WriteString(val)
	sb.WriteRune(')')
	return sb.String()
}

func generateQuery(node, predicate, xid string) string {
	// generates "node as node(func: eq(predicate, xid)) {uid}"

	sb := strings.Builder{}
	sb.WriteString(node)
	sb.WriteString(" as ")
	sb.WriteString(node)
	sb.WriteString("(func: eq(")
	sb.WriteString(predicate)
	sb.WriteString(`, `)
	sb.WriteString(strconv.Quote(xid))
	sb.WriteString(`)) {uid}`)
	return sb.String()
}

func (l *loader) upsertUids(nqs []*api.NQuad) error {
	// We form upsertPredicate query for each of the ids we saw in the request, along with
	// adding the corresponding xid to that uid. The mutation we added is only useful if the
	// uid doesn't exists.
	//
	// Example upsertPredicate mutation:
	//
	// query {
	//     u_1 as var(func: eq(xid, "m.1234"))
	// }
	//
	// mutation {
	//     set {
	//          uid(u_1) xid m.1234 .
	//     }
	// }
	l.upsertLock.Lock()
	defer l.upsertLock.Unlock()

	ids := make(map[string]string)

	for _, nq := range nqs {
		// taking hash as the value might contain invalid symbols
		subject := x.NamespaceAttr(nq.Namespace, nq.Subject)
		ids[subject] = generateBlankNode(subject)

		if len(nq.ObjectId) > 0 {
			// taking hash as the value might contain invalid symbols
			object := x.NamespaceAttr(nq.Namespace, nq.ObjectId)
			ids[object] = generateBlankNode(object)
		}
	}

	mutations := make([]*api.NQuad, 0, len(ids))
	query := strings.Builder{}
	query.WriteString("query {")
	query.WriteRune('\n')

	for xid, idx := range ids {
		if l.alloc.CheckUid(xid) {
			continue
		}

		// Strip away the namespace from the query and mutation.
		xid := x.ParseAttr(xid)
		query.WriteString(generateQuery(idx, opt.upsertPredicate, xid))
		query.WriteRune('\n')
		mutations = append(mutations, &api.NQuad{
			Subject:     generateUidFunc(idx),
			Predicate:   opt.upsertPredicate,
			ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: xid}},
		})
	}

	if len(mutations) == 0 {
		return nil
	}

	query.WriteRune('}')

	// allocate all the new xids
	resp, err := l.dc.NewTxn().Do(l.opts.Ctx, &api.Request{
		CommitNow: true,
		Query:     query.String(),
		Mutations: []*api.Mutation{{Set: mutations}},
	})
	if err != nil {
		return err
	}

	type dResult struct {
		Uid string
	}

	var result map[string][]dResult
	if err := json.Unmarshal(resp.GetJson(), &result); err != nil {
		return err
	}

	for xid, idx := range ids {
		// xid already exist in dgraph
		if val, ok := result[idx]; ok && len(val) > 0 {
			uid, err := strconv.ParseUint(val[0].Uid, 0, 64)
			if err != nil {
				return err
			}

			l.alloc.SetUid(xid, uid)
			continue
		}

		// new uid created in dgraph
		if val, ok := resp.GetUids()[generateUidFunc(idx)]; ok {
			uid, err := strconv.ParseUint(val, 0, 64)
			if err != nil {
				return err
			}

			l.alloc.SetUid(xid, uid)
			continue
		}
	}

	return nil
}

// allocateUids looks for the maximum uid value in the given NQuads and bumps the
// maximum seen uid to that value.
func (l *loader) allocateUids(nqs []*api.NQuad) {
	if opt.newUids {
		return
	}

	var maxUid uint64
	for _, nq := range nqs {
		sUid, err := strconv.ParseUint(nq.Subject, 0, 64)
		if err != nil {
			continue
		}
		if sUid > maxUid {
			maxUid = sUid
		}

		oUid, err := strconv.ParseUint(nq.ObjectId, 0, 64)
		if err != nil {
			continue
		}
		if oUid > maxUid {
			maxUid = oUid
		}
	}
	l.alloc.BumpTo(maxUid)
}

// processFile forwards a file to the RDF or JSON processor as appropriate
func (l *loader) processFile(ctx context.Context, fs filestore.FileStore, filename string,
	key x.Sensitive) error {

	fmt.Printf("Processing data file %q\n", filename)

	rd, cleanup := fs.ChunkReader(filename, key)
	defer cleanup()

	loadType := chunker.DataFormat(filename, opt.dataFormat)
	if loadType == chunker.UnknownFormat {
		if isJson, err := chunker.IsJSONData(rd); err == nil {
			if isJson {
				loadType = chunker.JsonFormat
			} else {
				return errors.Errorf("need --format=rdf or --format=json to load %s", filename)
			}
		}
	}

	return l.processLoadFile(ctx, rd, chunker.NewChunker(loadType, opt.batchSize))
}

func (l *loader) processLoadFile(ctx context.Context, rd *bufio.Reader, ck chunker.Chunker) error {
	nqbuf := ck.NQuads()
	errCh := make(chan error, 1)
	// Spin a goroutine to push NQuads to mutation channel.
	go func() {
		var err error
		defer func() {
			errCh <- err
		}()
		buffer := make([]*api.NQuad, 0, opt.bufferSize*opt.batchSize)

		drain := func() {
			// We collect opt.bufferSize requests and preprocess them. For the requests
			// to not conflict between themselves, we sort them on the basis of their predicates.
			// Predicates with count index will conflict among themselves, so we keep them at
			// end, making room for other predicates to load quickly.
			sort.Slice(buffer, func(i, j int) bool {
				iPred := sch.preds[x.NamespaceAttr(buffer[i].Namespace, buffer[i].Predicate)]
				jPred := sch.preds[x.NamespaceAttr(buffer[j].Namespace, buffer[j].Predicate)]
				t := func(a *Predicate) int {
					if a != nil && a.Count {
						return 1
					}
					return 0
				}

				// Sorts the nquads on basis of their predicates, while keeping the
				// predicates with count index later than those without it.
				if t(iPred) != t(jPred) {
					return t(iPred) < t(jPred)
				}
				return buffer[i].Predicate < buffer[j].Predicate
			})
			for len(buffer) > 0 {
				sz := opt.batchSize
				if len(buffer) < opt.batchSize {
					sz = len(buffer)
				}
				mu := &request{Mutation: &api.Mutation{Set: buffer[:sz]}}
				l.reqs <- mu
				buffer = buffer[sz:]
			}
		}

		for nqs := range nqbuf.Ch() {
			if len(nqs) == 0 {
				continue
			}

			for _, nq := range nqs {
				if !opt.preserveNs {
					// If do not preserve namespace, use the namespace passed through
					// `--force-namespace` flag.
					nq.Namespace = opt.namespaceToLoad
				}
				if _, ok := l.namespaces[nq.Namespace]; !ok {
					err = errors.Errorf("Cannot load nquad:%+v as its namespace doesn't exist.", nq)
					return
				}
			}

			if opt.upsertPredicate == "" {
				l.allocateUids(nqs)
			} else {
				// TODO(Naman): Handle this. Upserts UIDs send a single upsert block for multiple
				// nquads. These nquads may belong to different namespaces. Hence, alpha can't
				// figure out its processing.
				// Currently, this option works with data loading in the logged-in namespace.
				// TODO(Naman): Add a test for a case when it works and when it doesn't.
				if err := l.upsertUids(nqs); err != nil {
					return
				}
			}

			for _, nq := range nqs {
				nq.Subject = l.uid(nq.Subject, nq.Namespace)
				if len(nq.ObjectId) > 0 {
					nq.ObjectId = l.uid(nq.ObjectId, nq.Namespace)
				}
			}

			buffer = append(buffer, nqs...)
			if len(buffer) < opt.bufferSize*opt.batchSize {
				continue
			}

			drain()
		}
		drain()
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		default:
		}

		chunkBuf, err := ck.Chunk(rd)
		// Parses the rdf entries from the chunk, groups them into batches (each one
		// containing opt.batchSize entries) and sends the batches to the loader.reqs channel (see
		// above).
		if oerr := ck.Parse(chunkBuf); oerr != nil {
			return errors.Wrap(oerr, "During parsing chunk in processLoadFile")
		}
		if err == io.EOF {
			break
		} else {
			x.Check(err)
		}
	}
	nqbuf.Flush()
	return <-errCh
}

func setup(opts batchMutationOptions, dc *dgo.Dgraph, conf *viper.Viper) *loader {
	var db *badger.DB
	if len(opt.clientDir) > 0 {
		x.Check(os.MkdirAll(opt.clientDir, 0700))

		var err error
		db, err = badger.Open(badger.DefaultOptions(opt.clientDir).
			WithCompression(bopt.ZSTD).
			WithSyncWrites(false).
			WithBlockCacheSize(100 * (1 << 20)).
			WithIndexCacheSize(100 * (1 << 20)).
			WithZSTDCompressionLevel(3))
		x.Checkf(err, "Error while creating badger KV posting store")
	}

	dialOpts := []grpc.DialOption{}
	if conf.GetString("slash_grpc_endpoint") != "" && conf.IsSet("auth_token") {
		dialOpts = append(dialOpts, x.WithAuthorizationCredentials(conf.GetString("auth_token")))
	}

	xopts := xidmap.XidMapOptions{DB: db, DgClient: dc}
	alloc := xidmap.New(xopts)
	l := &loader{
		opts:       opts,
		dc:         dc,
		start:      time.Now(),
		reqs:       make(chan *request, opts.Pending*2),
		conflicts:  make(map[uint64]struct{}),
		alloc:      alloc,
		db:         db,
		namespaces: make(map[uint64]struct{}),
	}

	l.requestsWg.Add(opts.Pending)
	for range opts.Pending {
		go l.makeRequests()
	}

	return l
}

// populateNamespace fetches the schema and extracts the information about the existing namespaces.
func (l *loader) populateNamespaces(ctx context.Context, dc *dgo.Dgraph, singleNsOp bool) error {
	if singleNsOp {
		// The below schema query returns the predicates without the namespace if context does not
		// have the galaxy operation set. As we are not loading data across namespaces, so existence
		// of namespace is verified when the user logs in.
		l.namespaces[opt.namespaceToLoad] = struct{}{}
		return nil
	}

	txn := dc.NewTxn()
	defer func() {
		if err := txn.Discard(ctx); err != nil {
			glog.Warningf("error in discarding txn: %v", err)
		}
	}()

	res, err := txn.Query(ctx, "schema {}")
	if err != nil {
		return err
	}

	var sch Schema
	err = json.Unmarshal(res.GetJson(), &sch)
	if err != nil {
		return err
	}

	for _, pred := range sch.Predicates {
		ns := x.ParseNamespace(pred.Predicate)
		l.namespaces[ns] = struct{}{}
	}
	return nil
}

func run() error {
	creds := z.NewSuperFlag(Live.Conf.GetString("creds")).MergeAndCheckDefault(x.DefaultCreds)
	keys, err := x.GetEncAclKeys(Live.Conf)
	if err != nil {
		return err
	}

	x.PrintVersion()
	opt = options{
		dataFiles:       Live.Conf.GetString("files"),
		dataFormat:      Live.Conf.GetString("format"),
		schemaFile:      Live.Conf.GetString("schema"),
		concurrent:      Live.Conf.GetInt("conc"),
		batchSize:       Live.Conf.GetInt("batch"),
		clientDir:       Live.Conf.GetString("xidmap"),
		authToken:       Live.Conf.GetString("auth_token"),
		useCompression:  Live.Conf.GetBool("use_compression"),
		newUids:         Live.Conf.GetBool("new_uids"),
		verbose:         Live.Conf.GetBool("verbose"),
		httpAddr:        Live.Conf.GetString("http"),
		bufferSize:      Live.Conf.GetInt("bufferSize"),
		upsertPredicate: Live.Conf.GetString("upsertPredicate"),
		tmpDir:          Live.Conf.GetString("tmp"),
		key:             keys.EncKey,
	}

	forceNs := Live.Conf.GetInt64("force-namespace")
	switch creds.GetUint64("namespace") {
	case x.RootNamespace:
		if forceNs < 0 {
			opt.preserveNs = true
			opt.namespaceToLoad = math.MaxUint64
		} else {
			opt.namespaceToLoad = uint64(forceNs)
		}
	default:
		if Live.Conf.IsSet("force-namespace") {
			return errors.Errorf("cannot force namespace %#x when provided creds are not of"+
				" superadmin user", forceNs)
		}
	}

	z.SetTmpDir(opt.tmpDir)

	go func() {
		if err := http.ListenAndServe(opt.httpAddr, nil); err != nil {
			glog.Errorf("Error while starting HTTP server: %+v", err)
		}
	}()
	ctx := context.Background()
	// singleNsOp is set to false, when loading data into a namespace different from the one user
	// provided credentials for.
	singleNsOp := true
	if len(creds.GetString("user")) > 0 && creds.GetUint64("namespace") == x.RootNamespace &&
		opt.namespaceToLoad != x.RootNamespace {
		singleNsOp = false
	}
	rootNsOperation := false
	if !singleNsOp {
		// Attach the root namespace to the context to specify that the query/mutations with this context
		// will be root namespace wide.
		rootNsOperation = true
		ctx = x.AttachRootNsOperation(ctx, opt.namespaceToLoad)
		// We don't support upsert predicate while loading data in multiple namespace.
		if len(opt.upsertPredicate) > 0 {
			return errors.Errorf("Upsert Predicate feature is not supported for loading" +
				"into multiple namespaces.")
		}
	}

	bmOpts := batchMutationOptions{
		Size:          opt.batchSize,
		Pending:       opt.concurrent,
		PrintCounters: true,
		Ctx:           ctx,
		MaxRetries:    math.MaxUint32,
		bufferSize:    opt.bufferSize,
	}

	// Create directory for temporary buffers.
	x.Check(os.MkdirAll(opt.tmpDir, 0700))

	dg, closeFunc := x.GetDgraphClient(Live.Conf, true)
	defer closeFunc()

	l := setup(bmOpts, dg, Live.Conf)
	if err := l.populateNamespaces(ctx, dg, singleNsOp); err != nil {
		fmt.Printf("Error while populating namespaces %s\n", err)
		return err
	}

	if !opt.preserveNs {
		if _, ok := l.namespaces[opt.namespaceToLoad]; !ok {
			return errors.Errorf("Cannot load into namespace %#x. It does not exist.",
				opt.namespaceToLoad)
		}
	}

	if len(opt.schemaFile) > 0 {
		err := l.processSchemaFile(ctx, opt.schemaFile, opt.key, dg)
		if err != nil {
			if err == context.Canceled {
				fmt.Printf("Interrupted while processing schema file %q\n", opt.schemaFile)
				return nil
			}
			fmt.Printf("Error while processing schema file %q: %s\n", opt.schemaFile, err)
			return err
		}
		fmt.Printf("Processed schema file %q\n\n", opt.schemaFile)
	}

	if l.schema, err = getSchema(ctx, dg, rootNsOperation); err != nil {
		fmt.Printf("Error while loading schema from alpha %s\n", err)
		return err
	}

	if opt.dataFiles == "" {
		return errors.New("RDF or JSON file(s) location must be specified")
	}

	fs := filestore.NewFileStore(opt.dataFiles)

	filesList := fs.FindDataFiles(opt.dataFiles, []string{".rdf", ".rdf.gz", ".json", ".json.gz"})
	totalFiles := len(filesList)
	if totalFiles == 0 {
		return errors.Errorf("No data files found in %s", opt.dataFiles)
	}
	fmt.Printf("Found %d data file(s) to process\n", totalFiles)

	errCh := make(chan error, totalFiles)
	for _, file := range filesList {
		file = strings.Trim(file, " \t")
		go func(file string) {
			errCh <- errors.Wrap(l.processFile(ctx, fs, file, opt.key), file)
		}(file)
	}

	// PrintCounters should be called after schema has been updated.
	if bmOpts.PrintCounters {
		go l.printCounters()
	}

	for range totalFiles {
		if err := <-errCh; err != nil {
			fmt.Printf("Error while processing data file %s\n", err)
			return err
		}
	}

	close(l.reqs)
	// First we wait for requestsWg, when it is done we know all retry requests have been added
	// to retryRequestsWg. We can't have the same waitgroup as by the time we call Wait, we can't
	// be sure that all retry requests have been added to the waitgroup.
	l.requestsWg.Wait()
	l.retryRequestsWg.Wait()
	c := l.Counter()
	var rate uint64
	if c.Elapsed.Seconds() < 1 {
		rate = c.Nquads
	} else {
		rate = c.Nquads / uint64(c.Elapsed.Seconds())
	}
	// Lets print an empty line, otherwise Interrupted or Number of Mutations overwrites the
	// previous printed line.
	fmt.Printf("%100s\r", "")
	fmt.Printf("Number of TXs run            : %d\n", c.TxnsDone)
	fmt.Printf("Number of N-Quads processed  : %d\n", c.Nquads)
	fmt.Printf("Time spent                   : %v\n", c.Elapsed)
	fmt.Printf("N-Quads processed per second : %d\n", rate)

	if err := l.alloc.Flush(); err != nil {
		return err
	}
	if l.db != nil {
		if err := l.db.Close(); err != nil {
			return err
		}
	}
	return nil
}
