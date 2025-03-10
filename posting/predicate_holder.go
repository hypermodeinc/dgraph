/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package posting

import (
	"context"
	"encoding/binary"
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
}

func newPredicateHolder(attr string, startTs uint64) *PredicateHolder {
	return &PredicateHolder{
		attr:       attr,
		plists:     make(map[string]*List),
		deltas:     make(map[string][]byte),
		dataLists:  make(map[uint64]*List),
		indexLists: make(map[string]*List),
		startTs:    startTs,
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
}

func (ph *PredicateHolder) UpdateUidDelta() {
	dataKey := x.DataKey(ph.attr, 0)
	ph.Lock()
	defer ph.Unlock()
	for uid, list := range ph.dataLists {
		binary.BigEndian.PutUint64(dataKey[len(dataKey)-8:], uid)
		ph.deltas[string(dataKey)] = list.getMutationAndRelease(ph.startTs)
	}
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
	return ph.getScalarList(key)
}

func (ph *PredicateHolder) getScalarList(key []byte) (*List, error) {
	l, err := ph.getFromDelta(key)
	if err != nil {
		return nil, err
	}
	if l.mutationMap.len() == 0 && len(l.plist.Postings) == 0 {
		pl, err := ph.GetSinglePosting(key)
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
