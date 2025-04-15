/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dgraphtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/dgraphapi"
	"github.com/hypermodeinc/dgraph/v25/enc"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var datafiles = map[string]string{
	"1million.schema":  "https://github.com/hypermodeinc/dgraph-benchmarks/blob/main/data/1million.schema?raw=true",
	"1million.rdf.gz":  "https://github.com/hypermodeinc/dgraph-benchmarks/blob/main/data/1million.rdf.gz?raw=true",
	"21million.schema": "https://github.com/hypermodeinc/dgraph-benchmarks/blob/main/data/21million.schema?raw=true",
	"21million.rdf.gz": "https://github.com/hypermodeinc/dgraph-benchmarks/blob/main/data/21million.rdf.gz?raw=true",
}

type DatasetType int
type Dataset struct {
	name string
}

const (
	groupOneRdfFile   = "g01.rdf"
	groupOneRdfGzFile = "g01.rdf.gz"

	OneMillionDataset DatasetType = iota
	TwentyOneMillionDataset
)

// LiveOpts are options that are used for running live loader.
type LiveOpts struct {
	DataFiles      []string
	SchemaFiles    []string
	GqlSchemaFiles []string
}

// readGzFile reads the given file from disk completely and returns the content.
func readGzFile(sf string, encryption bool, encKeyPath string) ([]byte, error) {
	fd, err := os.Open(sf)
	if err != nil {
		return nil, errors.Wrapf(err, "error opening file [%v]", sf)
	}
	defer func() {
		if err := fd.Close(); err != nil {
			log.Printf("[WARNING] error closing file [%v]: %v", sf, err)
		}
	}()

	data, err := readGzData(fd, encryption, encKeyPath)
	if err != nil {
		return nil, errors.Wrapf(err, "error reading data from file [%v]", sf)
	}
	return data, nil
}

func readGzData(r io.Reader, encryption bool, encKeyPath string) ([]byte, error) {
	if encryption {
		encKey, err := os.ReadFile(encKeyPath)
		if err != nil {
			return nil, errors.Wrap(err, "error reading the encryption key from disk")
		}
		r, err = enc.GetReader(encKey, r)
		if err != nil {
			return nil, errors.Wrap(err, "error creating encrypted reader")
		}
	}

	gr, err := gzip.NewReader(r)
	if err != nil {
		return nil, errors.Wrapf(err, "error creating gzip reader")
	}
	defer func() {
		if err := gr.Close(); err != nil {
			log.Printf("[WARNING] error closing gzip reader: %v", err)
		}
	}()

	data, err := io.ReadAll(gr)
	if err != nil {
		return nil, errors.Wrap(err, "error reading data from io.Reader")
	}
	return data, nil
}

func writeGzData(data []byte, encryption bool, encKeyPath string) (io.Reader, error) {
	buf := bytes.NewBuffer(nil)

	var w io.Writer = buf
	if encryption {
		encKey, err := os.ReadFile(encKeyPath)
		if err != nil {
			return nil, errors.Wrap(err, "error reading the encryption key from disk")
		}
		w, err = enc.GetWriter(encKey, w)
		if err != nil {
			return nil, errors.Wrap(err, "error getting encrypted writer")
		}
	}

	zw := gzip.NewWriter(w)
	defer func() {
		if err := zw.Close(); err != nil {
			log.Printf("[WARNING] error closing zip writer: %v", err)
		}
	}()

	if _, err := zw.Write(data); err != nil {
		return nil, errors.Wrap(err, "error writing modified rdf file to zip")
	}
	return buf, nil
}

// setDQLSchema updates the DQL schema in the dgraph cluster to the
// schema present in all the files in the export as passed in args.
func setDQLSchema(c *LocalCluster, files []string) error {
	gc, cleanup, err := c.Client()
	if err != nil {
		return errors.Wrap(err, "error creating grpc client")
	}
	defer cleanup()

	if c.conf.acl {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		err := gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace)
		if err != nil {
			return errors.Wrap(err, "error login to default namespace")
		}
	}

	for _, sf := range files {
		data, err := readGzFile(sf, c.conf.encryption, c.encKeyPath)
		if err != nil {
			return err
		}
		if err := gc.SetupSchema(string(data)); err != nil {
			return errors.Wrapf(err, "error setting up DQL schema [%v]", string(data))
		}
	}
	return nil
}

// setGraphQLSchema updates the graphql schema in the dgraph cluster with
// the schema present in all the files in the export as passed in args.
func setGraphQLSchema(c *LocalCluster, files []string) error {
	hc, err := c.HTTPClient()
	if err != nil {
		return errors.Wrap(err, "error creating HTTP client")
	}

	for _, sf := range files {
		data, err := readGzFile(sf, c.conf.encryption, c.encKeyPath)
		if err != nil {
			return err
		}
		// if there is no GraphQL schema in the cluster, the GQL
		// file only has empty []. we can skip these files.
		if len(data) < 10 {
			continue
		}

		var nsToSch []struct {
			Namespace uint64 `json:"namespace"`
			Schema    string `json:"schema"`
		}
		if err := json.Unmarshal(data, &nsToSch); err != nil {
			return errors.Wrapf(err, "error parsing gql schema file content [%v]", string(data))
		}
		for _, nss := range nsToSch {
			if nss.Schema == "" {
				continue
			}

			if c.conf.acl {
				err := hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, nss.Namespace)
				if err != nil {
					return errors.Wrap(err, "error login into default namespace")
				}
			}
			if err := hc.UpdateGQLSchema(nss.Schema); err != nil {
				return errors.Wrapf(err, "error updating GQL schema to: [%v]", nss.Schema)
			}
		}
	}
	return nil
}

// LiveLoad runs the live loader with provided options
func (c *LocalCluster) LiveLoad(opts LiveOpts) error {
	log.Printf("[INFO] updating DQL schema from [%v]", strings.Join(opts.SchemaFiles, " "))
	if err := setDQLSchema(c, opts.SchemaFiles); err != nil {
		return err
	}
	log.Printf("[INFO] updating GraphQL schema from [%v]", strings.Join(opts.GqlSchemaFiles, " "))
	if err := setGraphQLSchema(c, opts.GqlSchemaFiles); err != nil {
		return err
	}

	var alphaURLs []string
	for i, aa := range c.alphas {
		url, err := aa.alphaURL(c)
		if err != nil {
			return errors.Wrapf(err, "error finding URL for alpha #%v", i)
		}
		alphaURLs = append(alphaURLs, url)
	}
	zeroURL, err := c.zeros[0].zeroURL(c)
	if err != nil {
		return errors.Wrap(err, "error finding URL of first zero")
	}

	args := []string{
		"live",
		"--files", strings.Join(opts.DataFiles, ","),
		"--alpha", strings.Join(alphaURLs, ","),
		"--zero", zeroURL,
	}
	if c.conf.acl {
		args = append(args, fmt.Sprintf("--creds=user=%s;password=%s;namespace=%d",
			dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))
	}
	if c.conf.encryption {
		args = append(args, fmt.Sprintf("--encryption=key-file=%v", c.encKeyPath))
	}

	log.Printf("[INFO] running live loader with args: [%v]", strings.Join(args, " "))
	cmd := exec.Command(filepath.Join(c.tempBinDir, "dgraph"), args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrapf(err, "error running live loader: %v", string(out))
	} else {
		log.Printf("[INFO] ==== output for live loader ====")
		log.Println(string(out))
	}
	return nil
}

// findGrootAndGuardians returns the UIDs of groot user and guardians group
func findGrootAndGuardians(c *LocalCluster) (string, string, error) {
	gc, cleanup, err := c.Client()
	if err != nil {
		return "", "", errors.Wrapf(err, "error creating grpc client")
	}
	defer cleanup()

	if c.conf.acl {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		err = gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace)
		if err != nil {
			return "", "", errors.Wrapf(err, "error logging in as groot")
		}
	}

	query := `{
		q(func: eq(dgraph.xid, "groot")){
			uid
			dgraph.user.group @filter(eq(dgraph.xid, "guardians")) {
				uid
			}
		}
	}`
	resp, err := gc.Query(query)
	if err != nil {
		return "", "", errors.Wrapf(err, "error querying groot & guardians UIDs")
	}
	var result struct {
		Q []struct {
			Uid    string
			Groups []struct {
				Uid string
			} `json:"dgraph.user.group"`
		}
	}
	if err := json.Unmarshal(resp.Json, &result); err != nil {
		return "", "", errors.Wrapf(err, "error unmarshalling resp: [%v]", string(resp.Json))
	}
	if len(result.Q) != 1 || result.Q[0].Uid == "" ||
		len(result.Q[0].Groups) != 1 || result.Q[0].Groups[0].Uid == "" {
		return "", "", errors.Errorf("unable to find groot & guardians, resp: [%+v]", resp)
	}
	return result.Q[0].Uid, result.Q[0].Groups[0].Uid, nil
}

// modifyACLEntries replaces groot's and guardians' UIDs
// in the exported files to the UIDs in this dgraph cluster.
func modifyACLEntries(c *LocalCluster, r io.Reader) (io.Reader, error) {
	grootUIDNew, guardiansUIDNew, err := findGrootAndGuardians(c)
	if err != nil {
		return nil, err
	}
	grootUIDNew = fmt.Sprintf("<%v>", grootUIDNew)
	guardiansUIDNew = fmt.Sprintf("<%v>", guardiansUIDNew)

	data, err := readGzData(r, c.conf.encryption, c.encKeyPath)
	if err != nil {
		return nil, err
	}

	// We need to find UIDs of the guardians group and groot node and replace them with
	// the right UID that the new cluster has. Because,all of the reserved predicates are
	// assigned to group 1 and we only have one rdf.gz file per group in the export, we
	// can just do it one io.Reader (flie) at a time as we get in this function. The way
	// we find the UIDs is through searching for following byte sequences.
	//   <dgraph.xid> "guardians"
	//   <dgraph.xid> "groot"
	findUID := func(sub []byte) ([]byte, error) {
		i := bytes.Index(data, sub)
		if i == -1 {
			return nil, errors.Errorf("unable to find data in RDF file: [%v]", string(sub))
		}
		var start, end int
		for i = i - 1; i >= 0; i-- {
			if data[i] == byte('>') {
				end = i
			} else if data[i] == byte('<') {
				start = i
				break
			}
		}
		return data[start : end+1], nil
	}
	guardiansUID, err := findUID([]byte(`<dgraph.xid> "guardians"`))
	if err != nil {
		return nil, err
	}
	grootUID, err := findUID([]byte(`<dgraph.xid> "groot"`))
	if err != nil {
		return nil, err
	}

	// we should only replace if RDF line is for a reserved type
	lines := bytes.Split(data, []byte{'\n'})
	for i, line := range lines {
		if bytes.Contains(line, []byte("<dgraph.")) {
			line = bytes.ReplaceAll(line, guardiansUID, []byte(guardiansUIDNew))
			line = bytes.ReplaceAll(line, grootUID, []byte(grootUIDNew))
			lines[i] = line
		}
	}
	data = bytes.Join(lines, []byte{'\n'})

	return writeGzData(data, c.conf.encryption, c.encKeyPath)
}

// LiveLoadFromExport runs the live loader from the output of dgraph export
// The exportDir is the directory present inside the container. This function
// first copies all the files on the host and then runs the live loader.
func (c *LocalCluster) LiveLoadFromExport(exportDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	exportDirHost, err := os.MkdirTemp("", "dgraph-export")
	if err != nil {
		return errors.Wrap(err, "error creating temp dir for exported data")
	}
	defer func() {
		if err := os.RemoveAll(exportDirHost); err != nil {
			log.Printf("[WARNING] error removing export copy on the host: %v", err)
		}
	}()

	// we need to copy the exported data from the container to host
	ts, _, err := c.dcli.CopyFromContainer(ctx, c.alphas[0].cid(), exportDir)
	if err != nil {
		return errors.Wrapf(err, "error copying export dir from container [%v]", c.alphas[0].cname())
	}
	defer func() {
		if err := ts.Close(); err != nil {
			log.Printf("[WARNING] error closing tared stream from docker cp for [%v]", c.alphas[0].cname())
		}
	}()

	// .rdf.gz, .schema.gz,.gql_schema.gz
	var rdfFiles, schemaFiles, gqlSchemaFiles, jsonFiles []string
	tr := tar.NewReader(ts)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return errors.Wrapf(err, "error reading file in tared stream: [%+v]", header)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}

		fileName := filepath.Base(header.Name)
		hostFile := filepath.Join(exportDirHost, fileName)
		switch {
		case strings.HasSuffix(fileName, ".rdf.gz"):
			rdfFiles = append(rdfFiles, hostFile)
		case strings.HasSuffix(fileName, ".json.gz"):
			jsonFiles = append(jsonFiles, hostFile)
		case strings.HasSuffix(fileName, ".schema.gz"):
			schemaFiles = append(schemaFiles, hostFile)
		case strings.HasSuffix(fileName, ".gql_schema.gz"):
			gqlSchemaFiles = append(gqlSchemaFiles, hostFile)
		default:
			return errors.Errorf("found unexpected file in export: %v", fileName)
		}

		fd, err := os.Create(hostFile)
		if err != nil {
			return errors.Wrapf(err, "error creating file [%v]", hostFile)
		}
		defer func() {
			if err := fd.Close(); err != nil {
				log.Printf("[WARNING] error closing file while docker cp: [%+v]", header)
			}
		}()

		// Because we export UIDs in the export, and groot and guardians nodes already exist
		// in the graph in any Dgraph cluster, we need to fix the exported data to use the UIDs
		// of the new cluster for both groot and guardians nodes. These UIDs will only be used
		// in the export file of group 1 because all reserved predicates are always in group 1.
		var fromReader io.Reader = tr
		if fileName == groupOneRdfGzFile {
			r, err := modifyACLEntries(c, tr)
			if err != nil {
				return err
			}
			fromReader = r
		}

		if _, err := io.Copy(fd, fromReader); err != nil {
			return errors.Wrapf(err, "error writing to [%v] from: [%+v]", fd.Name(), header)
		}
	}

	opts := LiveOpts{
		SchemaFiles:    schemaFiles,
		GqlSchemaFiles: gqlSchemaFiles,
	}
	if len(rdfFiles) == 0 {
		opts.DataFiles = jsonFiles
	} else {
		opts.DataFiles = rdfFiles
	}
	if err := c.LiveLoad(opts); err != nil {
		return errors.Wrapf(err, "error running live loader: %v", err)
	}
	return nil
}

type BulkOpts struct {
	DataFiles      []string
	SchemaFiles    []string
	GQLSchemaFiles []string
	OutDir         string
}

func (c *LocalCluster) BulkLoad(opts BulkOpts) error {
	zeroURL, err := c.zeros[0].zeroURL(c)
	if err != nil {
		return errors.Wrap(err, "error finding URL of first zero")
	}

	var outDir string
	if opts.OutDir != "" {
		outDir = opts.OutDir
	} else {
		outDir = c.conf.bulkOutDirForMount
	}

	shards := c.conf.numAlphas / c.conf.replicas
	args := []string{"bulk",
		"--store_xids=true",
		"--zero", zeroURL,
		"--reduce_shards", strconv.Itoa(shards),
		"--map_shards", strconv.Itoa(shards),
		"--out", outDir,
		// we had to create the dir for setting up docker, hence, replacing it here.
		"--replace_out",
	}

	if len(opts.DataFiles) > 0 {
		args = append(args, "-f", strings.Join(opts.DataFiles, ","))
	}
	if len(opts.SchemaFiles) > 0 {
		args = append(args, "-s", strings.Join(opts.SchemaFiles, ","))
	}
	if len(opts.GQLSchemaFiles) > 0 {
		args = append(args, "-g", strings.Join(opts.GQLSchemaFiles, ","))
	}

	log.Printf("[INFO] running bulk loader with args: [%v]", strings.Join(args, " "))
	cmd := exec.Command(filepath.Join(c.tempBinDir, "dgraph"), args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrapf(err, "error running bulk loader: %v", string(out))
	} else {
		log.Printf("[INFO] ==== output for bulk loader ====")
		log.Println(string(out))
		return nil
	}
}

// AddData will insert a total of end-start triples into the database.
func AddData(gc *dgraphapi.GrpcClient, pred string, start, end int) error {
	if err := gc.SetupSchema(fmt.Sprintf(`%v: string @index(exact) .`, pred)); err != nil {
		return err
	}

	rdf := ""
	for i := start; i <= end; i++ {
		rdf = rdf + fmt.Sprintf("_:a%v <%v> \"%v%v\" .	\n", i, pred, pred, i)
	}
	_, err := gc.Mutate(&api.Mutation{SetNquads: []byte(rdf), CommitNow: true})
	return err
}

func GetDataset(d DatasetType) *Dataset {
	switch d {
	case OneMillionDataset:
		return &Dataset{name: "1million"}
	case TwentyOneMillionDataset:
		return &Dataset{name: "21million"}
	default:
		panic("unknown dataset type")
	}
}

func (d *Dataset) DataFilePath() string {
	return d.ensureFile(fmt.Sprintf("%s.rdf.gz", d.name))
}

func (d *Dataset) SchemaPath() string {
	return d.ensureFile(fmt.Sprintf("%s.schema", d.name))
}

func (d *Dataset) GqlSchemaPath() string {
	return d.ensureFile(fmt.Sprintf("%s.gql_schema", d.name))
}

func (d *Dataset) ensureFile(filename string) string {
	fullPath := filepath.Join(datasetFilesPath, filename)
	if exists, _ := fileExists(fullPath); !exists {
		url, ok := datafiles[filename]
		if !ok {
			panic(fmt.Sprintf("dataset file %s not found in datafiles map", filename))
		}
		if err := downloadFile(filename, url); err != nil {
			panic(fmt.Sprintf("failed to download %s: %v", filename, err))
		}
	}
	return fullPath
}

func downloadFile(fname, url string) error {
	cmd := exec.Command("wget", "-O", fname, url)
	cmd.Dir = datasetFilesPath

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("error downloading file %s: %s", fname, string(out))
	}
	return nil
}
