package dgraphimport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v250"

	api_v2 "github.com/dgraph-io/dgo/v250/protos/api.v2"
)

func TestExample(t *testing.T) {
	ctx := context.Background()
	dg, err := dgo.Open("dgraph://fk-25-dgraph-v25.hypermode-stage.host:443?sslmode=verify-ca&bearertoken=M2VXzn4YB^mC_9")
	if err != nil {
		t.Fatalf("failed to connect to Dgraph: %v", err)
	}
	defer dg.Close()

	for i := 0; i < 100000000; i++ {
		fmt.Println("pinging Dgraph:", i)
		_, err = dg.GetAPIv2Client()[0].Ping(ctx, &api_v2.PingRequest{})
		if err != nil {
			t.Fatalf("failed to ping Dgraph: %v", err)
		}

		time.Sleep(time.Second * 20)
	}

	// fmt.Println(dg.RunDQL(ctx, dgo.RootNamespace, `{
	// 	  caro(func: allofterms(name@en, "Marc Caro")) {
	// 		  name@en
	// 		  director.film {
	// 		    name@en
	// 		  }
	// 	  }
	// 	}`))

	// fmt.Println(dg.GetAPIv2Client()[0].UpdateExtSnapshotStreamingState(ctx,
	// 	&api_v2.UpdateExtSnapshotStreamingStateRequest{
	// 		Finish:   true,
	// 		DropData: true,
	// 	}))
	// t.FailNow()
}
