package edgraph

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/dgraph-io/badger/v4"
	apiv25 "github.com/dgraph-io/dgo/v240/protos/api.v25"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/golang/glog"
	"google.golang.org/grpc"
)

type Client struct {
	opts grpc.DialOption
	dg   apiv25.DgraphClient
}

func NewImportClient(endpoint string, opts grpc.DialOption) (*Client, error) {
	conn, err := grpc.NewClient(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to endpoint [%s]: %w", endpoint, err)
	}

	return &Client{dg: apiv25.NewDgraphClient(conn), opts: opts}, nil
}

func (c *Client) InitiateSnapShotStream(ctx context.Context) (*apiv25.InitiateSnapShotStreamResponse, error) {
	req := &apiv25.InitiateSnapShotStreamRequest{}
	return c.dg.InitiateSnapShotStream(ctx, req)
}

func (c *Client) StreamSnapshot(ctx context.Context, pDir string, alphas map[uint32]string) error {
	groupDirs, err := getPDiPdirrectories(pDir)
	if err != nil {
		return fmt.Errorf("Error getting p directories: %v", err)
	}

	for key, leader := range alphas {
		pDir, exists := groupDirs[key-1]
		if !exists {
			fmt.Printf("No p directory found for group %d, skipping...\n", key)
			continue
		}

		if _, err := os.Stat(pDir); os.IsNotExist(err) {
			fmt.Printf("p directory does not exist: %s, skipping...\n", pDir)
			continue
		}

		conn, err := grpc.NewClient(leader, c.opts)
		if err != nil {
			return fmt.Errorf("Failed to connect to leader %s: %v", leader, err)
		}
		defer conn.Close()

		dg := apiv25.NewDgraphClient(conn)
		err = stream(ctx, dg, pDir)
		if err != nil {
			return err
		}
	}

	return nil
}

func stream(ctx context.Context, dg apiv25.DgraphClient, pdir string) error {
	out, err := dg.StreamPSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("failed to start snapshot stream: %w", err)
	}

	opt := badger.DefaultOptions(pdir)
	ps, err := badger.OpenManaged(opt)
	if err != nil {
		return fmt.Errorf("failed to open BadgerDB: %w", err)
	}
	defer ps.Close()

	stream := ps.NewStreamAt(math.MaxUint64)
	stream.LogPrefix = "Sending P dir"
	stream.KeyToList = nil
	stream.Send = func(buf *z.Buffer) error {
		kvs := &apiv25.KVS{Data: buf.Bytes()}
		if err := out.Send(kvs); err != nil {
			return fmt.Errorf("failed to send data: %w", err)
		}
		return nil
	}

	if err := stream.Orchestrate(ctx); err != nil {
		return fmt.Errorf("stream orchestration failed: %w", err)
	}

	done := &apiv25.KVS{
		Done:       true,
		Predicates: []string{},
		Types:      []string{},
	}
	if err := out.Send(done); err != nil {
		return fmt.Errorf("failed to send 'done' signal: %w", err)
	}

	fmt.Println("Snapshot writes DONE. Sending ACK")
	ack, err := out.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("failed to receive ACK: %w", err)
	}
	glog.Infof("Received ACK with message: %v\n", ack.Message)

	return nil
}

func getPDiPdirrectories(basePath string) (map[uint32]string, error) {
	groupDirs := make(map[uint32]string)

	entries, err := os.ReadDir(basePath)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			groupID, err := strconv.Atoi(entry.Name())
			if err == nil {
				pDir := filepath.Join(basePath, entry.Name(), "p")
				if _, err := os.Stat(pDir); err == nil {
					groupDirs[uint32(groupID)] = pDir
				}
			}
		}
	}

	return groupDirs, nil
}
