/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dgraphimport

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/dgraph-io/badger/v4"
	apiv25 "github.com/dgraph-io/dgo/v250/protos/api.v25"
	"github.com/dgraph-io/ristretto/v2/z"

	"github.com/golang/glog"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

// NewClient creates a new import client with the specified endpoint and gRPC options.
func newClient(endpoint string, opts grpc.DialOption) (apiv25.DgraphClient, error) {
	conn, err := grpc.NewClient(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to endpoint [%s]: %w", endpoint, err)
	}

	glog.Infof("Successfully connected to Dgraph endpoint: %s", endpoint)
	return apiv25.NewDgraphClient(conn), nil
}

func Import(ctx context.Context, endpoint string, opts grpc.DialOption, bulkOutDir string) error {
	dg, err := newClient(endpoint, opts)
	if err != nil {
		return err
	}
	resp, err := startSnapshotStream(ctx, dg)
	if err != nil {
		return err
	}

	return sendSnapshot(ctx, dg, bulkOutDir, resp.Groups)
}

// StartSnapshotStream initiates a snapshot stream session with the Dgraph server.
func startSnapshotStream(ctx context.Context, dc apiv25.DgraphClient) (*apiv25.InitiateSnapshotStreamResponse, error) {
	glog.V(2).Infof("Initiating snapshot stream")
	req := &apiv25.InitiateSnapshotStreamRequest{}
	resp, err := dc.InitiateSnapshotStream(ctx, req)
	if err != nil {
		glog.Errorf("Failed to initiate snapshot stream: %v", err)
		return nil, err
	}
	glog.Infof("Snapshot stream initiated successfully")
	return resp, nil
}

// SendSnapshot takes a p directory and a set of group IDs and streams the data from the
// p directory to the corresponding group IDs. It first scans the provided directory for
// subdirectories named with numeric group IDs.
func sendSnapshot(ctx context.Context, dg apiv25.DgraphClient, baseDir string, groups []uint32) error {
	glog.Infof("Starting to stream snapshots from directory: %s", baseDir)

	errG, ctx := errgroup.WithContext(ctx)
	for _, group := range groups {
		group := group
		errG.Go(func() error {
			pDir := filepath.Join(baseDir, fmt.Sprintf("%d", group-1), "p")
			if _, err := os.Stat(pDir); err != nil {
				return fmt.Errorf("p directory does not exist for group %d: %s", group, pDir)
			}

			glog.Infof("Streaming data for group %d from directory: %s", group, pDir)
			if err := streamData(ctx, dg, pDir, group); err != nil {
				return err
			}

			return nil
		})
	}
	if err := errG.Wait(); err != nil {
		return err
	}

	glog.Infof("Completed streaming all snapshots")
	return nil
}

// streamData handles the actual data streaming process for a single group.
// It opens the BadgerDB at the specified directory and streams all data to the server.
func streamData(ctx context.Context, dg apiv25.DgraphClient, pdir string, groupId uint32) error {
	glog.Infof("Opening stream for group %d from directory %s", groupId, pdir)

	// Initialize stream with the server
	out, err := dg.StreamSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("failed to start snapshot stream: %w", err)
	}

	// Open the BadgerDB instance at the specified directory
	opt := badger.DefaultOptions(pdir)
	ps, err := badger.OpenManaged(opt)
	if err != nil {
		return fmt.Errorf("failed to open BadgerDB at %s: %w", pdir, err)
	}
	defer ps.Close()

	// Send group ID as the first message in the stream
	glog.V(2).Infof("Sending group ID %d to server", groupId)
	groupReq := &apiv25.StreamSnapshotRequest{GroupId: groupId}
	if err := out.Send(groupReq); err != nil {
		return fmt.Errorf("failed to send group ID %d: %w", groupId, err)
	}

	// Configure and start the BadgerDB stream
	glog.V(2).Infof("Starting BadgerDB stream for group %d", groupId)
	stream := ps.NewStreamAt(math.MaxUint64)
	stream.LogPrefix = fmt.Sprintf("Sending P dir (group %d)", groupId)
	stream.KeyToList = nil
	stream.Send = func(buf *z.Buffer) error {
		kvs := &apiv25.KVS{Data: buf.Bytes()}
		if err := out.Send(&apiv25.StreamSnapshotRequest{Pairs: kvs}); err != nil && err != io.EOF {
			return fmt.Errorf("failed to send data chunk: %w", err)
		}
		return nil
	}

	// Execute the stream process
	if err := stream.Orchestrate(ctx); err != nil {
		return fmt.Errorf("stream orchestration failed for group %d: %w", groupId, err)
	}

	// Send the final 'done' signal to mark completion
	glog.V(2).Infof("Sending completion signal for group %d", groupId)
	done := &apiv25.KVS{Done: true}

	if err := out.Send(&apiv25.StreamSnapshotRequest{Pairs: done}); err != nil && err != io.EOF {
		return fmt.Errorf("failed to send 'done' signal for group %d: %w", groupId, err)
	}
	// Wait for acknowledgment from the server
	ack, err := out.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("failed to receive ACK for group %d: %w", groupId, err)
	}
	glog.Infof("Group %d: Received ACK with message: %v", groupId, ack.Done)

	return nil
}
