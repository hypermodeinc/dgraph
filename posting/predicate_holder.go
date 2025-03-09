/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package posting

import (
	"context"
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
}

func newPredicateHolder(attr string, startTs uint64) *PredicateHolder {
	return &PredicateHolder{
		attr:    attr,
		plists:  make(map[string]*List),
		deltas:  make(map[string][]byte),
		startTs: startTs,
	}
}

func (ph *PredicateHolder) getNoStore(key string) *List {
	ph.RLock()
	defer ph.RUnlock()
	return ph.plists[key]
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
