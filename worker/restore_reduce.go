//go:build !oss
// +build !oss

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 */

package worker

import (
	"bufio"
	"encoding/binary"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/golang/glog"
	"github.com/klauspost/compress/s2"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/badger/v4/y"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/x"
)

const (
	mapFileSz      int = math.MaxInt32
	partitionBufSz int = 4 << 20
)

type mapIterator struct {
	fd     *os.File
	reader *bufio.Reader
	meBuf  []byte
}

func (mi *mapIterator) Next(cbuf *z.Buffer, partitionKey []byte) error {
	readMapEntry := func() error {
		if len(mi.meBuf) > 0 {
			return nil
		}
		r := mi.reader
		sizeBuf, err := r.Peek(binary.MaxVarintLen64)
		if err != nil {
			return err
		}
		sz, n := binary.Uvarint(sizeBuf)
		if n <= 0 {
			log.Fatalf("Could not read uvarint: %d", n)
		}
		x.Check2(r.Discard(n))
		if cap(mi.meBuf) < int(sz) {
			mi.meBuf = make([]byte, int(sz))
		}
		mi.meBuf = mi.meBuf[:int(sz)]
		x.Check2(io.ReadFull(r, mi.meBuf))
		return nil
	}
	for {
		if err := readMapEntry(); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		key := mapEntry(mi.meBuf).Key()

		if len(partitionKey) == 0 || y.CompareKeys(key, partitionKey) < 0 {
			b := cbuf.SliceAllocate(len(mi.meBuf))
			copy(b, mi.meBuf)
			mi.meBuf = mi.meBuf[:0]
			// map entry is already part of cBuf.
			continue
		}
		// Current key is not part of this batch so track that we have already read the key.
		return nil
	}
	return nil
}

func (mi *mapIterator) Close() error {
	return mi.fd.Close()
}

func newMapIterator(filename string) (*pb.MapHeader, *mapIterator) {
	fd, err := os.Open(filename)
	x.Check(err)
	r := s2.NewReader(fd)

	// Read the header size.
	reader := bufio.NewReaderSize(r, 16<<10)
	headerLenBuf := make([]byte, 4)
	x.Check2(io.ReadFull(reader, headerLenBuf))
	headerLen := binary.BigEndian.Uint32(headerLenBuf)
	// Reader the map header.
	headerBuf := make([]byte, headerLen)

	x.Check2(io.ReadFull(reader, headerBuf))
	header := &pb.MapHeader{}
	err = proto.Unmarshal(headerBuf, header)
	x.Check(err)

	itr := &mapIterator{
		fd:     fd,
		reader: reader,
	}
	return header, itr
}

type reducer struct {
	mapDir        string
	mapItrs       []*mapIterator
	partitionKeys [][]byte
	bufferCh      chan *z.Buffer
	w             Writer

	bytesProcessed uint64
	bytesRead      uint64
}

type Writer interface {
	Write(buf *z.Buffer) error
}

func RunReducer(w Writer, mapDir string) error {
	r := &reducer{
		w:        w,
		bufferCh: make(chan *z.Buffer, 10),
		mapDir:   mapDir,
	}
	closer := z.NewCloser(1)
	defer closer.SignalAndWait()
	go r.Progress(closer)

	return r.Reduce()
}

func (r *reducer) Progress(closer *z.Closer) {
	defer closer.Done()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	start := time.Now()
	update := func() {
		since := time.Since(start)
		read := atomic.LoadUint64(&r.bytesRead)
		proc := atomic.LoadUint64(&r.bytesProcessed)
		pr := uint64(float64(proc) / since.Seconds())
		glog.Infof(
			"Restore REDUCE %s read: %s. processed: %s. rate: %s/sec. jemalloc: %s.\n",
			x.FixedDuration(since), humanize.IBytes(read), humanize.IBytes(proc),
			humanize.IBytes(pr), humanize.IBytes(uint64(z.NumAllocBytes())))
	}
	for {
		select {
		case <-closer.HasBeenClosed():
			update()
			glog.Infof("Restore REDUCE Done in %s.\n", x.FixedDuration(time.Since(start)))
			return
		case <-ticker.C:
			update()
		}
	}
}

func (r *reducer) Reduce() error {
	var files []string

	var total int64
	f := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(info.Name(), ".map") {
			files = append(files, path)
			total += info.Size()
		}
		return nil
	}

	if err := filepath.Walk(r.mapDir, f); err != nil {
		return err
	}
	glog.Infof("Got %d map files of compressed size: %s.\n",
		len(files), humanize.IBytes(uint64(total)))

	// Pick up map iterators and partition keys.
	partitions := make(map[string]struct{})
	for _, fname := range files {
		header, itr := newMapIterator(fname)
		for _, k := range header.PartitionKeys {
			if len(k) == 0 {
				continue
			}
			partitions[string(k)] = struct{}{}
		}
		r.mapItrs = append(r.mapItrs, itr)
	}

	keys := make([][]byte, 0, len(partitions))
	for k := range partitions {
		keys = append(keys, []byte(k))
	}
	sort.Slice(keys, func(i, j int) bool {
		return y.CompareKeys(keys[i], keys[j]) < 0
	})
	// Append nil for the last entries.
	keys = append(keys, nil)
	r.partitionKeys = keys

	errCh := make(chan error, 2)
	go func() {
		errCh <- r.blockingRead()
	}()
	go func() {
		errCh <- r.process()
	}()

	for range 2 {
		if err := <-errCh; err != nil {
			return err
		}
	}
	return nil
}

func (r *reducer) blockingRead() error {
	cbuf := z.NewBuffer(64<<20, "Restore.GetBuf")

	sortAndPush := func(buf *z.Buffer) {
		// Let's sort here. So, there's less work for processor.
		buf.SortSlice(func(ls, rs []byte) bool {
			lme := mapEntry(ls)
			rme := mapEntry(rs)
			return y.CompareKeys(lme.Key(), rme.Key()) < 0
		})
		atomic.AddUint64(&r.bytesRead, uint64(buf.LenNoPadding()))
		r.bufferCh <- buf
	}
	for _, pkey := range r.partitionKeys {
		for _, itr := range r.mapItrs {
			if err := itr.Next(cbuf, pkey); err != nil {
				defer func() {
					if ierr := cbuf.Release(); ierr != nil {
						glog.Warningf("error in releasing buffer: %v", ierr)
					}
				}()

				return err
			}
		}
		if cbuf.LenNoPadding() < 256<<20 {
			// Pick up more data.
			continue
		}
		sortAndPush(cbuf)
		cbuf = z.NewBuffer(64<<20, "Restore.GetBuf")
	}

	if !cbuf.IsEmpty() {
		sortAndPush(cbuf)
	} else {
		if err := cbuf.Release(); err != nil {
			glog.Warningf("error in releasing buffer: %v", err)
		}
	}
	close(r.bufferCh)
	return nil
}

func (r *reducer) process() error {
	if r.w == nil {
		return nil
	}
	writer := r.w

	kvBuf := z.NewBuffer(64<<20, "Restore.GetBuf")
	defer func() {
		if ierr := kvBuf.Release(); ierr != nil {
			glog.Warningf("error in releasing buffer: %v", ierr)
		}
	}()

	var lastKey []byte
	for cbuf := range r.bufferCh {
		err := cbuf.SliceIterate(func(s []byte) error {
			me := mapEntry(s)
			key := me.Key()

			// Don't need to pick multiple versions of the same key.
			if y.SameKey(key, lastKey) {
				return nil
			}
			lastKey = append(lastKey[:0], key...)

			kvBuf.WriteSlice(me.Data())
			return nil
		})
		if err != nil {
			return err
		}

		atomic.AddUint64(&r.bytesProcessed, uint64(cbuf.LenNoPadding()))
		if err := writer.Write(kvBuf); err != nil {
			return err
		}
		kvBuf.Reset()
		if err := cbuf.Release(); err != nil {
			glog.Warningf("error in releasing buffer: %v", err)
		}
	} // end loop for bufferCh
	return nil
}
