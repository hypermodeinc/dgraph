/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dgraphimport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dgraph-io/badger/v4"
	apiv2 "github.com/dgraph-io/dgo/v250/protos/api.v2"
	"github.com/hypermodeinc/dgraph/v25/worker"

	"github.com/golang/glog"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

// newClient creates a new import client with the specified endpoint and gRPC options.
func newClient(endpoint string, opts grpc.DialOption) (apiv2.DgraphClient, error) {
	conn, err := grpc.NewClient(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to endpoint [%s]: %w", endpoint, err)
	}

	glog.Infof("Successfully connected to Dgraph endpoint: %s", endpoint)
	return apiv2.NewDgraphClient(conn), nil
}

func Import(ctx context.Context, endpoint string, opts grpc.DialOption, bulkOutDir string) error {
	dg, err := newClient(endpoint, opts)
	if err != nil {
		return err
	}
	resp, err := startPDirStream(ctx, dg)
	if err != nil {
		return err
	}

	return sendPDir(ctx, dg, bulkOutDir, resp.Groups)
}

// startPDirStream initiates a snapshot stream session with the Dgraph server.
func startPDirStream(ctx context.Context, dc apiv2.DgraphClient) (*apiv2.UpdateExtSnapshotStreamingStateResponse, error) {
	glog.Info("Initiating pdir stream")
	req := &apiv2.UpdateExtSnapshotStreamingStateRequest{
		Start: true,
	}
	resp, err := dc.UpdateExtSnapshotStreamingState(ctx, req)
	if err != nil {
		glog.Errorf("failed to initiate pdir stream: %v", err)
		return nil, fmt.Errorf("failed to initiate pdir stream: %v", err)
	}
	glog.Info("Pdir stream initiated successfully")
	return resp, nil
}

// sendPDir takes a p directory and a set of group IDs and streams the data from the
// p directory to the corresponding group IDs. It first scans the provided directory for
// subdirectories named with numeric group IDs.
func sendPDir(ctx context.Context, dc apiv2.DgraphClient, baseDir string, groups []uint32) error {
	glog.Infof("Starting to stream pdir from directory: %s", baseDir)

	errG, ctx := errgroup.WithContext(ctx)
	for _, group := range groups {
		group := group
		errG.Go(func() error {
			pDir := filepath.Join(baseDir, fmt.Sprintf("%d", group-1), "p")
			if _, err := os.Stat(pDir); err != nil {
				return fmt.Errorf("p directory does not exist for group [%d]: [%s]", group, pDir)
			}
			glog.Infof("Streaming data for group [%d] from directory: [%s]", group, pDir)
			if err := streamData(ctx, dc, pDir, group); err != nil {
				glog.Errorf("Failed to stream data for groups [%v] from directory: [%s]: %v", group, pDir, err)
				return err
			}

			return nil
		})
	}
	if err1 := errG.Wait(); err1 != nil {
		// If the p directory doesn't exist for this group, it indicates that
		// streaming might be in progress to other groups. We disable drain mode
		// to prevent interference and drop any streamed data to ensure a clean state.
		req := &apiv2.UpdateExtSnapshotStreamingStateRequest{
			Start:    false,
			Finish:   true,
			DropData: true,
		}
		if _, err := dc.UpdateExtSnapshotStreamingState(context.Background(), req); err != nil {
			return fmt.Errorf("failed to stream data :%v failed to off drain mode: %v", err1, err)
		}

		glog.Info("successfully disabled drain mode")
		return err1
	}

	glog.Info("Completed streaming all pdirs")
	req := &apiv2.UpdateExtSnapshotStreamingStateRequest{
		Start:    false,
		Finish:   true,
		DropData: false,
	}
	if _, err := dc.UpdateExtSnapshotStreamingState(context.Background(), req); err != nil {
		return fmt.Errorf("failed to disabled drain mode: %v", err)
	}
	glog.Infof("Completed streaming all pdirs")
	return nil
}

// streamData handles the actual data streaming process for a single group.
// It opens the BadgerDB at the specified directory and streams all data to the server.
func streamData(ctx context.Context, dc apiv2.DgraphClient, pdir string, groupId uint32) error {
	glog.Infof("Opening stream for group %d from directory %s", groupId, pdir)

	// Initialize stream with the server
	out, err := dc.StreamExtSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("failed to start pdir stream for group %d: %w", groupId, err)
	}

	// Open the BadgerDB instance at the specified directory
	opt := badger.DefaultOptions(pdir)
	ps, err := badger.OpenManaged(opt)
	if err != nil {
		return fmt.Errorf("failed to open BadgerDB at [%s]: %w", pdir, err)
	}

	defer func() {
		if err := ps.Close(); err != nil {
			glog.Warningf("Error closing BadgerDB: %v", err)
		}
	}()

	// Send group ID as the first message in the stream
	glog.Infof("Sending group ID [%d] to server", groupId)
	groupReq := &apiv2.StreamExtSnapshotRequest{GroupId: groupId}
	if err := out.Send(groupReq); err != nil {
		return fmt.Errorf("failed to send group ID [%d]: %w", groupId, err)
	}

	// Configure and start the BadgerDB stream
	glog.Infof("Starting BadgerDB stream for group [%d]", groupId)
	// if err := RunBadgerStream(ctx, ps, out, groupId); err != nil {
	// 	return fmt.Errorf("badger stream failed for group [%d]: %w", groupId, err)
	// }

	if err := worker.RunBadgerStream(ctx, ps, out, groupId); err != nil {
		return fmt.Errorf("badger stream failed for group [%d]: %w", groupId, err)
	}
	if _, err := out.CloseAndRecv(); err != nil {
		return fmt.Errorf("failed to receive ACK for group [%d]: %w", groupId, err)
	}
	glog.Infof("Group [%d]: Received ACK ", groupId)
	return nil
}
