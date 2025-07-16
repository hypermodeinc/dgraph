/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dgraphimport

import (
	"context"
	"fmt"
	"os"

	"github.com/dgraph-io/badger/v4"
	"github.com/hypermodeinc/dgraph/v25/dgraph/cmd/bulk"
	"github.com/hypermodeinc/dgraph/v25/x"

	"github.com/spf13/cobra"
)

// Import is the sub-command invoked when running "dgraph import".
var ImportCmd x.SubCommand

func init() {
	ImportCmd.Cmd = &cobra.Command{
		Use:   "import",
		Short: "Run Dgraph Import",
		Run: func(cmd *cobra.Command, args []string) {
			run()
		},
		Annotations: map[string]string{"group": "data-load"},
	}
	ImportCmd.Cmd.SetHelpTemplate(x.NonRootTemplate)
	ImportCmd.EnvPrefix = "DGRAPH_IMPORT"

	flag := ImportCmd.Cmd.Flags()
	flag.StringP("files", "f", "", "Location of *.rdf(.gz) or *.json(.gz) file(s) to load.")
	flag.StringP("snapshot-dir", "p", "", "Location of p directory")
	flag.StringP("schema", "s", "", "Location of DQL schema file.")
	flag.StringP("graphql_schema", "g", "", "Location of the GraphQL schema file.")
	flag.StringP("graphql-schema", "", "", "Location of the GraphQL schema file.")
	flag.String("format", "", "Specify file format (rdf or json)")
	flag.Bool("drop-all", false, "Drops all the existing data in the cluster before importing data into Dgraph.")
	flag.Bool("drop-all-confirm", false, "Confirm drop-all operation.")
	flag.StringP("conn-str", "c", "", "Dgraph connection string.")
}

func run() {
	dropAll := ImportCmd.Conf.GetBool("drop-all")
	dropAllConfirm := ImportCmd.Conf.GetBool("drop-all-confirm")
	if dropAll && !dropAllConfirm {
		fmt.Println("Are you sure you want to drop all the existing data in the cluster?")
		fmt.Printf("If you are sure, this allows us to use dgraph bulk tool ")
		fmt.Println("which is much faster compared to using dgraph live tool.")
		fmt.Printf("Type 'YES' to confirm: ")

		var response string
		if _, err := fmt.Scan(&response); err != nil {
			fmt.Println("Aborting...")
			os.Exit(1)
		}

		if response != "YES" {
			fmt.Println("Aborting...")
			os.Exit(1)
		}

		dropAllConfirm = true
	}
	bulkLoad := dropAll && dropAllConfirm

	if !bulkLoad {
		fmt.Println("Live Loader is not supported right now!")
		os.Exit(1)
	}

	// if snapshot p directory is already provided, there is no need to run bulk loader
	if ImportCmd.Conf.GetString("snapshot-dir") != "" {
		connStr := ImportCmd.Conf.GetString("conn-str")
		snapshotDir := ImportCmd.Conf.GetString("snapshot-dir")
		if err := Import(context.Background(), connStr, snapshotDir); err != nil {
			fmt.Println("Failed to import data:", err)
			os.Exit(1)
		}
		return
	}

	cacheSize := 64 << 20 // These are the default values. User can overwrite them using --badger.
	cacheDefaults := fmt.Sprintf("indexcachesize=%d; blockcachesize=%d; ",
		(70*cacheSize)/100, (30*cacheSize)/100)
	bopts := badger.DefaultOptions("").FromSuperFlag(bulk.BulkBadgerDefaults + cacheDefaults).
		FromSuperFlag(ImportCmd.Conf.GetString("badger"))

	graphqlSchema := ImportCmd.Conf.GetString("graphql_schema")
	if graphqlSchema == "" {
		graphqlSchema = ImportCmd.Conf.GetString("graphql-schema")
	}

	// Run Bulk Loader
	opt := bulk.BulkOptions{
		DataFiles:        ImportCmd.Conf.GetString("files"),
		DataFormat:       ImportCmd.Conf.GetString("format"),
		SchemaFile:       ImportCmd.Conf.GetString("schema"),
		GqlSchemaFile:    graphqlSchema,
		Encrypted:        false,
		EncryptedOut:     false,
		OutDir:           "out",
		ReplaceOutDir:    true,
		TmpDir:           "tmp",
		NumGoroutines:    1,
		MapBufSize:       2048,
		PartitionBufSize: 4,
		SkipMapPhase:     false,
		CleanupTmp:       true,
		NumReducers:      1,
		StoreXids:        false,
		IgnoreErrors:     false,
		MapShards:        1,
		ReduceShards:     1,
		CustomTokenizers: "",
		NewUids:          false,
		ClientDir:        "",
		Namespace:        0,
		Badger:           bopts,
		EncryptionKey:    nil,
		ConnStr:          ImportCmd.Conf.GetString("conn-str"),
	}
	bulk.RunBulkLoader(opt)

	if err := Import(context.Background(), ImportCmd.Conf.GetString("conn-str"), "out"); err != nil {
		fmt.Println("Failed to import data:", err)
		os.Exit(1)
	}
}
