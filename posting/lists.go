/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package posting

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/x"
)

const (
	mb = 1 << 20
)

var (
	pstore *badger.DB
	closer *z.Closer
)

// Init initializes the posting lists package, the in memory and dirty list hash.
func Init(ps *badger.DB, cacheSize int64, removeOnUpdate bool) {
	pstore = ps
	closer = z.NewCloser(1)
	go x.MonitorMemoryMetrics(closer)

	memoryLayer = initMemoryLayer(cacheSize, removeOnUpdate)
}

func UpdateMaxCost(maxCost int64) {
}

// Cleanup waits until the closer has finished processing.
func Cleanup() {
	closer.SignalAndWait()
}

// GetNoStore returns the list stored in the key or creates a new one if it doesn't exist.
// It does not store the list in any cache.
func GetNoStore(key []byte, readTs uint64) (rlist *List, err error) {
	return getNew(key, pstore, readTs)
}

// LocalCache stores a cache of posting lists and deltas.
// This doesn't sync, so call this only when you don't care about dirty posting lists in
// memory(for example before populating snapshot) or after calling syncAllMarks
type LocalCache struct {
	sync.RWMutex

	startTs  uint64
	commitTs uint64

	// max committed timestamp of the read posting lists.
	maxVersions map[string]uint64

	// plists are posting lists in memory. They can be discarded to reclaim space.
	plists map[string]*PredicateHolder
}

// struct to implement LocalCache interface from vector-indexer
// acts as wrapper for dgraph *LocalCache
type viLocalCache struct {
	delegate *LocalCache
}

func (vc *viLocalCache) Find(prefix []byte, filter func([]byte) bool) (uint64, error) {
	return vc.delegate.Find(prefix, filter)
}

func (vc *viLocalCache) Get(key []byte) ([]byte, error) {
	pl, err := vc.delegate.Get(key)
	if err != nil {
		return nil, err
	}
	pl.Lock()
	defer pl.Unlock()
	return vc.GetValueFromPostingList(pl)
}

func (vc *viLocalCache) GetWithLockHeld(key []byte) ([]byte, error) {
	pl, err := vc.delegate.Get(key)
	if err != nil {
		return nil, err
	}
	return vc.GetValueFromPostingList(pl)
}

func (vc *viLocalCache) GetValueFromPostingList(pl *List) ([]byte, error) {
	if pl.cache != nil {
		return pl.cache, nil
	}
	value := pl.findStaticValue(vc.delegate.startTs)

	if value == nil || len(value.Postings) == 0 {
		return nil, ErrNoValue
	}

	if value.Postings[0].Op == Del {
		return nil, ErrNoValue
	}

	pl.cache = value.Postings[0].Value
	return pl.cache, nil
}

func NewViLocalCache(delegate *LocalCache) *viLocalCache {
	return &viLocalCache{delegate: delegate}
}

// NewLocalCache returns a new LocalCache instance.
func NewLocalCache(startTs uint64) *LocalCache {
	return &LocalCache{
		startTs:     startTs,
		plists:      make(map[string]*PredicateHolder),
		maxVersions: make(map[string]uint64),
	}
}

// NoCache returns a new LocalCache instance, which won't cache anything. Useful to pass startTs
// around.
func NoCache(startTs uint64) *LocalCache {
	return &LocalCache{
		startTs: startTs,
		plists:  make(map[string]*PredicateHolder),
	}
}

func (lc *LocalCache) UpdateCommitTs(commitTs uint64) {
	lc.Lock()
	defer lc.Unlock()
	lc.commitTs = commitTs
}

func (lc *LocalCache) Find(pred []byte, filter func([]byte) bool) (uint64, error) {
	txn := pstore.NewTransactionAt(lc.startTs, false)
	defer txn.Discard()

	attr := string(pred)

	initKey := x.ParsedKey{
		Attr: attr,
	}
	startKey := x.DataKey(attr, 0)
	prefix := initKey.DataPrefix()

	result := &pb.List{}
	var prevKey []byte
	itOpt := badger.DefaultIteratorOptions
	itOpt.PrefetchValues = false
	itOpt.AllVersions = true
	itOpt.Prefix = prefix
	it := txn.NewIterator(itOpt)
	defer it.Close()

	for it.Seek(startKey); it.Valid(); {
		item := it.Item()
		if bytes.Equal(item.Key(), prevKey) {
			it.Next()
			continue
		}
		prevKey = append(prevKey[:0], item.Key()...)

		// Parse the key upfront, otherwise ReadPostingList would advance the
		// iterator.
		pk, err := x.Parse(item.Key())
		if err != nil {
			return 0, err
		}

		// If we have moved to the next attribute, break
		if pk.Attr != attr {
			break
		}

		if pk.HasStartUid {
			// The keys holding parts of a split key should not be accessed here because
			// they have a different prefix. However, the check is being added to guard
			// against future bugs.
			continue
		}

		switch {
		case item.UserMeta()&BitEmptyPosting > 0:
			// This is an empty posting list. So, it should not be included.
			continue
		default:
			// This bit would only be set if there are valid uids in UidPack.
			key := x.DataKey(attr, pk.Uid)
			pl, err := lc.Get(key)
			if err != nil {
				return 0, err
			}
			vals, err := pl.Value(lc.startTs)
			switch {
			case err == ErrNoValue:
				continue
			case err != nil:
				return 0, err
			}

			if filter(vals.Value.([]byte)) {
				return pk.Uid, nil
			}

			continue
		}
	}

	if len(result.Uids) > 0 {
		return result.Uids[0], nil
	}

	return 0, badger.ErrKeyNotFound
}

// getNoStore returns the list for a given key without storing it in cache
func (lc *LocalCache) getNoStore(key string, attr string) *List {
	lc.RLock()
	defer lc.RUnlock()
	if ph, ok := lc.plists[attr]; ok {
		return ph.getNoStore(key)
	}
	return nil
}

func (lc *LocalCache) GetPredicateHolder(attr string) *PredicateHolder {
	lc.RLock()
	defer lc.RUnlock()
	return lc.plists[attr]
}

func (lc *LocalCache) GetOrCreatePredicateHolder(attr string) *PredicateHolder {
	ph := lc.GetPredicateHolder(attr)
	if ph == nil {
		ph = newPredicateHolder(attr, lc.startTs)
		lc.Lock()
		defer lc.Unlock()
		lc.plists[attr] = ph
	}
	return ph
}

// SetIfAbsent adds the list for the specified key to the cache
func (lc *LocalCache) SetIfAbsent(key string, attr string, updated *List) *List {
	ph := lc.GetOrCreatePredicateHolder(attr)
	return ph.SetIfAbsent(key, updated)
}

// Get retrieves the cached version of the list associated with the given key.
func (lc *LocalCache) Get(key []byte) (*List, error) {
	pk, err := x.Parse(key)
	if err != nil {
		return nil, err
	}
	ph := lc.GetOrCreatePredicateHolder(pk.Attr)
	return ph.getInternal(key, true)
}

// GetFromDelta gets the cached version of the list without reading from disk
// and only applies the existing deltas. This is used in situations where the
// posting list will only be modified and not read (e.g adding index mutations).
func (lc *LocalCache) GetFromDelta(key []byte) (*List, error) {
	pk, err := x.Parse(key)
	if err != nil {
		return nil, err
	}
	ph := lc.GetOrCreatePredicateHolder(pk.Attr)
	return ph.getInternal(key, false)
}

// UpdateDeltasAndDiscardLists updates the delta cache before removing the stored posting lists.
func (lc *LocalCache) UpdateDeltasAndDiscardLists() {
	lc.Lock()
	defer lc.Unlock()
	if len(lc.plists) == 0 {
		return
	}

	for _, ph := range lc.plists {
		for key, list := range ph.plists {
			data := list.getMutation(lc.startTs)
			if len(data) > 0 {
				ph.deltas[key] = data
			}
			lc.maxVersions[key] = list.maxVersion()
		}
		ph.plists = make(map[string]*List)
	}
}

func (lc *LocalCache) fillPreds(ctx *api.TxnContext, gid uint32) {
	lc.RLock()
	defer lc.RUnlock()
	for key := range lc.maxVersions {
		pk, err := x.Parse([]byte(key))
		x.Check(err)
		if len(pk.Attr) == 0 {
			continue
		}
		// Also send the group id that the predicate was being served by. This is useful when
		// checking if Zero should allow a commit during a predicate move.
		predKey := fmt.Sprintf("%d-%s", gid, pk.Attr)
		ctx.Preds = append(ctx.Preds, predKey)
	}
	ctx.Preds = x.Unique(ctx.Preds)
}
