/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dgraphimport

import (
	"fmt"
	_ "net/http/pprof" // http profiler
	"os"

	"github.com/spf13/cobra"

	"github.com/hypermodeinc/dgraph/v25/x"
)

// Import is the sub-command invoked when running "dgraph import".
var Import x.SubCommand

func init() {
	Import.Cmd = &cobra.Command{
		Use:   "import",
		Short: "Run Dgraph Import",
		Run: func(cmd *cobra.Command, args []string) {
			defer x.StartProfile(Import.Conf).Stop()
			run()
		},
		Annotations: map[string]string{"group": "data-load"},
	}
	Import.Cmd.SetHelpTemplate(x.NonRootTemplate)
	Import.EnvPrefix = "DGRAPH_IMPORT"

	flag := Import.Cmd.Flags()
	flag.StringP("files", "f", "", "Location of *.rdf(.gz) or *.json(.gz) file(s) to load.")
	flag.StringP("schema", "s", "", "Location of DQL schema file.")
	flag.StringP("graphql-schema", "g", "", "Location of the GraphQL schema file.")
	flag.String("format", "", "Specify file format (rdf or json)")
	flag.Bool("drop-all", false, "Drops all the existing data in the cluster before importing data into Dgraph.")
	flag.Bool("drop-all-confirm", false, "Confirm drop-all operation.")

	x.RegisterClientTLSFlags(flag)
	x.RegisterEncFlag(flag)
}

func run() {
	dropAll := Import.Conf.GetBool("drop-all")
	dropAllConfirm := Import.Conf.GetBool("drop-all-confirm")
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

	// We are going to run either dgraph live OR
	// dgraph bulk + importing external snapshot depending upon the flags provided.
	// Make sure it works with Dgraph Cloud, Hypermode Cloud and local clusters
	//
	//

	if !bulkLoad {
		// Run Live Loader
		liveLoader := x.NewLiveLoader(Import.Conf)
		liveLoader.Run()
		return
	}

	// Run Bulk Loader
	bulkLoader := x.NewBulkLoader(Import.Conf)
	bulkLoader.Run()
}
