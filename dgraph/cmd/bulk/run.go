/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package bulk

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	_ "net/http/pprof" // http profiler
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/filestore"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/tok"
	"github.com/hypermodeinc/dgraph/v25/x"
)

// Bulk is the sub-command invoked when running "dgraph bulk".
var Bulk x.SubCommand

var defaultOutDir = "./out"

const BulkBadgerDefaults = "compression=snappy; numgoroutines=8;"

func init() {
	Bulk.Cmd = &cobra.Command{
		Use:   "bulk",
		Short: "Run Dgraph Bulk Loader",
		Run: func(cmd *cobra.Command, args []string) {
			defer x.StartProfile(Bulk.Conf).Stop()
			run()
		},
		Annotations: map[string]string{"group": "data-load"},
	}
	Bulk.Cmd.SetHelpTemplate(x.NonRootTemplate)
	Bulk.EnvPrefix = "DGRAPH_BULK"

	flag := Bulk.Cmd.Flags()
	flag.StringP("files", "f", "",
		"Location of *.rdf(.gz) or *.json(.gz) file(s) to load.")
	flag.StringP("schema", "s", "",
		"Location of schema file.")
	flag.StringP("graphql_schema", "g", "", "Location of the GraphQL schema file.")
	flag.String("format", "",
		"Specify file format (rdf or json) instead of getting it from filename.")
	flag.Bool("encrypted", false,
		"Flag to indicate whether schema and data files are encrypted. "+
			"Must be specified with --encryption or vault option(s).")
	flag.Bool("encrypted_out", false,
		"Flag to indicate whether to encrypt the output. "+
			"Must be specified with --encryption or vault option(s).")
	flag.String("out", defaultOutDir,
		"Location to write the final dgraph data directories.")
	flag.Bool("replace_out", false,
		"Replace out directory and its contents if it exists.")
	flag.String("tmp", "tmp",
		"Temp directory used to use for on-disk scratch space. Requires free space proportional"+
			" to the size of the RDF file and the amount of indexing used.")

	flag.IntP("num_go_routines", "j", int(math.Ceil(float64(runtime.NumCPU())/4.0)),
		"Number of worker threads to use. MORE THREADS LEAD TO HIGHER RAM USAGE.")
	flag.Int64("mapoutput_mb", 2048,
		"The estimated size of each map file output. Increasing this increases memory usage.")
	flag.Int64("partition_mb", 4, "Pick a partition key every N megabytes of data.")
	flag.Bool("skip_map_phase", false,
		"Skip the map phase (assumes that map output files already exist).")
	flag.Bool("cleanup_tmp", true,
		"Clean up the tmp directory after the loader finishes. Setting this to false allows the"+
			" bulk loader can be re-run while skipping the map phase.")
	flag.Int("reducers", 1,
		"Number of reducers to run concurrently. Increasing this can improve performance, and "+
			"must be less than or equal to the number of reduce shards.")
	flag.Bool("version", false, "Prints the version of Dgraph Bulk Loader.")
	flag.Bool("store_xids", false, "Generate an xid edge for each node.")
	flag.StringP("zero", "z", "localhost:5080", "gRPC address for Dgraph zero")
	flag.String("xidmap", "", "Directory to store xid to uid mapping")
	// TODO: Potentially move http server to main.
	flag.String("http", "localhost:8080", "Address to serve http (pprof).")
	flag.Bool("ignore_errors", false, "ignore line parsing errors in rdf files")
	flag.Int("map_shards", 1,
		"Number of map output shards. Must be greater than or equal to the number of reduce "+
			"shards. Increasing allows more evenly sized reduce shards, at the expense of "+
			"increased memory usage.")
	flag.Int("reduce_shards", 1,
		"Number of reduce shards. This determines the number of dgraph instances in the final "+
			"cluster. Increasing this potentially decreases the reduce stage runtime by using "+
			"more parallelism, but increases memory usage.")
	flag.String("custom_tokenizers", "",
		"Comma separated list of tokenizer plugins")
	flag.Bool("new_uids", false,
		"Ignore UIDs in load files and assign new ones.")
	flag.Uint64("force-namespace", math.MaxUint64,
		"Namespace onto which to load the data. If not set, will preserve the namespace."+
			" When using this flag to load data into specific namespace, make sure that the "+
			"load data do not have ACL data.")

	flag.String("badger", BulkBadgerDefaults, z.NewSuperFlagHelp(BulkBadgerDefaults).
		Head("Badger options (Refer to badger documentation for all possible options)").
		Flag("compression",
			"Specifies the compression algorithm and compression level (if applicable) for the "+
				`postings directory. "none" would disable compression, while "zstd:1" would set `+
				"zstd compression at level 1.").
		Flag("numgoroutines",
			"The number of goroutines to use in badger.Stream.").
		String())

	x.RegisterClientTLSFlags(flag)
	// Encryption and Vault options
	x.RegisterEncFlag(flag)
}

func run() {
	cacheSize := 64 << 20 // These are the default values. User can overwrite them using --badger.
	cacheDefaults := fmt.Sprintf("indexcachesize=%d; blockcachesize=%d; ",
		(70*cacheSize)/100, (30*cacheSize)/100)

	bopts := badger.DefaultOptions("").FromSuperFlag(BulkBadgerDefaults + cacheDefaults).
		FromSuperFlag(Bulk.Conf.GetString("badger"))
	keys, err := x.GetEncAclKeys(Bulk.Conf)
	x.Check(err)

	opt := BulkOptions{
		DataFiles:        Bulk.Conf.GetString("files"),
		DataFormat:       Bulk.Conf.GetString("format"),
		EncryptionKey:    keys.EncKey,
		SchemaFile:       Bulk.Conf.GetString("schema"),
		GqlSchemaFile:    Bulk.Conf.GetString("graphql_schema"),
		Encrypted:        Bulk.Conf.GetBool("encrypted"),
		EncryptedOut:     Bulk.Conf.GetBool("encrypted_out"),
		OutDir:           Bulk.Conf.GetString("out"),
		ReplaceOutDir:    Bulk.Conf.GetBool("replace_out"),
		TmpDir:           Bulk.Conf.GetString("tmp"),
		NumGoroutines:    Bulk.Conf.GetInt("num_go_routines"),
		MapBufSize:       uint64(Bulk.Conf.GetInt("mapoutput_mb")),
		PartitionBufSize: int64(Bulk.Conf.GetInt("partition_mb")),
		SkipMapPhase:     Bulk.Conf.GetBool("skip_map_phase"),
		CleanupTmp:       Bulk.Conf.GetBool("cleanup_tmp"),
		NumReducers:      Bulk.Conf.GetInt("reducers"),
		Version:          Bulk.Conf.GetBool("version"),
		StoreXids:        Bulk.Conf.GetBool("store_xids"),
		ZeroAddr:         Bulk.Conf.GetString("zero"),
		HttpAddr:         Bulk.Conf.GetString("http"),
		IgnoreErrors:     Bulk.Conf.GetBool("ignore_errors"),
		MapShards:        Bulk.Conf.GetInt("map_shards"),
		ReduceShards:     Bulk.Conf.GetInt("reduce_shards"),
		CustomTokenizers: Bulk.Conf.GetString("custom_tokenizers"),
		NewUids:          Bulk.Conf.GetBool("new_uids"),
		ClientDir:        Bulk.Conf.GetString("xidmap"),
		Namespace:        Bulk.Conf.GetUint64("force-namespace"),
		Badger:           bopts,
	}

	x.PrintVersion()
	if opt.Version {
		os.Exit(0)
	}

	RunBulkLoader(opt)
}

func RunBulkLoader(opt BulkOptions) {
	if len(opt.EncryptionKey) == 0 {
		if opt.Encrypted || opt.EncryptedOut {
			fmt.Fprint(os.Stderr, "Must use --encryption or vault option(s).\n")
			os.Exit(1)
		}
	} else {
		requiredFlags := Bulk.Cmd.Flags().Changed("encrypted") &&
			Bulk.Cmd.Flags().Changed("encrypted_out")
		if !requiredFlags {
			fmt.Fprint(os.Stderr,
				"Must specify --encrypted and --encrypted_out when providing encryption key.\n")
			os.Exit(1)
		}
		if !opt.Encrypted && !opt.EncryptedOut {
			fmt.Fprint(os.Stderr,
				"Must set --encrypted and/or --encrypted_out to true when providing encryption key.\n")
			os.Exit(1)
		}

		tlsConf, err := x.LoadClientTLSConfigForInternalPort(Bulk.Conf)
		x.Check(err)
		// Need to set zero addr in WorkerConfig before checking the license.
		x.WorkerConfig.ZeroAddr = []string{opt.ZeroAddr}
		x.WorkerConfig.TLSClientConfig = tlsConf
		log.Printf("Encryption feature enabled.")
	}
	fmt.Printf("Encrypted input: %v; Encrypted output: %v\n", opt.Encrypted, opt.EncryptedOut)

	if opt.SchemaFile == "" {
		// if only graphql schema is provided, we can generate DQL schema from it.
		if opt.GqlSchemaFile == "" {
			fmt.Fprint(os.Stderr, "Schema file must be specified.\n")
			os.Exit(1)
		}
	} else {
		if !filestore.Exists(opt.SchemaFile) {
			fmt.Fprintf(os.Stderr, "Schema path(%v) does not exist.\n", opt.SchemaFile)
			os.Exit(1)
		}
	}
	if opt.DataFiles == "" {
		fmt.Fprint(os.Stderr, "RDF or JSON file(s) location must be specified.\n")
		os.Exit(1)
	} else {
		fileList := strings.SplitSeq(opt.DataFiles, ",")
		for file := range fileList {
			if !filestore.Exists(file) {
				fmt.Fprintf(os.Stderr, "Data path(%v) does not exist.\n", file)
				os.Exit(1)
			}
		}
	}

	if opt.ReduceShards > opt.MapShards {
		fmt.Fprintf(os.Stderr, "Invalid flags: reduce_shards(%d) should be <= map_shards(%d)\n",
			opt.ReduceShards, opt.MapShards)
		os.Exit(1)
	}
	if opt.NumReducers > opt.ReduceShards {
		fmt.Fprintf(os.Stderr, "Invalid flags: shufflers(%d) should be <= reduce_shards(%d)\n",
			opt.NumReducers, opt.ReduceShards)
		os.Exit(1)
	}
	if opt.CustomTokenizers != "" {
		for _, soFile := range strings.Split(opt.CustomTokenizers, ",") {
			tok.LoadCustomTokenizer(soFile)
		}
	}
	if opt.MapBufSize <= 0 || opt.PartitionBufSize <= 0 {
		fmt.Fprintf(os.Stderr, "mapoutput_mb: %d and partition_mb: %d must be greater than zero\n",
			opt.MapBufSize, opt.PartitionBufSize)
		os.Exit(1)
	}

	opt.MapBufSize <<= 20       // Convert from MB to B.
	opt.PartitionBufSize <<= 20 // Convert from MB to B.

	optBuf, err := json.MarshalIndent(&opt, "", "\t")
	x.Check(err)
	fmt.Println(string(optBuf))

	maxOpenFilesWarning()

	go func() {
		log.Fatal(http.ListenAndServe(opt.HttpAddr, nil))
	}()
	http.HandleFunc("/jemalloc", x.JemallocHandler)

	// Make sure it's OK to create or replace the directory specified with the --out option.
	// It is always OK to create or replace the default output directory.
	if opt.OutDir != defaultOutDir && !opt.ReplaceOutDir {
		err := x.IsMissingOrEmptyDir(opt.OutDir)
		if err == nil {
			fmt.Fprintf(os.Stderr, "Output directory exists and is not empty."+
				" Use --replace_out to overwrite it.\n")
			os.Exit(1)
		} else if err != x.ErrMissingDir {
			x.CheckfNoTrace(err)
		}
	}

	// Delete and recreate the output dirs to ensure they are empty.
	x.Check(os.RemoveAll(opt.OutDir))
	for i := range opt.ReduceShards {
		dir := filepath.Join(opt.OutDir, strconv.Itoa(i), "p")
		x.Check(os.MkdirAll(dir, 0700))
		opt.shardOutputDirs = append(opt.shardOutputDirs, dir)

		x.Check(x.WriteGroupIdFile(dir, uint32(i+1)))
	}

	// Create a directory just for bulk loader's usage.
	if !opt.SkipMapPhase {
		x.Check(os.RemoveAll(opt.TmpDir))
		x.Check(os.MkdirAll(opt.TmpDir, 0700))
	}
	if opt.CleanupTmp {
		defer os.RemoveAll(opt.TmpDir)
	}

	// Create directory for temporary buffers used in map-reduce phase
	bufDir := filepath.Join(opt.TmpDir, bufferDir)
	x.Check(os.RemoveAll(bufDir))
	x.Check(os.MkdirAll(bufDir, 0700))
	defer os.RemoveAll(bufDir)

	loader := newLoader(&opt)

	const bulkMetaFilename = "bulk.meta"
	bulkMetaPath := filepath.Join(opt.TmpDir, bulkMetaFilename)

	if opt.SkipMapPhase {
		bulkMetaData, err := os.ReadFile(bulkMetaPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error reading from bulk meta file")
			os.Exit(1)
		}

		var bulkMeta pb.BulkMeta
		if err = proto.Unmarshal(bulkMetaData, &bulkMeta); err != nil {
			fmt.Fprintln(os.Stderr, "Error deserializing bulk meta file")
			os.Exit(1)
		}

		loader.prog.mapEdgeCount = bulkMeta.EdgeCount
		loader.schema.schemaMap = bulkMeta.SchemaMap
		loader.schema.types = bulkMeta.Types
	} else {
		loader.mapStage()
		mergeMapShardsIntoReduceShards(&opt)
		loader.leaseNamespaces()

		bulkMeta := pb.BulkMeta{
			EdgeCount: loader.prog.mapEdgeCount,
			SchemaMap: loader.schema.schemaMap,
			Types:     loader.schema.types,
		}
		bulkMetaData, err := proto.Marshal(&bulkMeta)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error serializing bulk meta file")
			os.Exit(1)
		}
		if err = os.WriteFile(bulkMetaPath, bulkMetaData, 0600); err != nil {
			fmt.Fprintln(os.Stderr, "Error writing to bulk meta file")
			os.Exit(1)
		}
	}
	loader.reduceStage()
	loader.writeSchema()
	loader.cleanup()
}

func maxOpenFilesWarning() {
	const (
		red    = "\x1b[31m"
		green  = "\x1b[32m"
		yellow = "\x1b[33m"
		reset  = "\x1b[0m"
	)
	maxOpenFiles, err := x.QueryMaxOpenFiles()
	if err != nil || maxOpenFiles < 1e6 {
		fmt.Println(green + "\nThe bulk loader needs to open many files at once. This number depends" +
			" on the size of the data set loaded, the map file output size, and the level" +
			" of indexing. 100,000 is adequate for most data set sizes. See `man ulimit` for" +
			" details of how to change the limit.")
		if err != nil {
			fmt.Printf(red+"Nonfatal error: max open file limit could not be detected: %v\n"+reset, err)
		} else {
			fmt.Printf(yellow+"Current max open files limit: %d\n"+reset, maxOpenFiles)
		}
		fmt.Println()
	}
}
