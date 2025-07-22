/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package posting

import (
	"context"
	"encoding/hex"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/golang/glog"
	ostats "go.opencensus.io/stats"
	"go.opentelemetry.io/otel/trace"

	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/tok/index"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var o *oracle

// Oracle returns the global oracle instance.
// TODO: Oracle should probably be located in worker package, instead of posting
// package now that we don't run inSnapshot anymore.
func Oracle() *oracle {
	return o
}

func init() {
	o = new(oracle)
	o.init()
}

// Txn represents a transaction.
type Txn struct {
	StartTs uint64

	// atomic
	shouldAbort uint32
	// Fields which can changed after init
	sync.Mutex

	// Keeps track of conflict keys that should be used to determine if this
	// transaction conflicts with another.
	conflicts map[uint64]struct{}

	// Keeps track of last update wall clock. We use this fact later to
	// determine unhealthy, stale txns.
	lastUpdate time.Time

	cache *LocalCache // This pointer does not get modified.

	Span trace.Span
}

// struct to implement Txn interface from vector-indexer
// acts as wrapper for dgraph *Txn
type viTxn struct {
	delegate *Txn
}

func NewViTxn(delegate *Txn) *viTxn {
	return &viTxn{delegate: delegate}
}

func (vt *viTxn) Find(prefix []byte, filter func([]byte) bool) (uint64, error) {
	return vt.delegate.cache.Find(prefix, filter)
}

func (vt *viTxn) StartTs() uint64 {
	return vt.delegate.StartTs
}

func (vt *viTxn) Get(key []byte) ([]byte, error) {
	pl, err := vt.delegate.cache.Get(key)
	if err != nil {
		return nil, err
	}
	pl.Lock()
	defer pl.Unlock()
	return vt.GetValueFromPostingList(pl)
}

func (vt *viTxn) GetWithLockHeld(key []byte) ([]byte, error) {
	pl, err := vt.delegate.cache.Get(key)
	if err != nil {
		return nil, err
	}
	return vt.GetValueFromPostingList(pl)
}

func (vt *viTxn) GetValueFromPostingList(pl *List) ([]byte, error) {
	if pl.cache != nil {
		return pl.cache, nil
	}
	value := pl.findStaticValue(vt.delegate.StartTs)

	if value == nil || len(value.Postings) == 0 {
		return nil, ErrNoValue
	}

	if value.Postings[0].Op == Del {
		return nil, ErrNoValue
	}

	pl.cache = value.Postings[0].Value
	return pl.cache, nil
}

func (vt *viTxn) AddMutation(ctx context.Context, key []byte, t *index.KeyValue) error {
	pl, err := vt.delegate.cache.Get(key)
	if err != nil {
		return err
	}
	return pl.addMutation(ctx, vt.delegate, indexEdgeToPbEdge(t))
}

func (vt *viTxn) AddMutationWithLockHeld(ctx context.Context, key []byte, t *index.KeyValue) error {
	pl, err := vt.delegate.cache.Get(key)
	if err != nil {
		return err
	}
	return pl.addMutationInternal(ctx, vt.delegate, indexEdgeToPbEdge(t))
}

func (vt *viTxn) LockKey(key []byte) {
	pl, _ := vt.delegate.cache.Get(key)
	pl.Lock()
}

func (vt *viTxn) UnlockKey(key []byte) {
	pl, _ := vt.delegate.cache.Get(key)
	pl.Unlock()
}

// NewTxn returns a new Txn instance.
func NewTxn(startTs uint64) *Txn {
	return &Txn{
		StartTs:    startTs,
		cache:      NewLocalCache(startTs),
		lastUpdate: time.Now(),
	}
}

// Get retrieves the posting list for the given list from the local cache.
func (txn *Txn) Get(key []byte) (*List, error) {
	return txn.cache.Get(key)
}

// GetFromDelta retrieves the posting list from delta cache, not from Badger.
func (txn *Txn) GetFromDelta(key []byte) (*List, error) {
	return txn.cache.GetFromDelta(key)
}

func (txn *Txn) GetScalarList(key []byte) (*List, error) {
	l, err := txn.cache.GetFromDelta(key)
	if err != nil {
		return nil, err
	}
	l.Lock()
	defer l.Unlock()
	if l.mutationMap.len() == 0 && len(l.plist.Postings) == 0 {
		pl, err := txn.cache.readPostingListAt(key)
		if err == badger.ErrKeyNotFound {
			return l, nil
		}
		if err != nil {
			return nil, err
		}

		if pl.Pack != nil {
			l.plist = pl
		} else {
			if pl.CommitTs == 0 {
				l.mutationMap.setCurrentEntries(txn.StartTs, pl)
			} else {
				l.mutationMap.insertCommittedPostings(pl)
			}
		}
	}
	return l, nil
}

// Update calls UpdateDeltasAndDiscardLists on the local cache.
func (txn *Txn) Update() {
	txn.cache.UpdateDeltasAndDiscardLists()
}

// Store is used by tests.
func (txn *Txn) Store(pl *List) *List {
	return txn.cache.SetIfAbsent(string(pl.key), pl)
}

type oracle struct {
	x.SafeMutex

	// max start ts given out by Zero. Do not use mutex on this, only use atomics.
	maxAssigned uint64

	// Keeps track of all the startTs we have seen so far, based on the mutations. Then as
	// transactions are committed or aborted, we delete entries from the startTs map. When taking a
	// snapshot, we need to know the minimum start ts present in the map, which represents a
	// mutation which has not yet been committed or aborted.  As we iterate over entries, we should
	// only discard those whose StartTs is below this minimum pending start ts.
	pendingTxns map[uint64]*Txn

	// Used for waiting logic for transactions with startTs > maxpending so that we don't read an
	// uncommitted transaction.
	waiters map[uint64][]chan struct{}
}

func (o *oracle) init() {
	o.waiters = make(map[uint64][]chan struct{})
	o.pendingTxns = make(map[uint64]*Txn)
}

func (o *oracle) RegisterStartTs(ts uint64) *Txn {
	o.Lock()
	defer o.Unlock()
	txn, ok := o.pendingTxns[ts]
	if ok {
		txn.lastUpdate = time.Now()
	} else {
		txn = NewTxn(ts)
		o.pendingTxns[ts] = txn
	}
	return txn
}

func (o *oracle) CacheAt(ts uint64) *LocalCache {
	o.RLock()
	defer o.RUnlock()
	txn, ok := o.pendingTxns[ts]
	if !ok {
		return nil
	}
	return txn.cache
}

// MinPendingStartTs returns the min start ts which is currently pending a commit or abort decision.
func (o *oracle) MinPendingStartTs() uint64 {
	o.RLock()
	defer o.RUnlock()
	min := uint64(math.MaxUint64)
	for ts := range o.pendingTxns {
		if ts < min {
			min = ts
		}
	}
	return min
}

func (o *oracle) NumPendingTxns() int {
	o.RLock()
	defer o.RUnlock()
	return len(o.pendingTxns)
}

func (o *oracle) TxnOlderThan(dur time.Duration) (res []uint64) {
	o.RLock()
	defer o.RUnlock()

	cutoff := time.Now().Add(-dur)
	for startTs, txn := range o.pendingTxns {
		if txn.lastUpdate.Before(cutoff) {
			res = append(res, startTs)
		}
	}
	return res
}

func (o *oracle) addToWaiters(startTs uint64) (chan struct{}, bool) {
	if startTs <= o.MaxAssigned() {
		return nil, false
	}
	o.Lock()
	defer o.Unlock()
	// Check again after acquiring lock, because o.waiters is being processed serially. So, if we
	// don't check here, then it's possible that we add to waiters here, but MaxAssigned has already
	// moved past startTs.
	if startTs <= o.MaxAssigned() {
		return nil, false
	}
	ch := make(chan struct{})
	o.waiters[startTs] = append(o.waiters[startTs], ch)
	return ch, true
}

func (o *oracle) MaxAssigned() uint64 {
	return atomic.LoadUint64(&o.maxAssigned)
}

func (o *oracle) WaitForTs(ctx context.Context, startTs uint64) error {
	ch, ok := o.addToWaiters(startTs)
	if !ok {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *oracle) ProcessDelta(delta *pb.OracleDelta) {
	if glog.V(3) {
		glog.Infof("ProcessDelta: Max Assigned: %d", delta.MaxAssigned)
		glog.Infof("ProcessDelta: Group checksum: %v", delta.GroupChecksums)
		for _, txn := range delta.Txns {
			if txn.CommitTs == 0 {
				glog.Infof("ProcessDelta Aborted: %d", txn.StartTs)
			} else {
				glog.Infof("ProcessDelta Committed: %d -> %d", txn.StartTs, txn.CommitTs)
			}
		}
	}

	o.Lock()
	defer o.Unlock()
	for _, status := range delta.Txns {
		txn := o.pendingTxns[status.StartTs]
		if txn != nil && status.CommitTs > 0 {
			for k := range txn.cache.deltas {
				IncrRollup.addKeyToBatch([]byte(k), 0)
			}
		}
		delete(o.pendingTxns, status.StartTs)
	}
	curMax := o.MaxAssigned()
	if delta.MaxAssigned < curMax {
		return
	}

	// Notify the waiting cattle.
	for startTs, toNotify := range o.waiters {
		if startTs > delta.MaxAssigned {
			continue
		}
		for _, ch := range toNotify {
			close(ch)
		}
		delete(o.waiters, startTs)
	}
	x.AssertTrue(atomic.CompareAndSwapUint64(&o.maxAssigned, curMax, delta.MaxAssigned))
	ostats.Record(context.Background(),
		x.MaxAssignedTs.M(int64(delta.MaxAssigned))) // Can't access o.MaxAssigned without atomics.
}

func (o *oracle) ResetTxns() {
	o.Lock()
	defer o.Unlock()
	o.pendingTxns = make(map[uint64]*Txn)
}

// ResetTxnForNs deletes all the pending transactions for a given namespace.
func (o *oracle) ResetTxnsForNs(ns uint64) {
	txns := o.IterateTxns(func(key []byte) bool {
		pk, err := x.Parse(key)
		if err != nil {
			glog.Errorf("error %v while parsing key %v", err, hex.EncodeToString(key))
			return false
		}
		return x.ParseNamespace(pk.Attr) == ns
	})
	o.Lock()
	defer o.Unlock()
	for _, txn := range txns {
		delete(o.pendingTxns, txn)
	}
}

func (o *oracle) GetTxn(startTs uint64) *Txn {
	o.RLock()
	defer o.RUnlock()
	return o.pendingTxns[startTs]
}

func (txn *Txn) matchesDelta(ok func(key []byte) bool) bool {
	txn.Lock()
	defer txn.Unlock()
	for key := range txn.cache.deltas {
		if ok([]byte(key)) {
			return true
		}
	}
	return false
}

// IterateTxns returns a list of start timestamps for currently pending transactions, which match
// the provided function.
func (o *oracle) IterateTxns(ok func(key []byte) bool) []uint64 {
	o.RLock()
	defer o.RUnlock()
	var timestamps []uint64
	for startTs, txn := range o.pendingTxns {
		if txn.matchesDelta(ok) {
			timestamps = append(timestamps, startTs)
		}
	}
	return timestamps
}
