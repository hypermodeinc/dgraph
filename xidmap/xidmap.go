/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package xidmap

import (
	"context"
	"encoding/binary"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgryski/go-farm"
	"github.com/golang/glog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var maxLeaseRegex = regexp.MustCompile(`currMax:([0-9]+)`)

// XidMapOptions specifies the options for creating a new xidmap.
type XidMapOptions struct {
	UidAssigner *grpc.ClientConn
	DgClient    *dgo.Dgraph
	DB          *badger.DB
	Dir         string
}

// XidMap allocates and tracks mappings between Xids and Uids in a threadsafe
// manner. It's memory friendly because the mapping is stored on disk, but fast
// because it uses an LRU cache.
type XidMap struct {
	dg         *dgo.Dgraph
	shards     []*shard
	newRanges  chan *pb.AssignedIds
	zc         pb.ZeroClient
	maxUidSeen uint64

	// Optionally, these can be set to persist the mappings.
	writer *badger.WriteBatch
	wg     sync.WaitGroup

	kvBuf  []kv
	kvChan chan []kv
}

type shard struct {
	sync.RWMutex
	block

	tree *z.Tree
}

type block struct {
	start, end uint64
}

type kv struct {
	key, value []byte
}

// assign assumes the write lock is already acquired.
func (b *block) assign(ch <-chan *pb.AssignedIds) uint64 {
	if b.end == 0 || b.start > b.end {
		newRange := <-ch
		b.start, b.end = newRange.StartId, newRange.EndId
	}
	x.AssertTrue(b.start <= b.end)
	uid := b.start
	b.start++
	return uid
}

// New creates an XidMap. zero conn must be valid for UID allocations to happen. Optionally, a
// badger.DB can be provided to persist the xid to uid allocations. This would add latency to the
// assignment operations. XidMap creates the temporary buffers inside dir directory. The caller must
// ensure that the dir exists.
func New(opts XidMapOptions) *XidMap {
	numShards := 32
	xm := &XidMap{
		newRanges: make(chan *pb.AssignedIds, numShards),
		shards:    make([]*shard, numShards),
		kvChan:    make(chan []kv, 64),
		dg:        opts.DgClient,
	}
	for i := range xm.shards {
		xm.shards[i] = &shard{
			tree: z.NewTree("XidMap"),
		}
	}

	if opts.DB != nil {
		// If DB is provided, let's load up all the xid -> uid mappings in memory.
		xm.writer = opts.DB.NewWriteBatch()

		for range 16 {
			xm.wg.Add(1)
			go xm.dbWriter()
		}

		err := opts.DB.View(func(txn *badger.Txn) error {
			var count int
			opt := badger.DefaultIteratorOptions
			opt.PrefetchValues = false
			itr := txn.NewIterator(opt)
			defer itr.Close()
			for itr.Rewind(); itr.Valid(); itr.Next() {
				item := itr.Item()
				key := string(item.Key())
				sh := xm.shardFor(key)
				err := item.Value(func(val []byte) error {
					uid := binary.BigEndian.Uint64(val)
					// No need to acquire a lock. This is all serial access.
					sh.tree.Set(farm.Fingerprint64([]byte(key)), uid)
					return nil
				})
				if err != nil {
					return err
				}
				count++
			}
			glog.Infof("Loaded up %d xid to uid mappings", count)
			return nil
		})
		x.Check(err)
	}

	if opts.UidAssigner != nil {
		xm.zc = pb.NewZeroClient(opts.UidAssigner)
	}

	go func() {
		const initBackoff = 10 * time.Millisecond
		const maxBackoff = 5 * time.Second
		backoff := initBackoff
		for {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			ctx = xm.attachNamespace(ctx)

			var assigned *pb.AssignedIds
			var err error
			if xm.zc == nil {
				assigned = &pb.AssignedIds{}
				assigned.StartId, assigned.EndId, err = xm.dg.AllocateUIDs(ctx, 1e5)
			} else {
				assigned, err = xm.zc.AssignIds(ctx, &pb.Num{Val: 1e5, Type: pb.Num_UID})
			}
			glog.V(2).Infof("Assigned Uids: %+v. Err: %v", assigned, err)
			cancel()
			if err == nil {
				backoff = initBackoff
				xm.updateMaxSeen(assigned.EndId)
				xm.newRanges <- assigned
				continue
			}
			glog.Errorf("Error while getting lease: %v\n", err)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}

			if x.IsJwtExpired(err) {
				if err := xm.relogin(); err != nil {
					glog.Errorf("While trying to relogin: %v", err)
				}
			}
			time.Sleep(backoff)
		}
	}()
	return xm
}

func (m *XidMap) attachNamespace(ctx context.Context) context.Context {
	if m.dg == nil {
		return ctx
	}

	// Need to attach JWT because slash uses alpha as zero proxy.
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		md = metadata.New(nil)
	}
	md.Set("accessJwt", m.dg.GetJwt().AccessJwt)
	ctx = metadata.NewOutgoingContext(ctx, md)
	return ctx
}

func (m *XidMap) relogin() error {
	if m.dg == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return m.dg.Relogin(ctx)
}

func (m *XidMap) shardFor(xid string) *shard {
	fp := z.MemHashString(xid)
	idx := fp % uint64(len(m.shards))
	return m.shards[idx]
}

func (m *XidMap) CheckUid(xid string) bool {
	sh := m.shardFor(xid)
	sh.RLock()
	defer sh.RUnlock()
	uid := sh.tree.Get(farm.Fingerprint64([]byte(xid)))
	return uid != 0
}

func (m *XidMap) SetUid(xid string, uid uint64) {
	sh := m.shardFor(xid)
	sh.Lock()
	defer sh.Unlock()
	sh.tree.Set(farm.Fingerprint64([]byte(xid)), uid)
	if m.writer != nil {
		var uidBuf [8]byte
		binary.BigEndian.PutUint64(uidBuf[:], uid)
		m.kvBuf = append(m.kvBuf, kv{key: []byte(xid), value: uidBuf[:]})

		if len(m.kvBuf) == 64 {
			m.kvChan <- m.kvBuf
			m.kvBuf = make([]kv, 0, 64)
		}
	}
}

func (m *XidMap) dbWriter() {
	defer m.wg.Done()
	for buf := range m.kvChan {
		for _, kv := range buf {
			x.Panic(m.writer.Set(kv.key, kv.value))
		}
	}
}

// AssignUid creates new or looks up existing XID to UID mappings. It also returns if
// UID was created.
func (m *XidMap) AssignUid(xid string) (uint64, bool) {
	sh := m.shardFor(xid)
	sh.RLock()

	uid := sh.tree.Get(farm.Fingerprint64([]byte(xid)))
	sh.RUnlock()
	if uid > 0 {
		return uid, false
	}

	sh.Lock()
	defer sh.Unlock()

	uid = sh.tree.Get(farm.Fingerprint64([]byte(xid)))
	if uid > 0 {
		return uid, false
	}

	newUid := sh.assign(m.newRanges)
	sh.tree.Set(farm.Fingerprint64([]byte(xid)), newUid)

	if m.writer != nil {
		var uidBuf [8]byte
		binary.BigEndian.PutUint64(uidBuf[:], newUid)
		m.kvBuf = append(m.kvBuf, kv{key: []byte(xid), value: uidBuf[:]})

		if len(m.kvBuf) == 64 {
			m.kvChan <- m.kvBuf
			m.kvBuf = make([]kv, 0, 64)
		}
	}

	return newUid, true
}

func (sh *shard) Current() uint64 {
	sh.RLock()
	defer sh.RUnlock()
	return sh.start
}

func (m *XidMap) updateMaxSeen(max uint64) {
	for {
		prev := atomic.LoadUint64(&m.maxUidSeen)
		if prev >= max {
			return
		}
		if atomic.CompareAndSwapUint64(&m.maxUidSeen, prev, max) {
			return
		}
	}
}

// BumpTo can be used to make Zero allocate UIDs up to this given number. Attempts are made to
// ensure all future allocations of UIDs be higher than this one, but results are not guaranteed.
func (m *XidMap) BumpTo(uid uint64) {
	// If we have a cluster that cannot lease out new UIDs because it has already leased upto its
	// max limit. Now, we try to live load the data with the given UIDs and the AssignIds complains
	// that the limit has reached. Hence, update the xidmap's maxSeenUid and make progress.
	updateLease := func(msg string) {
		if !strings.Contains(msg, "limit has reached. currMax:") {
			return
		}
		matches := maxLeaseRegex.FindAllStringSubmatch(msg, 1)
		if len(matches) == 0 {
			return
		}
		maxUidLeased, err := strconv.ParseUint(matches[0][1], 10, 64)
		if err != nil {
			glog.Errorf("While parsing currMax %+v", err)
			return
		}
		m.updateMaxSeen(maxUidLeased)
	}

	for {
		curMax := atomic.LoadUint64(&m.maxUidSeen)
		if uid <= curMax {
			return
		}
		glog.V(1).Infof("Bumping up to %v", uid)
		num := x.Max(uid-curMax, 1e4)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ctx = m.attachNamespace(ctx)

		var err error
		var assigned *pb.AssignedIds
		if m.zc == nil {
			assigned = &pb.AssignedIds{}
			assigned.StartId, assigned.EndId, err = m.dg.AllocateUIDs(ctx, num)
		} else {
			assigned, err = m.zc.AssignIds(ctx, &pb.Num{Val: num, Type: pb.Num_UID})
		}
		cancel()
		if err == nil {
			glog.V(1).Infof("Requested bump: %d. Got assigned: %v", uid, assigned)
			m.updateMaxSeen(assigned.EndId)
			return
		}
		updateLease(err.Error())
		glog.Errorf("While requesting AssignUids(%d): %v", num, err)
		if x.IsJwtExpired(err) {
			if err := m.relogin(); err != nil {
				glog.Errorf("While trying to relogin: %v", err)
			}
		}
	}
}

// AllocateUid gives a single uid without creating an xid to uid mapping.
func (m *XidMap) AllocateUid() uint64 {
	//nolint:gosec // random index in slice does not require cryptographic precision
	sh := m.shards[rand.Intn(len(m.shards))]
	sh.Lock()
	defer sh.Unlock()
	return sh.assign(m.newRanges)
}

// Flush must be called if DB is provided to XidMap.
func (m *XidMap) Flush() error {
	// While running bulk loader, this method is called at the completion of map phase. After this
	// method returns xidmap of bulk loader is made nil. But xidmap still show up in memory profiles
	// even during reduce phase. If bulk loader is running on large dataset, this occupies lot of
	// memory and causing OOM sometimes. Making shards explicitly nil in this method fixes this.
	// TODO: find why xidmap is not getting GCed without below line.
	for _, shards := range m.shards {
		if err := shards.tree.Close(); err != nil {
			glog.Warningf("error closing shards tree: %v", err)
		}
	}
	m.shards = nil
	if m.writer == nil {
		return nil
	}
	glog.Infof("Writing xid map to DB")
	defer func() {
		glog.Infof("Finished writing xid map to DB")
	}()

	if len(m.kvBuf) > 0 {
		m.kvChan <- m.kvBuf
	}
	close(m.kvChan)
	m.wg.Wait()

	return m.writer.Flush()
}
