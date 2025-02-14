package dgraphimport

import (
	"fmt"
	"log"
	"os"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
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
	// Create a new dgo client to talk with the dgraph server
	conn, err := grpc.NewClient("localhost:9080", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	dg := dgo.NewDgraphClient(api.NewDgraphClient(conn))

	// here we are getting alpha leader request so now we will need to stream to
	// desired alpha only
	alphs, err := dg.InitiateSnapShotStream()
	fmt.Println("alphas are----------->", alphs)
	if err != nil {
		return err
	}

	alphaLeader := "localhost:9080"
	conn, err = grpc.NewClient(alphaLeader, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	// worker.Snapshot

	dg1 := dgo.NewDgraphClient(api.NewDgraphClient(conn))

	if err := dg1.StreamSnapshot("/home/shiva/workspace/dgraph-work/stream_data/p"); err != nil {
		log.Fatal()
	}

	// here we get a list of alphas now which we have to send stream of lpahs back

	// add new rpc whcih can stram p to alpha

	// now we have dgraph client which can tell server that p dir is ready to proced further

	return nil
}
