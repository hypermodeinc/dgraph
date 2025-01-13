package dgraphimport

import (
	"context"
	"fmt"
	"os"

	"github.com/hypermodeinc/dgraph/v24/edgraph"
	"github.com/hypermodeinc/dgraph/v24/x"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	Import x.SubCommand
)

func init() {
	Import.Cmd = &cobra.Command{
		Use:   "import",
		Short: "Run the import tool",
		Run: func(cmd *cobra.Command, args []string) {
			run()
		},
		Annotations: map[string]string{"group": "tool"},
	}
	Import.Cmd.SetHelpTemplate(x.NonRootTemplate)
}

func run() {
	if err := importP(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func importP() error {
	client, err := edgraph.NewImportClient("localhost:9080", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}

	alphas, err := client.InitiateSnapShotStream(context.Background())
	if err := client.StreamSnapshot(context.Background(), "/home/shiva/workspace/dgraph-work/benchmarks/data/out", alphas.LeaderAlphas); err != nil {
		fmt.Println("error is---------", err)
	}

	return nil
}
