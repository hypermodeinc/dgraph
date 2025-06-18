/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package bulk

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hypermodeinc/dgraph/v25/x"
)

const (
	mapShardDir    = "map_output"
	reduceShardDir = "shards"
	bufferDir      = "buffer"
)

func mergeMapShardsIntoReduceShards(opt *BulkOptions) {
	if opt == nil {
		fmt.Printf("Nil options passed to merge shards phase.\n")
		os.Exit(1)
	}

	shardDirs := readShardDirs(filepath.Join(opt.TmpDir, mapShardDir))
	if len(shardDirs) == 0 {
		fmt.Printf(
			"No map shards found. Possibly caused by empty data files passed to the bulk loader.\n")
		os.Exit(1)
	}

	// First shard is handled differently because it contains reserved predicates.
	firstShard := shardDirs[0]
	// Sort the rest of the shards by size to allow the largest shards to be shuffled first.
	shardDirs = shardDirs[1:]
	sortBySize(shardDirs)

	var reduceShards []string
	for i := range opt.ReduceShards {
		shardDir := filepath.Join(opt.TmpDir, reduceShardDir, fmt.Sprintf("shard_%d", i))
		x.Check(os.MkdirAll(shardDir, 0750))
		reduceShards = append(reduceShards, shardDir)
	}

	// Put the first map shard in the first reduce shard since it contains all the reserved
	// predicates. We want all the reserved predicates in group 1.
	reduceShard := filepath.Join(reduceShards[0], filepath.Base(firstShard))
	fmt.Printf("Shard %s -> Reduce %s\n", firstShard, reduceShard)
	x.Check(os.Rename(firstShard, reduceShard))

	// Heuristic: put the largest map shard into the smallest reduce shard
	// until there are no more map shards left. Should be a good approximation.
	for _, shard := range shardDirs {
		sortBySize(reduceShards)
		reduceShard := filepath.Join(
			reduceShards[len(reduceShards)-1], filepath.Base(shard))
		fmt.Printf("Shard %s -> Reduce %s\n", shard, reduceShard)
		x.Check(os.Rename(shard, reduceShard))
	}
}

func readShardDirs(d string) []string {
	_, err := os.Stat(d)
	if os.IsNotExist(err) {
		return nil
	}
	dir, err := os.Open(d)
	x.Check(err)
	shards, err := dir.Readdirnames(0)
	x.Check(err)
	x.Check(dir.Close())
	for i, shard := range shards {
		shards[i] = filepath.Join(d, shard)
	}
	sort.Strings(shards)
	return shards
}

func filenamesInTree(dir string) []string {
	var fnames []string
	x.Check(filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".gz") {
			fnames = append(fnames, path)
		}
		return nil
	}))
	return fnames
}

type sizedDir struct {
	dir string
	sz  int64
}

// sortBySize sorts the input directories by size of their content (biggest to smallest).
func sortBySize(dirs []string) {
	sizedDirs := make([]sizedDir, len(dirs))
	for i, dir := range dirs {
		sizedDirs[i] = sizedDir{dir: dir, sz: treeSize(dir)}
	}
	sort.SliceStable(sizedDirs, func(i, j int) bool {
		return sizedDirs[i].sz > sizedDirs[j].sz
	})
	for i := range sizedDirs {
		dirs[i] = sizedDirs[i].dir
	}
}

func treeSize(dir string) int64 {
	var sum int64
	x.Check(filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		sum += info.Size()
		return nil
	}))
	return sum
}
