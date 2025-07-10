/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dgraphimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/dgo/v250"
	apiv2 "github.com/dgraph-io/dgo/v250/protos/api.v2"
	"github.com/dgraph-io/ristretto/v2/z"

	"github.com/golang/glog"
	"golang.org/x/sync/errgroup"
)

// newClient creates a new import client with the specified endpoint and gRPC options.
func newClient(connectionString string) (apiv2.DgraphClient, error) {
	dg, err := dgo.Open(connectionString)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to endpoint [%s]: %w", connectionString, err)
	}

	glog.Infof("[import] Successfully connected to Dgraph endpoint: %s", connectionString)
	return dg.GetAPIv2Client()[0], nil
}

func Import(ctx context.Context, connectionString string, bulkOutDir string) error {
	dg, err := newClient(connectionString)
	if err != nil {
		return err
	}
	resp, err := initiateSnapshotStream(ctx, dg)
	if err != nil {
		return err
	}

	return streamSnapshot(ctx, dg, bulkOutDir, resp.Groups)
}

// initiateSnapshotStream initiates a snapshot stream session with the Dgraph server.
func initiateSnapshotStream(ctx context.Context, dc apiv2.DgraphClient) (*apiv2.UpdateExtSnapshotStreamingStateResponse, error) {
	glog.Info("[import] Initiating external snapshot stream")
	req := &apiv2.UpdateExtSnapshotStreamingStateRequest{
		Start: true,
	}
	resp, err := dc.UpdateExtSnapshotStreamingState(ctx, req)
	if err != nil {
		glog.Errorf("[import] failed to initiate external snapshot stream: %v", err)
		return nil, fmt.Errorf("failed to initiate external snapshot stream: %v", err)
	}
	glog.Info("[import] External snapshot stream initiated successfully")
	return resp, nil
}

// streamSnapshot takes a p directory and a set of group IDs and streams the data from the
// p directory to the corresponding group IDs. It first scans the provided directory for
// subdirectories named with numeric group IDs.
func streamSnapshot(ctx context.Context, dc apiv2.DgraphClient, baseDir string, groups []uint32) error {
	glog.Infof("[import] Starting to stream snapshot from directory: %s", baseDir)

	errG, errGrpCtx := errgroup.WithContext(ctx)
	for _, group := range groups {
		errG.Go(func() error {
			pDir := filepath.Join(baseDir, fmt.Sprintf("%d", group-1), "p")
			if _, err := os.Stat(pDir); err != nil {
				return fmt.Errorf("p directory does not exist for group [%d]: [%s]", group, pDir)
			}
			glog.Infof("[import] Streaming data for group [%d] from directory: [%s]", group, pDir)
			if err := streamSnapshotForGroup(errGrpCtx, dc, pDir, group); err != nil {
				glog.Errorf("[import] Failed to stream data for group [%v] from directory: [%s]: %v", group, pDir, err)
				return err
			}

			return nil
		})
	}

	if err := errG.Wait(); err != nil {
		glog.Errorf("[import] failed to stream external snapshot: %v", err)
		// If errors occurs during streaming of the external snapshot, we drop all the data and
		// go back to ensure a clean slate and the cluster remains in working state.
		glog.Info("[import] dropping all the data and going back to clean slate")
		req := &apiv2.UpdateExtSnapshotStreamingStateRequest{
			Start:    false,
			Finish:   true,
			DropData: true,
		}
		if _, err := dc.UpdateExtSnapshotStreamingState(ctx, req); err != nil {
			return fmt.Errorf("failed to turn off drain mode: %v", err)
		}

		glog.Info("[import] successfully disabled drain mode")
		return err
	}

	glog.Info("[import] Completed streaming external snapshot")
	req := &apiv2.UpdateExtSnapshotStreamingStateRequest{
		Start:    false,
		Finish:   true,
		DropData: false,
	}
	if _, err := dc.UpdateExtSnapshotStreamingState(ctx, req); err != nil {
		glog.Errorf("[import] failed to disable drain mode: %v", err)
		return fmt.Errorf("failed to disable drain mode: %v", err)
	}
	glog.Info("[import] successfully disable drain mode")
	return nil
}

// streamSnapshotForGroup handles the actual data streaming process for a single group.
// It opens the BadgerDB at the specified directory and streams all data to the server.
func streamSnapshotForGroup(ctx context.Context, dc apiv2.DgraphClient, pdir string, groupId uint32) error {
	glog.Infof("Opening stream for group %d from directory %s", groupId, pdir)

	// Initialize stream with the server
	out, err := dc.StreamExtSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("failed to start external snapshot stream for group %d: %w", groupId, err)
	}
	defer func() {
		_ = out.CloseSend()
	}()

	// Open the BadgerDB instance at the specified directory
	opt := badger.DefaultOptions(pdir)
	opt.ReadOnly = true
	ps, err := badger.OpenManaged(opt)
	if err != nil {
		glog.Errorf("failed to open BadgerDB at [%s]: %v", pdir, err)
		return fmt.Errorf("failed to open BadgerDB at [%v]: %v", pdir, err)
	}
	defer func() {
		if err := ps.Close(); err != nil {
			glog.Warningf("[import] Error closing BadgerDB: %v", err)
		}
	}()

	// Send group ID as the first message in the stream
	glog.Infof("[import] Sending request for streaming external snapshot for group ID [%v]", groupId)
	groupReq := &apiv2.StreamExtSnapshotRequest{GroupId: groupId}
	if err := out.Send(groupReq); err != nil {
		return fmt.Errorf("failed to send request for group ID [%v] to the server: %w", groupId, err)
	}
	if _, err := out.Recv(); err != nil {
		return fmt.Errorf("failed to receive response for group ID [%v] from the server: %w", groupId, err)
	}
	glog.Infof("[import] Group [%v]: Received ACK for sending group request", groupId)

	// Configure and start the BadgerDB stream
	glog.Infof("[import] Starting BadgerDB stream for group [%v]", groupId)
	if err := streamBadger(ctx, ps, out, groupId); err != nil {
		return fmt.Errorf("badger streaming failed for group [%v]: %v", groupId, err)
	}
	return nil
}

// streamBadger runs a BadgerDB stream to send key-value pairs to the specified group.
// It creates a new stream at the maximum sequence number and sends the data to the specified group.
// It also sends a final 'done' signal to mark completion.
func streamBadger(ctx context.Context, ps *badger.DB, out apiv2.Dgraph_StreamExtSnapshotClient, groupId uint32) error {
	stream := ps.NewStreamAt(math.MaxUint64)
	stream.LogPrefix = "[import] Sending external snapshot to group [" + fmt.Sprintf("%d", groupId) + "]"
	stream.KeyToList = nil
	stream.Send = func(buf *z.Buffer) error {
		p := &apiv2.StreamPacket{Data: buf.Bytes()}
		if err := out.Send(&apiv2.StreamExtSnapshotRequest{Pkt: p}); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("failed to send data chunk: %w", err)
		}
		if _, err := out.Recv(); err != nil {
			return fmt.Errorf("failed to receive response for group ID [%v] from the server: %w", groupId, err)
		}
		glog.Infof("[import] Group [%v]: Received ACK for sending data chunk", groupId)

		return nil
	}

	// Execute the stream process
	if err := stream.Orchestrate(ctx); err != nil {
		return fmt.Errorf("stream orchestration failed for group [%v]: %w, badger path: %s", groupId, err, ps.Opts().Dir)
	}

	// Send the final 'done' signal to mark completion
	glog.Infof("[import] Sending completion signal for group [%d]", groupId)
	done := &apiv2.StreamPacket{Done: true}

	if err := out.Send(&apiv2.StreamExtSnapshotRequest{Pkt: done}); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("failed to send 'done' signal for group [%d]: %w", groupId, err)
	}

	if _, err := out.Recv(); err != nil {
		return fmt.Errorf("failed to receive response for group ID [%v] from the server: %w", groupId, err)
	}
	glog.Infof("[import] Group [%v]: Received ACK for sending completion signal", groupId)

	return nil
}
