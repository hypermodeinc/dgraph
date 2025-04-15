/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dgraphimport

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

// Client represents a Dgraph import client that handles snapshot streaming.
type Client struct {
	opts grpc.DialOption
	dg   apiv25.DgraphClient
}

// NewClient creates a new import client with the specified endpoint and gRPC options.
func NewClient(endpoint string, opts grpc.DialOption) (*Client, error) {
	conn, err := grpc.NewClient(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to endpoint [%s]: %w", endpoint, err)
	}

	glog.Infof("Successfully connected to Dgraph endpoint: %s", endpoint)
	return &Client{dg: apiv25.NewDgraphClient(conn), opts: opts}, nil
}

// StartSnapshotStream initiates a snapshot stream session with the Dgraph server.
func (c *Client) StartSnapshotStream(ctx context.Context) (*apiv25.InitiateSnapshotStreamResponse, error) {
	glog.V(2).Infof("Initiating snapshot stream")
	req := &apiv25.InitiateSnapshotStreamRequest{}
	resp, err := c.dg.InitiateSnapshotStream(ctx, req)
	if err != nil {
		glog.Errorf("Failed to initiate snapshot stream: %v", err)
		return nil, err
	}
	glog.Infof("Snapshot stream initiated successfully")
	return resp, nil
}

// SendSnapshot takes a p directory and a set of group IDs and streams the data from the
// p directory to the corresponding group IDs. The function will skip any groups that do not
// have a corresponding p directory.
func (c *Client) SendSnapshot(ctx context.Context, pDir string, groups []uint32) error {
	glog.Infof("Starting to stream snapshots from directory: %s", pDir)

	// Get mapping of group IDs to their respective p directories
	groupDirs, err := scanPDirs(pDir)
	if err != nil {
		glog.Errorf("Error getting p directories: %v", err)
		return fmt.Errorf("error getting p directories: %w", err)
	}

	// Process each group in the provided list
	for _, group := range groups {
		pDir, exists := groupDirs[group-1]
		if !exists {
			glog.Warningf("No p directory found for group %d, skipping...", group)
			continue
		}

		if _, err := os.Stat(pDir); os.IsNotExist(err) {
			glog.Warningf("P directory does not exist: %s, skipping...", pDir)
			continue
		}

		glog.Infof("Streaming data for group %d from directory: %s", group, pDir)
		err = streamData(ctx, c.dg, pDir, group)
		if err != nil {
			glog.Errorf("Failed to stream snapshot for group %d: %v", group, err)
			return err
		}
		glog.Infof("Successfully streamed snapshot for group %d", group)
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
	groupReq := &apiv25.StreamSnapshotRequest{
		GroupId: groupId,
	}
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
		if err := out.Send(&apiv25.StreamSnapshotRequest{
			Kvs: kvs}); err != nil {
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
	done := &apiv25.KVS{
		Done: true,
	}

	if err := out.Send(&apiv25.StreamSnapshotRequest{
		Kvs: done}); err != nil {
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

// scanPDirs scans the base path and returns a mapping of group IDs to their
// corresponding p directory paths. It looks for numbered subdirectories which contain a "p" folder.
func scanPDirs(basePath string) (map[uint32]string, error) {
	glog.V(2).Infof("Scanning for p directories in %s", basePath)
	groupDirs := make(map[uint32]string)

	entries, err := os.ReadDir(basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %w", basePath, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			groupID, err := strconv.Atoi(entry.Name())
			if err == nil {
				pDir := filepath.Join(basePath, entry.Name(), "p")
				if _, err := os.Stat(pDir); err == nil {
					groupDirs[uint32(groupID)] = pDir
					glog.V(2).Infof("Found p directory for group %d: %s", groupID, pDir)
				}
			}
		}
	}

	glog.Infof("Found %d group directories", len(groupDirs))
	return groupDirs, nil
}
