/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package bulk

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"hash/adler32"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/golang/glog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/y"
	"github.com/hypermodeinc/dgraph/v25/chunker"
	"github.com/hypermodeinc/dgraph/v25/enc"
	"github.com/hypermodeinc/dgraph/v25/filestore"
	gqlSchema "github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/x"
	"github.com/hypermodeinc/dgraph/v25/xidmap"
)

type options struct {
	DataFiles        string
	DataFormat       string
	SchemaFile       string
	GqlSchemaFile    string
	OutDir           string
	ReplaceOutDir    bool
	TmpDir           string
	NumGoroutines    int
	MapBufSize       uint64
	PartitionBufSize int64
	SkipMapPhase     bool
	CleanupTmp       bool
	NumReducers      int
	Version          bool
	StoreXids        bool
	ZeroAddr         string
	HttpAddr         string
	IgnoreErrors     bool
	CustomTokenizers string
	NewUids          bool
	ClientDir        string
	Encrypted        bool
	EncryptedOut     bool

	MapShards    int
	ReduceShards int

	Namespace uint64

	shardOutputDirs []string

	// ........... Badger options ..........
	// EncryptionKey is the key used for encryption.
	EncryptionKey x.Sensitive
	// Badger options.
	Badger badger.Options
}

type state struct {
	opt           *options
	prog          *progress
	xids          *xidmap.XidMap
	schema        *schemaStore
	shards        *shardMap
	readerChunkCh chan *bytes.Buffer
	mapFileId     uint32 // Used atomically to name the output files of the mappers.
	dbs           []*badger.DB
	tmpDbs        []*badger.DB // Temporary DB to write the split lists to avoid ordering issues.
	writeTs       uint64       // All badger writes use this timestamp
	namespaces    *sync.Map    // To store the encountered namespaces.
}

type loader struct {
	*state
	mappers []*mapper
	zero    *grpc.ClientConn
}

func newLoader(opt *options) *loader {
	if opt == nil {
		log.Fatalf("Cannot create loader with nil options.")
	}

	fmt.Printf("Connecting to zero at %s\n", opt.ZeroAddr)

	tlsConf, err := x.LoadClientTLSConfigForInternalPort(Bulk.Conf)
	x.Check(err)
	dialOpts := []grpc.DialOption{}
	if tlsConf != nil {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConf)))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	zero, err := grpc.NewClient(opt.ZeroAddr, dialOpts...)
	x.Checkf(err, "Unable to connect to zero, Is it running at %s?", opt.ZeroAddr)
	st := &state{
		opt:    opt,
		prog:   newProgress(),
		shards: newShardMap(opt.MapShards),
		// Lots of gz readers, so not much channel buffer needed.
		readerChunkCh: make(chan *bytes.Buffer, opt.NumGoroutines),
		writeTs:       getWriteTimestamp(zero),
		namespaces:    &sync.Map{},
	}
	st.schema = newSchemaStore(readSchema(opt), opt, st)
	ld := &loader{
		state:   st,
		mappers: make([]*mapper, opt.NumGoroutines),
		zero:    zero,
	}
	for i := range opt.NumGoroutines {
		ld.mappers[i] = newMapper(st)
	}
	go ld.prog.report()
	return ld
}

func getWriteTimestamp(zero *grpc.ClientConn) uint64 {
	client := pb.NewZeroClient(zero)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ts, err := client.Timestamps(ctx, &pb.Num{Val: 1})
		cancel()
		if err == nil {
			return ts.GetStartId()
		}
		fmt.Printf("Error communicating with dgraph zero, retrying: %v", err)
		time.Sleep(time.Second)
	}
}

// leaseNamespace is called at the end of map phase. It leases the namespace ids till the maximum
// seen namespace id.
func (ld *loader) leaseNamespaces() {
	var maxNs uint64
	ld.namespaces.Range(func(key, value interface{}) bool {
		if ns := key.(uint64); ns > maxNs {
			maxNs = ns
		}
		return true
	})

	// If only the default namespace is seen, do nothing.
	if maxNs == 0 {
		return
	}

	client := pb.NewZeroClient(ld.zero)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ns, err := client.AssignIds(ctx, &pb.Num{Val: maxNs, Type: pb.Num_NS_ID})
		cancel()
		if err == nil {
			fmt.Printf("Assigned namespaces till %d\n", ns.GetEndId())
			return
		}
		fmt.Printf("Error communicating with dgraph zero, retrying: %v", err)
		time.Sleep(time.Second)
	}
}

func readSchema(opt *options) *schema.ParsedSchema {
	if opt.SchemaFile == "" {
		return genDQLSchema(opt)
	}

	f, err := filestore.Open(opt.SchemaFile)
	x.Check(err)
	defer func() {
		if err := f.Close(); err != nil {
			glog.Warningf("error while closing fd: %v", err)
		}
	}()

	key := opt.EncryptionKey
	if !opt.Encrypted {
		key = nil
	}
	r, err := enc.GetReader(key, f)
	x.Check(err)
	if filepath.Ext(opt.SchemaFile) == ".gz" {
		r, err = gzip.NewReader(r)
		x.Check(err)
	}

	buf, err := io.ReadAll(r)
	x.Check(err)

	result, err := schema.ParseWithNamespace(string(buf), opt.Namespace)
	x.Check(err)
	return result
}

func genDQLSchema(opt *options) *schema.ParsedSchema {
	gqlSchBytes := readGqlSchema(opt)
	nsToSchemas := parseGqlSchema(string(gqlSchBytes))

	var finalSch schema.ParsedSchema
	for ns, gqlSch := range nsToSchemas {
		if opt.Namespace != math.MaxUint64 {
			ns = opt.Namespace
		}

		h, err := gqlSchema.NewHandler(gqlSch, false)
		x.Check(err)

		_, err = gqlSchema.FromString(h.GQLSchema(), ns)
		x.Check(err)

		ps, err := schema.ParseWithNamespace(h.DGSchema(), ns)
		x.Check(err)

		finalSch.Preds = append(finalSch.Preds, ps.Preds...)
		finalSch.Types = append(finalSch.Types, ps.Types...)
	}

	return &finalSch
}

func (ld *loader) mapStage() {
	ld.prog.setPhase(mapPhase)
	var db *badger.DB
	if len(ld.opt.ClientDir) > 0 {
		x.Check(os.MkdirAll(ld.opt.ClientDir, 0700))

		var err error
		db, err = badger.Open(badger.DefaultOptions(ld.opt.ClientDir))
		x.Checkf(err, "Error while creating badger KV posting store")
	}
	ld.xids = xidmap.New(xidmap.XidMapOptions{
		UidAssigner: ld.zero,
		DB:          db,
		Dir:         filepath.Join(ld.opt.TmpDir, bufferDir),
	})

	fs := filestore.NewFileStore(ld.opt.DataFiles)

	files := fs.FindDataFiles(ld.opt.DataFiles, []string{".rdf", ".rdf.gz", ".json", ".json.gz"})
	if len(files) == 0 {
		fmt.Printf("No data files found in %s.\n", ld.opt.DataFiles)
		os.Exit(1)
	}

	// Because mappers must handle chunks that may be from different input files, they must all
	// assume the same data format, either RDF or JSON. Use the one specified by the user or by
	// the first load file.
	loadType := chunker.DataFormat(files[0], ld.opt.DataFormat)
	if loadType == chunker.UnknownFormat {
		// Dont't try to detect JSON input in bulk loader.
		fmt.Printf("Need --format=rdf or --format=json to load %s", files[0])
		os.Exit(1)
	}

	var mapperWg sync.WaitGroup
	mapperWg.Add(len(ld.mappers))
	for _, m := range ld.mappers {
		go func(m *mapper) {
			m.run(loadType)
			mapperWg.Done()
		}(m)
	}

	// This is the main map loop.
	thr := y.NewThrottle(ld.opt.NumGoroutines)
	for i, file := range files {
		x.Check(thr.Do())
		fmt.Printf("Processing file (%d out of %d): %s\n", i+1, len(files), file)

		go func(file string) {
			defer thr.Done(nil)

			key := ld.opt.EncryptionKey
			if !ld.opt.Encrypted {
				key = nil
			}
			r, cleanup := fs.ChunkReader(file, key)
			defer cleanup()

			chunk := chunker.NewChunker(loadType, 1000)
			for {
				chunkBuf, err := chunk.Chunk(r)
				if chunkBuf != nil && chunkBuf.Len() > 0 {
					ld.readerChunkCh <- chunkBuf
				}
				if err == io.EOF {
					break
				} else if err != nil {
					x.Check(err)
				}
			}
		}(file)
	}
	x.Check(thr.Finish())

	// Send the graphql triples
	ld.processGqlSchema(loadType)

	close(ld.readerChunkCh)
	mapperWg.Wait()

	// Allow memory to GC before the reduce phase.
	for i := range ld.mappers {
		ld.mappers[i] = nil
	}
	x.Check(ld.xids.Flush())
	if db != nil {
		x.Check(db.Close())
	}
	ld.xids = nil
}

func parseGqlSchema(s string) map[uint64]string {
	var schemas []x.ExportedGQLSchema
	if err := json.Unmarshal([]byte(s), &schemas); err != nil {
		fmt.Println("Error while decoding the graphql schema. Assuming it to be in format < 21.03.")
		return map[uint64]string{x.RootNamespace: s}
	}

	schemaMap := make(map[uint64]string)
	for _, schema := range schemas {
		if _, ok := schemaMap[schema.Namespace]; ok {
			fmt.Printf("Found multiple GraphQL schema for namespace %d.", schema.Namespace)
			continue
		}
		schemaMap[schema.Namespace] = schema.Schema
	}
	return schemaMap
}

func readGqlSchema(opt *options) []byte {
	f, err := filestore.Open(opt.GqlSchemaFile)
	x.Check(err)
	defer func() {
		if err := f.Close(); err != nil {
			glog.Warningf("error while closing fd: %v", err)
		}
	}()

	key := opt.EncryptionKey
	if !opt.Encrypted {
		key = nil
	}
	r, err := enc.GetReader(key, f)
	x.Check(err)
	if filepath.Ext(opt.GqlSchemaFile) == ".gz" {
		r, err = gzip.NewReader(r)
		x.Check(err)
	}

	buf, err := io.ReadAll(r)
	x.Check(err)
	return buf
}

func (ld *loader) processGqlSchema(loadType chunker.InputFormat) {
	if ld.opt.GqlSchemaFile == "" {
		return
	}

	rdfSchema := `_:gqlschema <dgraph.type> "dgraph.graphql" <%#x> .
	_:gqlschema <dgraph.graphql.xid> "dgraph.graphql.schema" <%#x> .
	_:gqlschema <dgraph.graphql.schema> %s <%#x> .
	`

	jsonSchema := `{
		"namespace": "%#x",
		"dgraph.type": "dgraph.graphql",
		"dgraph.graphql.xid": "dgraph.graphql.schema",
		"dgraph.graphql.schema": %s
	}`

	process := func(ns uint64, schema string) {
		// Ignore the schema if the namespace is not already seen.
		if _, ok := ld.schema.namespaces.Load(ns); !ok {
			fmt.Printf("No data exist for namespace: %d. Cannot load the graphql schema.", ns)
			return
		}
		gqlBuf := &bytes.Buffer{}
		schema = strconv.Quote(schema)
		switch loadType {
		case chunker.RdfFormat:
			_, err := fmt.Fprintf(gqlBuf, rdfSchema, ns, ns, schema, ns)
			x.Check(err)
		case chunker.JsonFormat:
			_, err := fmt.Fprintf(gqlBuf, jsonSchema, ns, schema)
			x.Check(err)
		}
		ld.readerChunkCh <- gqlBuf
	}

	buf := readGqlSchema(ld.opt)
	schemas := parseGqlSchema(string(buf))
	if ld.opt.Namespace == math.MaxUint64 {
		// Preserve the namespace.
		for ns, schema := range schemas {
			process(ns, schema)
		}
		return
	}

	switch len(schemas) {
	case 1:
		// User might have exported from a different namespace. So, schema.Namespace will not be
		// having the correct value.
		for _, schema := range schemas {
			process(ld.opt.Namespace, schema)
		}
	default:
		if _, ok := schemas[ld.opt.Namespace]; !ok {
			// We expect only a single GraphQL schema when loading into specific namespace.
			fmt.Printf("Didn't find GraphQL schema for namespace %d. Not loading GraphQL schema.",
				ld.opt.Namespace)
			return
		}
		process(ld.opt.Namespace, schemas[ld.opt.Namespace])
	}
}

func (ld *loader) reduceStage() {
	ld.prog.setPhase(reducePhase)

	r := reducer{
		state:     ld.state,
		streamIds: make(map[string]uint32),
	}
	x.Check(r.run())
}

func (ld *loader) writeSchema() {
	numDBs := uint32(len(ld.dbs))
	preds := make([][]string, numDBs)

	// Get all predicates that have data in some DB.
	m := make(map[string]struct{})
	for i, db := range ld.dbs {
		preds[i] = ld.schema.getPredicates(db)
		for _, p := range preds[i] {
			m[p] = struct{}{}
		}
	}

	// Find any predicates that don't have data in any DB
	// and distribute them among all the DBs.
	for p := range ld.schema.schemaMap {
		if _, ok := m[p]; !ok {
			i := adler32.Checksum([]byte(p)) % numDBs
			preds[i] = append(preds[i], p)
		}
	}

	// Write out each DB's final predicate list.
	for i, db := range ld.dbs {
		ld.schema.write(db, preds[i])
	}
}

func (ld *loader) cleanup() {
	for _, db := range ld.dbs {
		x.Check(db.Close())
	}
	for _, db := range ld.tmpDbs {
		opts := db.Opts()
		x.Check(db.Close())
		x.Check(os.RemoveAll(opts.Dir))
	}
	ld.prog.endSummary()
}
