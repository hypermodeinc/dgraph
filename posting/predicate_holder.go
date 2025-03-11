/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package posting

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger/v4"
	ostats "go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"google.golang.org/protobuf/proto"

	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/x"
)

type PredicateHolder struct {
	x.SafeMutex
	attr    string
	startTs uint64

	// plists are posting lists in memory. They can be discarded to reclaim space.
	plists map[string]*List

	// The keys for these maps is a string representation of the Badger key for the posting list.
	// deltas keep track of the updates made by txn. These must be kept around until written to disk
	// during commit.
	deltas map[string][]byte // Store deltas at predicate level

	dataLists  map[uint64]*List
	indexLists map[string]*List

	// Batches for efficient allocation
	batch        []*postingListBatch
	postingBatch []*postingBatch
}

func newPredicateHolder(attr string, startTs uint64) *PredicateHolder {
	fmt.Println("newPredicateHolder", numNewPostingListBatches, numNewPostingBatches, numGetPostingListBatches, numGetPostingBatches, numPutPostingListBatches, numPutPostingBatches)
	return &PredicateHolder{
		attr:         attr,
		plists:       make(map[string]*List),
		deltas:       make(map[string][]byte),
		dataLists:    make(map[uint64]*List),
		indexLists:   make(map[string]*List),
		startTs:      startTs,
		batch:        []*postingListBatch{postingListPool.New().(*postingListBatch)},
		postingBatch: []*postingBatch{postingPool.New().(*postingBatch)},
	}
}

func (ph *PredicateHolder) GetDataList(uid uint64) *List {
	ph.RLock()
	defer ph.RUnlock()
	return ph.dataLists[uid]
}

func (ph *PredicateHolder) SetDataList(uid uint64, updated *List) {
	ph.Lock()
	defer ph.Unlock()
	ph.dataLists[uid] = updated
}

func (ph *PredicateHolder) GetPartialIndexList(token string) (*List, error) {
	ph.Lock()
	defer ph.Unlock()
	if val, ok := ph.indexLists[token]; !ok {
		key := x.IndexKey(ph.attr, token)
		pl, err := ph.readPostingListAt(key)
		if err != nil && err != badger.ErrKeyNotFound {
			return nil, err
		}

		l := &List{
			key:         key,
			mutationMap: newMutableLayer(),
		}

		if pl != nil {
			l.mutationMap.setCurrentEntries(ph.startTs, pl)
		}
		ph.indexLists[token] = l
		return l, nil
	} else {
		return val, nil
	}
}

func (ph *PredicateHolder) GetIndexListFromDisk(token string) (*List, error) {
	ph.Lock()
	defer ph.Unlock()
	if val, ok := ph.indexLists[token]; !ok {
		key := x.IndexKey(ph.attr, token)
		pl, err := getNew(key, pstore, ph.startTs)
		if err != nil {
			return nil, err
		}
		ph.indexLists[token] = pl
		return pl, nil
	} else {
		return val, nil
	}
}

func (ph *PredicateHolder) GetIndexListFromDelta(token string) (*List, error) {
	ph.Lock()
	defer ph.Unlock()
	if _, ok := ph.indexLists[token]; !ok {
		ph.indexLists[token] = &List{
			mutationMap: newMutableLayer(),
		}
	}
	return ph.indexLists[token], nil
}

func (ph *PredicateHolder) GetPartialDataList(uid uint64) (*List, error) {
	ph.Lock()
	defer ph.Unlock()
	if val, ok := ph.dataLists[uid]; !ok {
		key := x.DataKey(ph.attr, uid)
		pl, err := ph.readPostingListAt(key)
		if err != nil && err != badger.ErrKeyNotFound {
			return nil, err
		}

		l := &List{
			key:         key,
			mutationMap: newMutableLayer(),
		}

		if pl != nil {
			l.mutationMap.setCurrentEntries(ph.startTs, pl)
		}
		ph.dataLists[uid] = l
		return l, nil
	} else {
		return val, nil
	}
}

func (ph *PredicateHolder) GetDataListFromDisk(uid uint64) (*List, error) {
	ph.Lock()
	defer ph.Unlock()
	if val, ok := ph.dataLists[uid]; !ok {
		key := x.DataKey(ph.attr, uid)
		pl, err := getNew(key, pstore, ph.startTs)
		if err != nil {
			return nil, err
		}
		ph.dataLists[uid] = pl
		return pl, nil
	} else {
		return val, nil
	}
}

func (ph *PredicateHolder) GetDataListFromDelta(uid uint64) (*List, error) {
	ph.Lock()
	defer ph.Unlock()
	if _, ok := ph.dataLists[uid]; !ok {
		ph.dataLists[uid] = &List{
			mutationMap: newMutableLayer(),
		}
	}
	return ph.dataLists[uid], nil
}

func (ph *PredicateHolder) UpdateIndexDelta() {
	ph.Lock()
	defer ph.Unlock()
	for token, list := range ph.indexLists {
		dataKey := x.IndexKey(ph.attr, token)
		ph.deltas[string(dataKey)] = list.getMutationAndRelease(ph.startTs)
	}
	ph.indexLists = make(map[string]*List)
}

func (ph *PredicateHolder) UpdateUidDelta() {
	dataKey := x.DataKey(ph.attr, 0)
	ph.Lock()
	defer ph.Unlock()
	for uid, list := range ph.dataLists {
		binary.BigEndian.PutUint64(dataKey[len(dataKey)-8:], uid)
		data := list.getMutationAndRelease(ph.startTs)
		ph.deltas[string(dataKey)] = data
	}
	ph.dataLists = make(map[uint64]*List)
}

func (ph *PredicateHolder) SetIfAbsent(key string, updated *List) *List {
	ph.Lock()
	defer ph.Unlock()
	if _, ok := ph.plists[key]; !ok {
		ph.plists[key] = updated
	}
	return ph.plists[key]
}

func (ph *PredicateHolder) Get(key []byte) (*List, error) {
	return ph.getInternal(key, true)
}

func (ph *PredicateHolder) Delete(key string) {
	ph.Lock()
	defer ph.Unlock()
	delete(ph.plists, key)
}

func (ph *PredicateHolder) Len() int {
	ph.RLock()
	defer ph.RUnlock()
	return len(ph.plists)
}

func (ph *PredicateHolder) getInternal(key []byte, readFromDisk bool) (*List, error) {
	skey := string(key)

	// Try to get from cache first
	ph.RLock()
	if list, ok := ph.plists[skey]; ok {
		ph.RUnlock()
		return list, nil
	}
	ph.RUnlock()

	// Create new list if not found
	var pl *List
	if readFromDisk {
		var err error
		pl, err = getNew(key, pstore, ph.startTs)
		if err != nil {
			return nil, err
		}
	} else {
		pl = &List{
			key:         key,
			plist:       new(pb.PostingList),
			mutationMap: newMutableLayer(),
		}
	}

	// Apply any pending deltas
	ph.RLock()
	if delta, ok := ph.deltas[skey]; ok && len(delta) > 0 {
		pl.setMutation(ph.startTs, delta)
	}
	ph.RUnlock()

	return ph.SetIfAbsent(skey, pl), nil
}

func (ph *PredicateHolder) readPostingListAt(key []byte) (*pb.PostingList, error) {
	start := time.Now()
	defer func() {
		pk, _ := x.Parse(key)
		ms := x.SinceMs(start)
		var tags []tag.Mutator
		tags = append(tags, tag.Upsert(x.KeyMethod, "get"))
		tags = append(tags, tag.Upsert(x.KeyStatus, pk.Attr))
		_ = ostats.RecordWithTags(context.Background(), tags, x.BadgerReadLatencyMs.M(ms))
	}()

	pl := &pb.PostingList{}
	txn := pstore.NewTransactionAt(ph.startTs, false)
	defer txn.Discard()

	item, err := txn.Get(key)
	if err != nil {
		return nil, err
	}

	err = item.Value(func(val []byte) error {
		return proto.Unmarshal(val, pl)
	})

	return pl, err
}

func (ph *PredicateHolder) GetScalarList(key []byte) (*List, error) {
	l, err := ph.getScalarList(key)
	if err != nil {
		return l, err
	}
	l = ph.SetIfAbsent(string(key), l)
	//fmt.Println("GET SCALAR LIST", ph.plists)
	return l, nil
}

func (ph *PredicateHolder) getScalarList(key []byte) (*List, error) {
	//fmt.Println("GETTING SCALAR LIST", key)
	l, err := ph.getFromDelta(key)
	if err != nil {
		return nil, err
	}
	if l.mutationMap.len() == 0 && len(l.plist.Postings) == 0 {
		pl, err := ph.readPostingListAt(key)
		if err == badger.ErrKeyNotFound {
			return l, nil
		}
		if err != nil {
			return nil, err
		}
		if pl.CommitTs == 0 {
			l.mutationMap.setCurrentEntries(ph.startTs, pl)
		} else {
			l.mutationMap.insertCommittedPostings(pl)
		}
	}
	return l, nil
}

func (ph *PredicateHolder) GetSinglePosting(key []byte) (*pb.PostingList, error) {
	skey := string(key)

	// Check deltas first
	checkInMemory := func() (*pb.PostingList, error) {
		ph.RLock()
		if delta, ok := ph.deltas[skey]; ok && len(delta) > 0 {
			ph.RUnlock()
			pl := &pb.PostingList{}
			err := proto.Unmarshal(delta, pl)
			return pl, err
		}

		// Check cached list
		if list, ok := ph.plists[skey]; ok {
			ph.RUnlock()
			return list.StaticValue(ph.startTs)
		}
		ph.RUnlock()
		return nil, nil
	}

	// Read from disk if not found
	getPostings := func() (*pb.PostingList, error) {
		pl, err := checkInMemory()
		// If both pl and err are empty, that means that there was no data in local cache, hence we should
		// read the data from badger.
		if pl != nil || err != nil {
			return pl, err
		}

		return ph.readPostingListAt(key)
	}

	pl, err := getPostings()
	if err == badger.ErrKeyNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Filter and remove STAR_ALL and OP_DELETE Postings
	idx := 0
	for _, postings := range pl.Postings {
		if hasDeleteAll(postings) {
			return nil, nil
		}
		if postings.Op != Del {
			pl.Postings[idx] = postings
			idx++
		}
	}
	pl.Postings = pl.Postings[:idx]
	return pl, nil
}

func (ph *PredicateHolder) GetFromDelta(key []byte) (*List, error) {
	return ph.getFromDelta(key)
}

func (ph *PredicateHolder) getFromDelta(key []byte) (*List, error) {
	return ph.getInternal(key, false)
}

var numNewPostingListBatches = int64(0)
var numGetPostingListBatches = int64(0)
var numPutPostingListBatches = int64(0)

var numNewPostingBatches = int64(0)
var numGetPostingBatches = int64(0)
var numPutPostingBatches = int64(0)

var (
	// Pool for efficiently allocating batches of pb.PostingList objects
	postingListPool = sync.Pool{
		New: func() interface{} {
			batch := &postingListBatch{
				lists: make([]*pb.PostingList, initialBatchSize),
			}
			// Initialize all lists in the batch
			for i := 0; i < initialBatchSize; i++ {
				batch.lists[i] = &pb.PostingList{
					Postings: make([]*pb.Posting, 0),
				}
			}
			atomic.AddInt64(&numNewPostingListBatches, 1)
			return batch
		},
	}

	// Pool for efficiently allocating batches of pb.Posting objects
	postingPool = sync.Pool{
		New: func() interface{} {
			batch := &postingBatch{
				postings: make([]*pb.Posting, initialBatchSize),
			}
			// Initialize all postings in the batch
			for i := 0; i < initialBatchSize; i++ {
				batch.postings[i] = &pb.Posting{}
			}
			atomic.AddInt64(&numNewPostingBatches, 1)
			return batch
		},
	}
)

func (ph *PredicateHolder) getPostingFromPool() *pb.Posting {
	if len(ph.postingBatch) == 0 {
		ph.postingBatch = []*postingBatch{postingPool.New().(*postingBatch)}
		atomic.AddInt64(&numGetPostingBatches, 1)
	}

	lastBatch := ph.postingBatch[len(ph.postingBatch)-1]
	if lastBatch.nextIdx >= len(lastBatch.postings) {
		// Batch is full, get a new one
		ph.postingBatch = append(ph.postingBatch, postingPool.New().(*postingBatch))
		lastBatch = ph.postingBatch[len(ph.postingBatch)-1]
		atomic.AddInt64(&numGetPostingBatches, 1)
	}

	posting := lastBatch.postings[lastBatch.nextIdx]
	lastBatch.nextIdx++

	// Reset the posting before returning
	posting.Reset()
	return posting
}

func (ph *PredicateHolder) getPostingListFromPool() *pb.PostingList {
	if len(ph.batch) == 0 {
		atomic.AddInt64(&numGetPostingListBatches, 1)
		ph.batch = []*postingListBatch{postingListPool.New().(*postingListBatch)}
	}

	lastBatch := ph.batch[len(ph.batch)-1]
	if lastBatch.nextIdx >= len(lastBatch.lists) {
		// Batch is full, get a new one
		atomic.AddInt64(&numGetPostingListBatches, 1)
		ph.batch = append(ph.batch, postingListPool.New().(*postingListBatch))
		lastBatch = ph.batch[len(ph.batch)-1]
	}

	list := lastBatch.lists[lastBatch.nextIdx]
	lastBatch.nextIdx++

	// Reset the list before returning
	list.Postings = list.Postings[:0]
	return list
}

func (ph *PredicateHolder) releaseAll() {
	atomic.AddInt64(&numPutPostingListBatches, int64(len(ph.batch)))
	atomic.AddInt64(&numPutPostingBatches, int64(len(ph.postingBatch)))
	for _, batch := range ph.batch {
		postingListPool.Put(batch)
	}
	ph.batch = nil

	for _, batch := range ph.postingBatch {
		postingPool.Put(batch)
	}
	ph.postingBatch = nil
}
