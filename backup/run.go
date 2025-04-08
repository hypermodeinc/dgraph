/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/golang/glog"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	bpb "github.com/dgraph-io/badger/v4/pb"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v24/posting"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/worker"
	"github.com/hypermodeinc/dgraph/v24/x"
)

// Restore is the sub-command used to restore a backup.
var Restore x.SubCommand

// LsBackup is the sub-command used to list the backups in a folder.
var LsBackup x.SubCommand

var ExportBackup x.SubCommand

var opt struct {
	backupId    string
	badger      string
	location    string
	pdir        string
	zero        string
	key         x.Sensitive
	forceZero   bool
	destination string
	format      string
	verbose     bool
	upgrade     bool // used by export backup command.
}

func init() {
	initRestore()
	initBackupLs()
	initExportBackup()
}

func initRestore() {
	Restore.Cmd = &cobra.Command{
		Use:   "restore",
		Short: "Restore backup from Dgraph",
		Long: `
Restore loads objects created with the backup.
Backups are originated from HTTP at /admin/backup, then can be restored using CLI restore
command. Restore is intended to be used with new Dgraph clusters in offline state.
The --location flag indicates a source URI with Dgraph backup objects. This URI supports all
the schemes used for backup.

Source URI formats:
  [scheme]://[host]/[path]?[args]
  [scheme]:///[path]?[args]
  /[path]?[args] (only for local or NFS)

Source URI parts:
  scheme - service handler, one of: "s3", "minio", "file"
    host - remote address. ex: "dgraph.s3.amazonaws.com"
    path - directory, bucket or container at target. ex: "/dgraph/backups/"
    args - specific arguments that are ok to appear in logs.

The --posting flag sets the posting list parent dir to store the loaded backup files.
Using the --zero flag will use a Dgraph Zero address to update the start timestamp using
the restored version. Otherwise, the timestamp must be manually updated through Zero's HTTP
'assign' command.

Dgraph backup creates a unique backup object for each node group, and restore will create
a posting directory 'p' matching the backup group ID. Such that a backup file
named '.../r32-g2.backup' will be loaded to posting dir 'p2'.

Usage examples:
# Restore from local dir or NFS mount:
$ dgraph restore -p . -l /var/backups/dgraph

# Restore from S3:
$ dgraph restore -p /var/db/dgraph -l s3://s3.us-west-2.amazonaws.com/srfrog/dgraph

# Restore from dir and update Ts:
$ dgraph restore -p . -l /var/backups/dgraph -z localhost:5080
		`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			defer x.StartProfile(Restore.Conf).Stop()
			if err := runRestoreCmd(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		},
		Annotations: map[string]string{"group": "data-load"},
	}
	Restore.Cmd.SetHelpTemplate(x.NonRootTemplate)
	flag := Restore.Cmd.Flags()

	flag.StringVarP(&opt.badger, "badger", "b", worker.BadgerDefaults,
		z.NewSuperFlagHelp(worker.BadgerDefaults).
			Head("Badger options").
			Flag("compression",
				"Specifies the compression algorithm & compression level (if applicable) for the "+
					`postings directory. "none" would disable compression, while "zstd:1" would `+
					"set zstd compression at level 1.").
			Flag("goroutines", "The number of goroutines to use in badger.Stream.").
			String())

	flag.StringVarP(&opt.location, "location", "l", "",
		"Sets the source location URI (required).")
	flag.StringVarP(&opt.pdir, "postings", "p", "",
		"Directory where posting lists are stored (required).")
	flag.StringVarP(&opt.zero, "zero", "z", "", "gRPC address for Dgraph zero. ex: localhost:5080")
	flag.StringVarP(&opt.backupId, "backup_id", "", "", "The ID of the backup series to "+
		"restore. If empty, it will restore the latest series.")
	flag.BoolVarP(&opt.forceZero, "force_zero", "", true, "If false, no connection to "+
		"a zero in the cluster will be required. Keep in mind this requires you to manually "+
		"update the timestamp and max uid when you start the cluster. The correct values are "+
		"printed near the end of this command's output.")
	x.RegisterClientTLSFlags(flag)
	x.RegisterEncFlag(flag)
	_ = Restore.Cmd.MarkFlagRequired("postings")
	_ = Restore.Cmd.MarkFlagRequired("location")
}

func initBackupLs() {
	LsBackup.Cmd = &cobra.Command{
		Use:   "lsbackup",
		Short: "List info on backups in a given location",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			defer x.StartProfile(LsBackup.Conf).Stop()
			if err := runLsbackupCmd(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		},
		Annotations: map[string]string{"group": "tool"},
	}
	LsBackup.Cmd.SetHelpTemplate(x.NonRootTemplate)
	flag := LsBackup.Cmd.Flags()
	flag.StringVarP(&opt.location, "location", "l", "",
		"Sets the source location URI (required).")
	flag.BoolVar(&opt.verbose, "verbose", false,
		"Outputs additional info in backup list.")
	_ = LsBackup.Cmd.MarkFlagRequired("location")
}

func runRestoreCmd() error {
	keys, err := x.GetEncAclKeys(Restore.Conf)
	if err != nil {
		return err
	}
	opt.key = keys.EncKey
	fmt.Println("Restoring backups from:", opt.location)
	fmt.Println("Writing postings to:", opt.pdir)

	if opt.zero == "" && opt.forceZero {
		return errors.New("No Dgraph Zero address passed. Use the --force_zero option if you " +
			"meant to do this")
	}

	var zc pb.ZeroClient
	if opt.zero != "" {
		fmt.Println("Updating Zero timestamp at:", opt.zero)

		tlsConfig, err := x.LoadClientTLSConfigForInternalPort(Restore.Conf)
		x.Checkf(err, "Unable to generate helper TLS config")
		callOpts := []grpc.DialOption{}
		if tlsConfig != nil {
			callOpts = append(callOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
		} else {
			callOpts = append(callOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		}

		zero, err := grpc.NewClient(opt.zero, callOpts...)
		if err != nil {
			return fmt.Errorf("unable to connect to %s: %w", opt.zero, err)
		}
		zc = pb.NewZeroClient(zero)
	}

	badger := z.NewSuperFlag(opt.badger).MergeAndCheckDefault(worker.BadgerDefaults)
	ctype, clevel := x.ParseCompression(badger.GetString("compression"))

	start := time.Now()
	result := worker.RunOfflineRestore(opt.pdir, opt.location,
		opt.backupId, "", opt.key, ctype, clevel)
	if result.Err != nil {
		return result.Err
	}
	if result.Version == 0 {
		return errors.New("failed to obtain a restore version")
	}
	fmt.Printf("Restore version: %d\n", result.Version)
	fmt.Printf("Restore max uid: %d\n", result.MaxLeaseUid)

	if zc != nil {
		ctx, cancelTs := context.WithTimeout(context.Background(), time.Minute)
		defer cancelTs()

		if _, err := zc.Timestamps(ctx, &pb.Num{Val: result.Version}); err != nil {
			fmt.Printf("Failed to assign timestamp %d in Zero: %v", result.Version, err)
			return err
		}

		leaseID := func(val uint64, typ pb.NumLeaseType) error {
			// MaxLeaseUid can be zero if the backup was taken on an empty DB.
			if val == 0 {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if _, err = zc.AssignIds(ctx, &pb.Num{Val: val, Type: typ}); err != nil {
				fmt.Printf("Failed to assign %s %d in Zero: %v\n",
					pb.NumLeaseType_name[int32(typ)], val, err)
				return err
			}
			return nil
		}

		if err := leaseID(result.MaxLeaseUid, pb.Num_UID); err != nil {
			return fmt.Errorf("cannot update max uid lease after restore: %w", err)
		}
		if err := leaseID(result.MaxLeaseNsId, pb.Num_NS_ID); err != nil {
			return fmt.Errorf("cannot update max namespace lease after restore: %w", err)
		}
	}

	fmt.Printf("Restore: Time elapsed: %s\n", time.Since(start).Round(time.Second))
	return nil
}

func runLsbackupCmd() error {
	manifests, err := worker.ListBackupManifests(opt.location, nil)
	if err != nil {
		return fmt.Errorf("while listing manifests: %w", err)
	}

	type backupEntry struct {
		Path           string              `json:"path"`
		Since          uint64              `json:"since"`
		ReadTs         uint64              `json:"read_ts"`
		BackupId       string              `json:"backup_id"`
		BackupNum      uint64              `json:"backup_num"`
		Encrypted      bool                `json:"encrypted"`
		Type           string              `json:"type"`
		Groups         map[uint32][]string `json:"groups,omitempty"`
		DropOperations []*pb.DropOperation `json:"drop_operations,omitempty"`
	}

	type backupOutput []backupEntry

	var output backupOutput
	for _, manifest := range manifests {

		be := backupEntry{
			Path:      manifest.Path,
			Since:     manifest.SinceTsDeprecated,
			ReadTs:    manifest.ReadTs,
			BackupId:  manifest.BackupId,
			BackupNum: manifest.BackupNum,
			Encrypted: manifest.Encrypted,
			Type:      manifest.Type,
		}
		if opt.verbose {
			be.Groups = manifest.Groups
			be.DropOperations = manifest.DropOperations
		}
		output = append(output, be)
	}
	b, err := json.MarshalIndent(output, "", "\t")
	if err != nil {
		fmt.Println("error:", err)
	}
	_, _ = os.Stdout.Write(b)
	fmt.Println()
	return nil
}

func initExportBackup() {
	ExportBackup.Cmd = &cobra.Command{
		Use:   "export_backup",
		Short: "Export data inside single full or incremental backup",
		Long:  ``,
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			defer x.StartProfile(ExportBackup.Conf).Stop()
			if err := runExportBackup(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		},
		Annotations: map[string]string{"group": "tool"},
	}

	ExportBackup.Cmd.SetHelpTemplate(x.NonRootTemplate)
	flag := ExportBackup.Cmd.Flags()
	flag.StringVarP(&opt.location, "location", "l", "",
		`Sets the location of the backup. Both file URIs and s3 are supported.
		This command will take care of all the full + incremental backups present in the location.`)
	flag.StringVarP(&opt.destination, "destination", "d", "",
		"The folder to which export the backups.")
	flag.StringVarP(&opt.format, "format", "f", "rdf",
		"The format of the export output. Accepts a value of either rdf or json")
	flag.BoolVar(&opt.upgrade, "upgrade", false,
		`If true, retrieve the CORS from DB and append at the end of GraphQL schema.
		It also deletes the deprecated types and predicates.
		Use this option when exporting a backup of 20.11 for loading onto 21.03.`)
	x.RegisterEncFlag(flag)
}

type bufWriter struct {
	writers *worker.Writers
	req     *pb.ExportRequest
}

func exportSchema(writers *worker.Writers, val []byte, pk x.ParsedKey) error {
	var kv *bpb.KV
	var err error
	if pk.IsSchema() {
		kv, err = worker.SchemaExportKv(pk.Attr, val, true)
		if err != nil {
			return err
		}
	} else {
		kv, err = worker.TypeExportKv(pk.Attr, val)
		if err != nil {
			return err
		}
	}
	return worker.WriteExport(writers, kv, "rdf")
}

func (bw *bufWriter) Write(buf *z.Buffer) error {
	kv := &bpb.KV{}
	err := buf.SliceIterate(func(s []byte) error {
		kv.Reset()
		if err := proto.Unmarshal(s, kv); err != nil {
			return fmt.Errorf("processKvBuf failed to unmarshal kv: %w", err)
		}
		pk, err := x.Parse(kv.Key)
		if err != nil {
			return fmt.Errorf("processKvBuf failed to parse key: %w", err)
		}
		if pk.Attr == "_predicate_" {
			return nil
		}
		if pk.IsSchema() || pk.IsType() {
			return exportSchema(bw.writers, kv.Value, pk)
		}
		if pk.IsData() {
			pl := &pb.PostingList{}
			if err := proto.Unmarshal(kv.Value, pl); err != nil {
				return fmt.Errorf("processKvBuf failed to Unmarshal pl: %w", err)
			}
			l := posting.NewList(kv.Key, pl, kv.Version)
			kvList, err := worker.ToExportKvList(pk, l, bw.req)
			if err != nil {
				return fmt.Errorf("processKvBuf failed to export: %w", err)
			}
			if len(kvList.Kv) == 0 {
				return nil
			}
			exportKv := kvList.Kv[0]
			return worker.WriteExport(bw.writers, exportKv, bw.req.Format)
		}
		return nil
	})
	return fmt.Errorf("bufWriter failed to write: %w", err)
}

func runExportBackup() error {
	keys, err := x.GetEncAclKeys(ExportBackup.Conf)
	if err != nil {
		return err
	}
	opt.key = keys.EncKey
	if opt.format != "json" && opt.format != "rdf" {
		return fmt.Errorf("invalid format %s", opt.format)
	}
	// Create exportDir and temporary folder to store the restored backup.
	exportDir, err := filepath.Abs(opt.destination)
	if err != nil {
		return fmt.Errorf("cannot convert path %s to absolute path: %w", exportDir, err)
	}
	if err := os.MkdirAll(exportDir, 0755); err != nil {
		return fmt.Errorf("cannot create dir %s: %w", exportDir, err)
	}

	uri, err := url.Parse(opt.location)
	if err != nil {
		return fmt.Errorf("runExportBackup: %w", err)
	}
	handler, err := worker.NewUriHandler(uri, nil)
	if err != nil {
		return fmt.Errorf("runExportBackup: %w", err)
	}
	latestManifest, err := worker.GetLatestManifest(handler, uri)
	if err != nil {
		return fmt.Errorf("runExportBackup: %w", err)
	}

	mapDir, err := os.MkdirTemp(x.WorkerConfig.TmpDir, "restore-export")
	x.Check(err)
	defer func() {
		if err := os.RemoveAll(mapDir); err != nil {
			glog.Warningf("Error removing temp restore-export dir: %v", err)
		}
	}()
	glog.Infof("Created temporary map directory: %s\n", mapDir)

	// TODO: Can probably make this procesing concurrent.
	for gid := range latestManifest.Groups {
		glog.Infof("Exporting group: %d", gid)
		req := &pb.RestoreRequest{
			GroupId:           gid,
			Location:          opt.location,
			EncryptionKeyFile: ExportBackup.Conf.GetString("encryption_key_file"),
			RestoreTs:         1,
		}
		if _, err := worker.RunMapper(req, mapDir); err != nil {
			return fmt.Errorf("failed to map the backups: %w", err)
		}
		in := &pb.ExportRequest{
			GroupId:     gid,
			ReadTs:      latestManifest.ValidReadTs(),
			UnixTs:      time.Now().Unix(),
			Format:      opt.format,
			Destination: exportDir,
		}
		uts := time.Unix(in.UnixTs, 0)
		destPath := fmt.Sprintf("dgraph.r%d.u%s", in.ReadTs, uts.UTC().Format("0102.1504"))
		exportStorage, err := worker.NewExportStorage(in, destPath)
		if err != nil {
			return err
		}

		writers, err := worker.InitWriters(exportStorage, in)
		if err != nil {
			return err
		}

		w := &bufWriter{req: in, writers: writers}
		if err := worker.RunReducer(w, mapDir); err != nil {
			return fmt.Errorf("failed to reduce the map: %w", err)
		}
		if files, err := exportStorage.FinishWriting(writers); err != nil {
			return fmt.Errorf("failed to finish write: %w", err)
		} else {
			glog.Infof("done exporting files: %v\n", files)
		}
	}
	return nil
}
