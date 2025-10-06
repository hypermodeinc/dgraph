/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package posting

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"sort"

	"github.com/dgryski/go-farm"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	bpb "github.com/dgraph-io/badger/v4/pb"
	"github.com/dgraph-io/badger/v4/y"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/algo"
	"github.com/hypermodeinc/dgraph/v25/codec"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/tok/index"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/types/facets"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var (
	// ErrRetry can be triggered if the posting list got deleted from memory due to a hard commit.
	// In such a case, retry.
	ErrRetry = errors.New("Temporary error. Please retry")
	// ErrNoValue would be returned if no value was found in the posting list.
	ErrNoValue = errors.New("No value found")
	// ErrStopIteration is returned when an iteration is terminated early.
	ErrStopIteration = errors.New("Stop iteration")
	emptyPosting     = &pb.Posting{}
	maxListSize      = mb / 2
)

const (
	// Set means set in mutation layer. It contributes 1 in Length.
	Set uint32 = 0x01
	// Del means delete in mutation layer. It contributes -1 in Length.
	Del uint32 = 0x02
	// Ovr means overwrite in mutation layer. It contributes 0 in Length.
	Ovr uint32 = 0x03

	// BitSchemaPosting signals that the value stores a schema or type.
	BitSchemaPosting byte = 0x01
	// BitDeltaPosting signals that the value stores the delta of a posting list.
	BitDeltaPosting byte = 0x04
	// BitCompletePosting signals that the values stores a complete posting list.
	BitCompletePosting byte = 0x08
	// BitEmptyPosting signals that the value stores an empty posting list.
	BitEmptyPosting byte = 0x10
)

// List stores the in-memory representation of a posting list.
type List struct {
	x.SafeMutex
	key         []byte
	plist       *pb.PostingList
	mutationMap *MutableLayer
	minTs       uint64 // commit timestamp of immutable layer, reject reads before this ts.
	maxTs       uint64 // max commit timestamp seen for this list.

	cache []byte
}

// MutableLayer is the structure that will store mutable layer of the posting list. Every posting list has an immutable
// layer and a mutable layer. Whenever posting is added into a list, it's added as deltas into the posting list. Once
// this list of deltas keep piling up, they are converted into a complete posting list through rollup and stored as
// immutable layer. Mutable layer contains all the deltas after the last complete posting list.
// Mutable Layer used to be a map from commitTs to PostingList.
// Every transaction that starts, gets its own copy of a posting list that it stores in the local cache of the txn.
// Everytime we make a copy of the postling list, we had to deep clone the map. If we give the same map by reference
// we start seeing concurrent writes and reads into the map causing issues. With this new MutableLayer struct, we
// know that committedEntries will not get changed and this can be copied by reference without any issues.
// This structure, makes it much faster to clone the Mutable Layer and be faster.
type MutableLayer struct {
	committedEntries map[uint64]*pb.PostingList
	currentEntries   *pb.PostingList
	readTs           uint64

	// Since we are storing the committedEntries and currentEntries separately. We can cache things that are
	// going to be used repeatedly.
	deleteAllMarker uint64 // Stores the latest deleteAllMarker found in the posting list
	// including currentEntries.
	committedUids     map[uint64]*pb.Posting // Stores the uid to posting mapping in committedEntries.
	committedUidsTime uint64                 // Stores the latest commitTs in the committedEntries.
	length            int                    // Stores the length of the posting list until committedEntries.
	lastEntry         *pb.PostingList        // Stores the last entry stored in committedUids

	// We also cache some things required for us to update currentEntries faster
	currentUids map[uint64]int // Stores the uid to index mapping in the currentEntries posting list

	// Cache for calculated UIDS
	isUidsCalculated bool
	calculatedUids   []uint64
}

func newMutableLayer() *MutableLayer {
	return &MutableLayer{
		committedEntries:  make(map[uint64]*pb.PostingList),
		readTs:            0,
		deleteAllMarker:   math.MaxUint64,
		length:            math.MaxInt,
		committedUids:     make(map[uint64]*pb.Posting),
		committedUidsTime: math.MaxUint64,
		isUidsCalculated:  false,
		calculatedUids:    []uint64{},
	}
}

func (mm *MutableLayer) setTs(readTs uint64) {
	if mm == nil {
		return
	}
	mm.readTs = readTs
}

// This function clones an existing mutable layer for the new transactions. This function makes sure we copy the right
// things from the existing mutable layer for the new list. It basically copies committedEntries using reference and
// ignores currentEntires and readTs. Similarly, all the cache items related to currentEntries are ignored and
// committedEntries are presevred for the new list.
func (mm *MutableLayer) clone() *MutableLayer {
	if mm == nil {
		return nil
	}
	return &MutableLayer{
		committedEntries:  mm.committedEntries,
		readTs:            0,
		deleteAllMarker:   mm.deleteAllMarker,
		committedUids:     mm.committedUids,
		length:            mm.length,
		lastEntry:         mm.lastEntry,
		committedUidsTime: mm.committedUidsTime,
		isUidsCalculated:  mm.isUidsCalculated,
		calculatedUids:    mm.calculatedUids,
	}
}

// setCurrentEntires() sets the posting in currentEntries. It's used to overwrite the currentEntires. It empties the
// currentUids and sets the readTs.
func (mm *MutableLayer) setCurrentEntries(ts uint64, pl *pb.PostingList) {
	if mm == nil {
		x.AssertTrue(false)
		return
	}
	if mm.readTs != 0 {
		x.AssertTruef(mm.readTs == ts, "List object reused for a different transaction %d %d", mm.readTs, ts)
	}

	mm.readTs = ts
	mm.currentEntries = pl
	clear(mm.currentUids)
	mm.isUidsCalculated = false
	mm.calculatedUids = nil
	mm.deleteAllMarker = math.MaxUint64
	mm.populateUidMap(pl)
}

// get() returns the posting stored in the mutable layer at any given timestamp. If the ts is the same as readTs,
// we will return the currentEntries, otherwise it should be the commitTs of old postings.
func (mm *MutableLayer) get(ts uint64) *pb.PostingList {
	if mm == nil {
		return nil
	}
	if mm.readTs == ts {
		return mm.currentEntries
	}
	return mm.committedEntries[ts]
}

// len() returns the number of entries in the mutable layer. This should only be used to see if there's any data or
// getting the rough size of the layer. This shouldn't be used in places where accurate length is required. For those
// functions use listLen() instead.
func (mm *MutableLayer) len() int {
	if mm == nil {
		return 0
	}
	length := len(mm.committedEntries)
	if mm.currentEntries != nil {
		length += 1
	}
	return length
}

// listLen() returns the length of the mutable layer at the readTs. If the readTs changes, the list len could change.
func (mm *MutableLayer) listLen(readTs uint64) int {
	if mm == nil {
		return 0
	}

	count := 0
	checkPostingForCount := func(pl *pb.PostingList) {
		for _, mpost := range pl.Postings {
			if hasDeleteAll(mpost) {
				// We reach here via either iterating the entire mutable layer, or for just the
				// current entries. For both of them we can only see the latest delete all. If a
				// posting list has a delete all marker, we still need to set count for all the other
				// entries. Hence we need to make sure that count = 0 before we reach here.
				continue
			}
			count += getLengthDelta(mpost.Op)
		}
	}

	// mm.committedUidsTime could be math.MaxUint64 or the actual value. If it's MaxUint64, we know there is no
	// entry in the mm, and we can just do iterate. If value is set and readTs < committedUidsTime, we need to
	// iterate.
	if mm.length == math.MaxInt || readTs < mm.committedUidsTime {
		mm.iterate(func(_ uint64, pl *pb.PostingList) {
			checkPostingForCount(pl)
		}, readTs)
		return count
	}

	count = mm.length

	if mm.currentEntries != nil && (readTs == mm.readTs) {
		if mm.populateDeleteAll(readTs) == mm.readTs {
			// If deleteAll is present, we don't need the count from mm.length.
			count = 0
		}
		checkPostingForCount(mm.currentEntries)
	}
	return count
}

// populateDeleteAll() returns the deleteAllMarker under readTs. It also finds out and sets the global deleteAllMarker
// in hopes to cache it and use it later if required.
func (mm *MutableLayer) populateDeleteAll(readTs uint64) uint64 {
	if mm == nil {
		return 0
	}
	if mm.deleteAllMarker != math.MaxUint64 {
		if readTs >= mm.deleteAllMarker {
			return mm.deleteAllMarker
		}
		// I need to calculate deleteAllMarker again. I can't use the one from cache
	}
	deleteAllMarker := uint64(0)
	deleteAllMarkerBelowTs := uint64(0)
	mm.iterateCommittedEntries(func(ts uint64, pl *pb.PostingList) {
		for _, pl := range pl.Postings {
			if hasDeleteAll(pl) {
				deleteAllMarker = x.Max(deleteAllMarker, ts)
				if ts <= readTs {
					deleteAllMarkerBelowTs = x.Max(deleteAllMarkerBelowTs, ts)
				}
			}
		}
	})

	mm.deleteAllMarker = deleteAllMarker
	return deleteAllMarkerBelowTs
}

// iterateCommittedEntries is an internal function that's used to calculate delete all marker and iterate. No other
// function should use this. They should use .iterate() instead.
func (mm *MutableLayer) iterateCommittedEntries(f func(uint64, *pb.PostingList)) {
	if mm == nil {
		return
	}

	for ts, pl := range mm.committedEntries {
		if pl.CommitTs == ts || ts == mm.readTs {
			f(ts, pl)
		}
	}

	if mm.currentEntries != nil {
		f(mm.readTs, mm.currentEntries)
	}
}

// Before iterating, we have to figure out where the last delete marker is
// Then gather the posts that would be above the marker
func (mm *MutableLayer) iterate(f func(ts uint64, pl *pb.PostingList), readTs uint64) uint64 {
	if mm == nil {
		return 0
	}

	deleteAllMarker := mm.populateDeleteAll(readTs)
	mm.iterateCommittedEntries(func(ts uint64, pl *pb.PostingList) {
		// Note this might not be required, but just here for safety
		if ts >= deleteAllMarker && ts <= readTs {
			f(ts, pl)
		}
	})
	return deleteAllMarker
}

// insertCommittedPostings inserts an old committed posting in the mutable layer. It also updates fields that are
// cached. This includes deleteAllMarker, length and committedUids map. this should be called while
// building the list only.
func (mm *MutableLayer) insertCommittedPostings(pl *pb.PostingList) {
	if mm.committedUidsTime == math.MaxUint64 {
		mm.committedUidsTime = 0
	}
	if mm.length == math.MaxInt64 {
		mm.length = 0
	}
	if mm.deleteAllMarker == math.MaxUint64 {
		mm.deleteAllMarker = 0
	}

	if pl.CommitTs > mm.committedUidsTime {
		mm.lastEntry = pl
	}
	mm.committedUidsTime = x.Max(pl.CommitTs, mm.committedUidsTime)
	mm.committedEntries[pl.CommitTs] = pl

	for _, mpost := range pl.Postings {
		mpost.CommitTs = pl.CommitTs
		if hasDeleteAll(mpost) {
			if mpost.CommitTs > mm.deleteAllMarker {
				mm.deleteAllMarker = mpost.CommitTs
			}
			// No need to set the length here as we are reading the list in reverse.
			continue
		}
		// If this posting is less than deleteAllMarker, we don't need to add it to the mutable map results.
		if mpost.CommitTs >= mm.deleteAllMarker {
			mm.length += getLengthDelta(mpost.Op)
		}
		// We insert old postings in reverse order. So we only need to read the first update to an UID.
		if _, ok := mm.committedUids[mpost.Uid]; !ok {
			mm.committedUids[mpost.Uid] = mpost
		}
	}
}

func (mm *MutableLayer) populateUidMap(pl *pb.PostingList) {
	if mm.currentUids != nil {
		return
	}

	mm.currentUids = make(map[uint64]int, len(pl.Postings))
	for i, post := range pl.Postings {
		mm.currentUids[post.Uid] = i
	}
}

// insertPosting inserts a new posting in the mutable layers. It updates the currentUids map.
func (mm *MutableLayer) insertPosting(mpost *pb.Posting, hasCountIndex bool) {
	if mm.readTs != 0 {
		x.AssertTruef(mpost.StartTs == mm.readTs, "Diffrenent read ts and start ts found %d %d", mpost.StartTs, mm.readTs)
	}

	mm.readTs = mpost.StartTs

	if hasDeleteAll(mpost) {
		if mpost.CommitTs > mm.deleteAllMarker {
			mm.deleteAllMarker = mpost.CommitTs
		}
	}

	if mpost.Uid != 0 {
		// If hasCountIndex, in that case while inserting uids, if there's a delete, we only delete from the
		// current entries, we dont' insert the delete posting. If we insert the delete posting, there won't be
		// any set posting in the list. This would mess up the count. We can do this for all types, however,
		// there might be a performance hit becasue of it.
		mm.populateUidMap(mm.currentEntries)
		if postIndex, ok := mm.currentUids[mpost.Uid]; ok {
			if hasCountIndex && mpost.Op == Del {
				// If the posting was there before, just remove it from the map, and then remove it
				// from the array.
				post := mm.currentEntries.Postings[postIndex]
				if post.Op == Del {
					// No need to do anything
					mm.currentEntries.Postings[postIndex] = mpost
					return
				}
				res := mm.currentEntries.Postings[:postIndex]
				if postIndex+1 <= len(mm.currentEntries.Postings) {
					res = append(res,
						mm.currentEntries.Postings[(postIndex+1):]...)
				}
				mm.currentUids = nil
				mm.currentEntries.Postings = res
				return
			}
			mm.currentEntries.Postings[postIndex] = mpost
		} else {
			mm.currentEntries.Postings = append(mm.currentEntries.Postings, mpost)
			mm.currentUids[mpost.Uid] = len(mm.currentEntries.Postings) - 1
		}
		return
	}

	mm.currentEntries.Postings = append(mm.currentEntries.Postings, mpost)
}

func (mm *MutableLayer) print() string {
	if mm == nil {
		return ""
	}
	return fmt.Sprintf("Committed List: %+v Proposed list: %+v Delete all marker: %d  \n",
		mm.committedEntries,
		mm.currentEntries,
		mm.deleteAllMarker)
}

func (l *List) Print() string {
	return fmt.Sprintf("minTs: %d, plist: %+v, mutationMap: %s", l.minTs, l.plist, l.mutationMap.print())
}

// Return if piterator needs to be searched or not after mutable map and the posting if found.
func (mm *MutableLayer) findPosting(readTs, uid uint64) (bool, *pb.Posting) {
	if mm == nil {
		return true, nil
	}

	deleteAllMarker := mm.populateDeleteAll(readTs)

	// To get the posting from cached values, we need to make sure that it is >= deleteAllMarker
	// If we get it using mm.iterate (in getPosting), we know that we only see postings >= deleteAllMarker
	getPostingFromCachedValues := func() (*pb.Posting, uint64) {
		if readTs == mm.readTs {
			posI, ok := mm.currentUids[uid]
			if ok {
				return mm.currentEntries.Postings[posI], mm.readTs
			}
		}
		posting, ok := mm.committedUids[uid]
		if ok {
			return posting, posting.CommitTs
		}
		return nil, 0
	}

	// Check if readTs >= committedUidTime. It lets us figure out if we can use the cached values or not.
	getPosting := func() *pb.Posting {
		// If the timestamp that we are reading for is ahead of the cache map, we can check the map and return.
		// Otherwise we need to iterate (slow) the entire map, and keep the latest entry per the commitTs.
		if readTs >= mm.committedUidsTime {
			posting, ts := getPostingFromCachedValues()
			if posting == nil || ts < deleteAllMarker {
				return nil
			}
			return posting
		} else {
			var posting *pb.Posting
			var tsFound uint64
			// Since iterate could be out of order, we need to keep a track of the time we saw the posting.
			mm.iterate(func(ts uint64, pl *pb.PostingList) {
				for _, mpost := range pl.Postings {
					if mpost.Uid == uid {
						if posting == nil {
							posting = mpost
							tsFound = ts
						} else if tsFound <= ts {
							posting = mpost
							tsFound = ts
						}
					}
				}
			}, readTs)
			return posting
		}
	}

	posting := getPosting()
	if posting != nil {
		// If we find the posting, either we return it, or it has been deleted. Either ways, we don't search
		// more in the immutable layer.
		if posting.Op != Del {
			return false, posting
		} else {
			return false, nil
		}
	}

	// If delete all is set, immutable layer can't be read. Hence setting searchFurther as false.
	if deleteAllMarker > 0 {
		return false, nil
	}

	// No posting was found, and no delete was there. Hence we can now search in immutable layer.
	return true, nil
}

func indexEdgeToPbEdge(t *index.KeyValue) *pb.DirectedEdge {
	return &pb.DirectedEdge{
		Entity:    t.Entity,
		Attr:      t.Attr,
		Value:     t.Value,
		ValueType: pb.Posting_ValType(0),
		Op:        pb.DirectedEdge_SET,
	}
}

// NewList returns a new list with an immutable layer set to plist and the
// timestamp of the immutable layer set to minTs.
func NewList(key []byte, plist *pb.PostingList, minTs uint64) *List {
	return &List{
		key:         key,
		plist:       plist,
		mutationMap: newMutableLayer(),
		minTs:       minTs,
	}
}

func (l *List) maxVersion() uint64 {
	l.RLock()
	defer l.RUnlock()
	return l.maxTs
}

type pIterator struct {
	l          *List
	plist      *pb.PostingList
	uidPosting *pb.Posting
	pidx       int // index of postings
	plen       int

	dec  *codec.Decoder
	uids []uint64
	uidx int // Offset into the uids slice

	afterUid uint64
	splitIdx int
	// The timestamp of a delete marker in the mutable layer. If this value is greater
	// than zero, then the immutable posting list should not be traversed.
	deleteBelowTs uint64
}

func (it *pIterator) seek(l *List, afterUid, deleteBelowTs uint64) error {
	// Because we store rollup at commitTs + 1, it could happen that a transaction has a startTs = prev commitTs
	// + 1. Within that transcation if there's a delete all, deleteBelowTs (=startT) would be equal to l.minTs
	// (rollup timestamp, prev commitTs + 1). So it's allowed deleteBelowTs == l.minTs
	if deleteBelowTs > 0 && deleteBelowTs < l.minTs {
		return errors.Errorf("deleteBelowTs (%d) must be greater than the minTs in the list (%d)",
			deleteBelowTs, l.minTs)
	}

	it.l = l
	it.splitIdx = it.selectInitialSplit(afterUid)
	if len(it.l.plist.Splits) > 0 {
		plist, err := l.readListPart(it.l.plist.Splits[it.splitIdx])
		if err != nil {
			return errors.Wrapf(err, "cannot read initial list part for list with base key %s",
				hex.EncodeToString(l.key))
		}
		it.plist = plist
	} else {
		it.plist = l.plist
	}

	it.afterUid = afterUid
	it.deleteBelowTs = deleteBelowTs
	if deleteBelowTs > 0 {
		// We don't need to iterate over the immutable layer if this is > 0. Returning here would
		// mean it.uids is empty and valid() would return false.
		return nil
	}

	it.uidPosting = &pb.Posting{}
	it.dec = &codec.Decoder{Pack: it.plist.Pack}
	it.uids = it.dec.Seek(it.afterUid, codec.SeekCurrent)
	it.uidx = 0

	it.plen = len(it.plist.Postings)
	it.pidx = sort.Search(it.plen, func(idx int) bool {
		p := it.plist.Postings[idx]
		return it.afterUid < p.Uid
	})
	return nil
}

func (it *pIterator) selectInitialSplit(afterUid uint64) int {
	if afterUid == 0 {
		return 0
	}

	for i, startUid := range it.l.plist.Splits {
		// If startUid == afterUid, the current block should be selected.
		if startUid == afterUid {
			return i
		}
		// If this split starts at an UID greater than afterUid, there might be
		// elements in the previous split that need to be checked.
		if startUid > afterUid {
			return i - 1
		}
	}

	// In case no split's startUid is greater or equal than afterUid, start the
	// iteration at the start of the last split.
	return len(it.l.plist.Splits) - 1
}

// moveToNextPart re-initializes the iterator at the start of the next list part.
func (it *pIterator) moveToNextPart() error {
	it.splitIdx++
	plist, err := it.l.readListPart(it.l.plist.Splits[it.splitIdx])
	if err != nil {
		return errors.Wrapf(err, "cannot move to next list part in iterator for list with key %s",
			hex.EncodeToString(it.l.key))
	}
	it.plist = plist

	it.uidPosting = &pb.Posting{}
	it.dec = &codec.Decoder{Pack: it.plist.Pack}
	// codec.SeekCurrent makes sure we skip returning afterUid during seek.
	it.uids = it.dec.Seek(it.afterUid, codec.SeekCurrent)
	it.uidx = 0

	it.plen = len(it.plist.Postings)
	it.pidx = sort.Search(it.plen, func(idx int) bool {
		p := it.plist.Postings[idx]
		return it.afterUid < p.Uid
	})

	return nil
}

// moveToNextValidPart moves the iterator to the next part that contains valid data.
// This is used to skip over parts of the list that might not contain postings.
func (it *pIterator) moveToNextValidPart() error {
	// Not a multi-part list, the iterator has reached the end of the list.
	if len(it.l.plist.Splits) == 0 {
		return nil
	}

	// Iterate while there are no UIDs, and while we have more splits to iterate over.
	for len(it.uids) == 0 && it.splitIdx < len(it.l.plist.Splits)-1 {
		// moveToNextPart will increment it.splitIdx. Therefore, the for loop must only
		// continue until len(splits)-1.
		if err := it.moveToNextPart(); err != nil {
			return err
		}
	}

	return nil
}

// next advances pIterator to the next valid part.
func (it *pIterator) next() error {
	if it.deleteBelowTs > 0 {
		it.uids = nil
		return nil
	}

	it.uidx++
	if it.uidx < len(it.uids) {
		return nil
	}
	it.uidx = 0
	it.uids = it.dec.Next()

	return errors.Wrapf(it.moveToNextValidPart(), "cannot advance iterator for list with key %s",
		hex.EncodeToString(it.l.key))
}

// valid asserts that pIterator has valid uids, or advances it to the next valid part.
// It returns false if there are no more valid parts.
func (it *pIterator) valid() (bool, error) {
	if it.deleteBelowTs > 0 {
		it.uids = nil
		return false, nil
	}

	if len(it.uids) > 0 {
		return true, nil
	}

	err := it.moveToNextValidPart()
	switch {
	case err != nil:
		return false, errors.Wrapf(err, "cannot advance iterator when calling pIterator.valid")
	case len(it.uids) > 0:
		return true, nil
	default:
		return false, nil
	}
}

func (it *pIterator) posting() *pb.Posting {
	uid := it.uids[it.uidx]

	for it.pidx < it.plen {
		p := it.plist.Postings[it.pidx]
		if p.Uid > uid {
			break
		}
		if p.Uid == uid {
			return p
		}
		it.pidx++
	}
	it.uidPosting.Uid = uid
	it.uidPosting.ValType = pb.Posting_UID
	return it.uidPosting
}

// ListOptions is used in List.Uids (in posting) to customize our output list of
// UIDs, for each posting list. It should be pb.to this package.
type ListOptions struct {
	ReadTs    uint64
	AfterUid  uint64   // Any UIDs returned must be after this value.
	Intersect *pb.List // Intersect results with this list of UIDs.
	First     int
}

func NewVectorPosting(uid uint64, vec *[]byte) *pb.Posting {
	return &pb.Posting{
		Value:   *vec,
		ValType: pb.Posting_BINARY,
		Op:      Set,
	}
}

// NewPosting takes the given edge and returns its equivalent representation as a posting.
func NewPosting(t *pb.DirectedEdge) *pb.Posting {
	var op uint32
	switch t.Op {
	case pb.DirectedEdge_SET:
		op = Set
	case pb.DirectedEdge_OVR:
		op = Ovr
	case pb.DirectedEdge_DEL:
		op = Del
	default:
		x.Fatalf("Unhandled operation: %+v", t)
	}

	var postingType pb.Posting_PostingType
	switch {
	case len(t.Lang) > 0:
		postingType = pb.Posting_VALUE_LANG
	case t.ValueId == 0:
		postingType = pb.Posting_VALUE
	default:
		postingType = pb.Posting_REF
	}

	p := &pb.Posting{
		Uid:         t.ValueId,
		Value:       t.Value,
		ValType:     t.ValueType,
		PostingType: postingType,
		LangTag:     []byte(t.Lang),
		Op:          op,
		Facets:      t.Facets,
	}
	return p
}

func createDeleteAllPosting() *pb.Posting {
	return &pb.Posting{
		Op:    Del,
		Value: []byte(x.Star),
		Uid:   math.MaxUint64,
	}
}

func hasDeleteAll(mpost *pb.Posting) bool {
	return mpost.Op == Del && bytes.Equal(mpost.Value, []byte(x.Star)) && len(mpost.LangTag) == 0
}

// Ensure that you either abort the uncommitted postings or commit them before calling me.
func (l *List) updateMutationLayer(mpost *pb.Posting, singleUidUpdate, hasCountIndex bool) error {
	l.AssertLock()
	x.AssertTrue(mpost.Op == Set || mpost.Op == Del || mpost.Op == Ovr)

	if l.mutationMap == nil {
		l.mutationMap = newMutableLayer()
	}

	// If we have a delete all, then we replace the map entry with just one.
	if hasDeleteAll(mpost) {
		plist := &pb.PostingList{}
		plist.Postings = append(plist.Postings, mpost)
		l.mutationMap.setCurrentEntries(mpost.StartTs, plist)
		return nil
	}

	if l.mutationMap.currentEntries == nil {
		l.mutationMap.currentEntries = &pb.PostingList{}
	}

	l.mutationMap.isUidsCalculated = false
	l.mutationMap.calculatedUids = nil

	if singleUidUpdate {
		// This handles the special case when adding a value to predicates of type uid.
		// The current value should be deleted in favor of this value. This needs to
		// be done because the fingerprint for the value is not math.MaxUint64 as is
		// the case with the rest of the scalar predicates.
		newPlist := &pb.PostingList{}
		if mpost.Op != Del {
			// If we are setting a new value then we can just delete all the older values.
			newPlist.Postings = append(newPlist.Postings, createDeleteAllPosting())
		}
		newPlist.Postings = append(newPlist.Postings, mpost)
		l.mutationMap.setCurrentEntries(mpost.StartTs, newPlist)
		return nil
	}

	l.mutationMap.insertPosting(mpost, hasCountIndex)
	return nil
}

// TypeID returns the typeid of destination vertex
func TypeID(edge *pb.DirectedEdge) types.TypeID {
	if edge.ValueId != 0 {
		return types.UidID
	}
	return types.TypeID(edge.ValueType)
}

func fingerprintEdge(t *pb.DirectedEdge) uint64 {
	// There could be a collision if the user gives us a value with Lang = "en" and later gives
	// us a value = "en" for the same predicate. We would end up overwriting his older lang
	// value.

	// All edges with a value without LANGTAG, have the same UID. In other words,
	// an (entity, attribute) can only have one untagged value.
	var id uint64 = math.MaxUint64

	// Value with a lang type.
	switch {
	case len(t.Lang) > 0:
		id = farm.Fingerprint64([]byte(t.Lang))
	case schema.State().IsList(t.Attr):
		// TODO - When values are deleted for list type, then we should only delete the UID from
		// index if no other values produces that index token.
		// Value for list type.
		id = farm.Fingerprint64(t.Value)
	}
	return id
}

func (l *List) addMutation(ctx context.Context, txn *Txn, t *pb.DirectedEdge) error {
	l.Lock()
	defer l.Unlock()
	return l.addMutationInternal(ctx, txn, t)
}

func GetConflictKey(pk x.ParsedKey, key []byte, t *pb.DirectedEdge) uint64 {
	getKey := func(key []byte, uid uint64) uint64 {
		// Instead of creating a string first and then doing a fingerprint, let's do a fingerprint
		// here to save memory allocations.
		// Not entirely sure about effect on collision chances due to this simple XOR with uid.
		return farm.Fingerprint64(key) ^ uid
	}

	var conflictKey uint64
	switch {
	case schema.State().HasNoConflict(t.Attr):
		break
	case schema.State().HasUpsert(t.Attr):
		// Consider checking to see if a email id is unique. A user adds:
		// <uid> <email> "email@email.org", and there's a string equal tokenizer
		// and upsert directive on the schema.
		// Then keys are "<email> <uid>" and "<email> email@email.org"
		// The first key won't conflict, because two different UIDs can try to
		// get the same email id. But, the second key would. Thus, we ensure
		// that two users don't set the same email id.
		conflictKey = getKey(key, 0)

	case pk.IsData() && schema.State().IsList(t.Attr):
		// Data keys, irrespective of whether they are UID or values, should be judged based on
		// whether they are lists or not. For UID, t.ValueId = UID. For value, t.ValueId =
		// fingerprint(value) or could be fingerprint(lang) or something else.
		//
		// For singular uid predicate, like partner: uid // no list.
		// a -> b
		// a -> c
		// Run concurrently, only one of them should succeed.
		// But for friend: [uid], both should succeed.
		//
		// Similarly, name: string
		// a -> "x"
		// a -> "y"
		// This should definitely have a conflict.
		// But, if name: [string], then they can both succeed.
		conflictKey = getKey(key, t.ValueId)

	case pk.IsData(): // NOT a list. This case must happen after the above case.
		conflictKey = getKey(key, 0)

	case pk.IsIndex() || pk.IsCountOrCountRev():
		// Index keys are by default of type [uid].
		conflictKey = getKey(key, t.ValueId)

	default:
		// Don't assign a conflictKey.
	}

	return conflictKey
}

// SetTs allows us to set the transaction timestamp in mutation map. Should be used before the posting list is passed
// on to the functions that would read the data.
func (l *List) SetTs(readTs uint64) {
	l.mutationMap.setTs(readTs)
}

func (l *List) addMutationInternal(ctx context.Context, txn *Txn, t *pb.DirectedEdge) error {
	l.cache = nil
	l.AssertLock()

	if txn.ShouldAbort() {
		return x.ErrConflict
	}

	mpost := NewPosting(t)
	mpost.StartTs = txn.StartTs
	if mpost.PostingType != pb.Posting_REF {
		t.ValueId = fingerprintEdge(t)
		mpost.Uid = t.ValueId
	}

	// Check whether this mutation is an update for a predicate of type uid.
	pk, err := x.Parse(l.key)
	if err != nil {
		return errors.Wrapf(err, "cannot parse key when adding mutation to list with key %s",
			hex.EncodeToString(l.key))
	}
	pred, ok := schema.State().Get(ctx, t.Attr)
	isSingleUidUpdate := ok && !pred.GetList() && pred.GetValueType() == pb.Posting_UID &&
		pk.IsData() && mpost.Op != Del && mpost.PostingType == pb.Posting_REF

	if err != l.updateMutationLayer(mpost, isSingleUidUpdate, pred.GetCount() && (pk.IsData() || pk.IsReverse())) {
		return errors.Wrapf(err, "cannot update mutation layer of key %s with value %+v",
			hex.EncodeToString(l.key), mpost)
	}

	x.PrintMutationEdge(t, pk, txn.StartTs)

	// We ensure that commit marks are applied to posting lists in the right
	// order. We can do so by proposing them in the same order as received by the Oracle delta
	// stream from Zero, instead of in goroutines.
	txn.addConflictKey(GetConflictKey(pk, l.key, t))
	return nil
}

// getMutation returns a marshaled version of posting list mutation stored internally.
func (l *List) getPosting(startTs uint64) *pb.PostingList {
	l.RLock()
	defer l.RUnlock()
	return l.mutationMap.get(startTs)
}

func (l *List) GetPosting(startTs uint64) *pb.PostingList {
	return l.getPosting(startTs)
}

// getMutation returns a marshaled version of posting list mutation stored internally.
func (l *List) getMutation(startTs uint64) []byte {
	l.RLock()
	defer l.RUnlock()
	if pl := l.mutationMap.get(startTs); pl != nil {
		data, err := proto.Marshal(pl)
		x.Check(err)
		return data
	}
	return nil
}

func getLengthDelta(op uint32) int {
	if op == Del {
		return -1
	} else if op == Set {
		return 1
	}
	return 0
}

// Here we update the mutableLayer as required. If the refresh is set to true, we make a new map for committedEntries
// as it could be shared by multiple different lists. Then we update the mutableLayer to commit the data we recieved.
// We also empty out the current stuff. (currentUids, currentEntries and readTs)
func (l *List) setMutationAfterCommit(startTs, commitTs uint64, pl *pb.PostingList, refresh bool) {
	pl.CommitTs = commitTs
	for _, p := range pl.Postings {
		p.CommitTs = commitTs
	}

	x.AssertTrue(pl.Pack == nil)

	l.Lock()
	defer l.Unlock()
	if l.mutationMap == nil {
		l.mutationMap = newMutableLayer()
	}

	if refresh {
		newMap := make(map[uint64]*pb.PostingList, l.mutationMap.len())
		for k, v := range l.mutationMap.committedEntries {
			newMap[k] = proto.Clone(v).(*pb.PostingList)
		}
		newMap[commitTs] = pl
		l.mutationMap.committedEntries = newMap
	} else {
		l.mutationMap.committedEntries[commitTs] = pl

	}

	if l.mutationMap.committedUidsTime == math.MaxUint64 {
		l.mutationMap.committedUidsTime = 0
	}
	if pl.CommitTs > l.mutationMap.committedUidsTime {
		l.mutationMap.lastEntry = pl
	}
	l.mutationMap.committedUidsTime = x.Max(l.mutationMap.committedUidsTime, commitTs)

	if refresh {
		newMap := make(map[uint64]*pb.Posting, len(l.mutationMap.committedUids))
		for uid, post := range l.mutationMap.committedUids {
			newMap[uid] = post
		}
		l.mutationMap.committedUids = newMap
	}

	for _, mpost := range pl.Postings {
		if hasDeleteAll(mpost) {
			l.mutationMap.deleteAllMarker = commitTs
			l.mutationMap.length = 0
			continue
		}

		l.mutationMap.committedUids[mpost.Uid] = mpost
		if l.mutationMap.length == math.MaxInt64 {
			l.mutationMap.length = 0
		}
		l.mutationMap.length += getLengthDelta(mpost.Op)
	}

	l.mutationMap.currentEntries = nil
	l.mutationMap.readTs = 0
	l.mutationMap.currentUids = nil
	l.mutationMap.isUidsCalculated = false
	l.mutationMap.calculatedUids = nil

	if pl.CommitTs != 0 {
		l.maxTs = x.Max(l.maxTs, pl.CommitTs)
	}
}

func (l *List) setMutation(startTs uint64, data []byte) {
	pl := new(pb.PostingList)
	x.Check(proto.Unmarshal(data, pl))

	l.Lock()
	if l.mutationMap == nil {
		l.mutationMap = newMutableLayer()
	}
	l.mutationMap.setCurrentEntries(startTs, pl)
	if pl.CommitTs != 0 {
		l.maxTs = x.Max(l.maxTs, pl.CommitTs)
	}
	l.Unlock()
}

// Iterate will allow you to iterate over the mutable and immutable layers of
// this posting List, while having acquired a read lock.
// So, please keep this iteration cheap, otherwise mutations would get stuck.
// The iteration will start after the provided UID. The results would not include this uid.
// The function will loop until either the posting List is fully iterated, or you return a false
// in the provided function, which will indicate to the function to break out of the iteration.
//
//		pl.Iterate(..., func(p *pb.posting) error {
//	   // Use posting p
//	   return nil // to continue iteration.
//	   return errStopIteration // to break iteration.
//	 })
func (l *List) Iterate(readTs uint64, afterUid uint64, f func(obj *pb.Posting) error) error {
	l.RLock()
	defer l.RUnlock()
	return l.iterate(readTs, afterUid, f)
}

// pickPostings goes through the mutable layer and returns the appropriate postings,
// along with the timestamp of the delete marker, if any. If this timestamp is greater
// than zero, it indicates that the immutable layer should be ignored during traversals.
// If greater than zero, this timestamp must thus be greater than l.minTs.
func (l *List) pickPostings(readTs uint64) (uint64, []*pb.Posting) {
	// This function would return zero ts for entries above readTs.
	effective := func(start, commit uint64) uint64 {
		if commit > 0 && commit <= readTs {
			// Has been committed and below the readTs.
			return commit
		}
		if start == readTs {
			// This mutation is by ME. So, I must be able to read it.
			return start
		}
		return 0
	}

	// First pick up the postings.
	var posts []*pb.Posting
	deleteAllMarker := l.mutationMap.iterate(func(ts uint64, plist *pb.PostingList) {
		// ts will be plist.CommitTs for committed transactions
		// ts will be readTs for mutations that are me
		posts = append(posts, plist.Postings...)
	}, readTs)

	// Sort all the postings by UID (inc order), then by commit/startTs in dec order.
	sort.Slice(posts, func(i, j int) bool {
		pi := posts[i]
		pj := posts[j]
		if pi.Uid == pj.Uid {
			ei := effective(pi.StartTs, pi.CommitTs)
			ej := effective(pj.StartTs, pj.CommitTs)
			return ei > ej // Pick the higher, so we can discard older commits for the same UID.
		}
		return pi.Uid < pj.Uid
	})
	return deleteAllMarker, posts
}

func (l *List) iterate(readTs uint64, afterUid uint64, f func(obj *pb.Posting) error) error {
	l.AssertRLock()

	// mposts is the list of mutable postings
	deleteBelowTs, mposts := l.pickPostings(readTs)
	if readTs < l.minTs {
		return errors.Errorf("readTs: %d less than minTs: %d for key: %q", readTs, l.minTs, l.key)
	}

	midx, mlen := 0, len(mposts)
	if afterUid > 0 {
		midx = sort.Search(mlen, func(idx int) bool {
			mp := mposts[idx]
			return afterUid < mp.Uid
		})
	}

	numDeletePostingsRead := 0
	numNormalPostingsRead := 0
	defer func() {
		// If we see a lot of these logs, it means that a lot of elements are getting deleted.
		// This could be normal, but if we see this too much, that means that rollups are too slow.
		if numNormalPostingsRead < numDeletePostingsRead &&
			(numNormalPostingsRead > 0 || numDeletePostingsRead > 0) {
			glog.V(3).Infof("High proportion of deleted data observed for posting list %b: total = %d, "+
				"percent deleted = %d", l.key, numNormalPostingsRead+numDeletePostingsRead,
				(numDeletePostingsRead*100)/(numDeletePostingsRead+numNormalPostingsRead))
		}
	}()

	var (
		mp, pp  *pb.Posting
		pitr    pIterator
		prevUid uint64
		err     error
	)

	// pitr iterates through immutable postings
	err = pitr.seek(l, afterUid, deleteBelowTs)
	if err != nil {
		return errors.Wrapf(err, "cannot initialize iterator when calling List.iterate %v", l.Print())
	}

loop:
	for err == nil {
		if midx < mlen {
			mp = mposts[midx]
		} else {
			mp = emptyPosting
		}

		valid, err := pitr.valid()
		switch {
		case err != nil:
			break loop
		case valid:
			pp = pitr.posting()
		default:
			pp = emptyPosting
		}

		switch {
		case mp.Uid > 0 && mp.Uid == prevUid:
			// Only pick the latest version of this posting.
			// mp.Uid can be zero if it's an empty posting.
			midx++
		case pp.Uid == 0 && mp.Uid == 0:
			// Reached empty posting for both iterators.
			return nil
		case mp.Uid == 0 || (pp.Uid > 0 && pp.Uid < mp.Uid):
			// Either mp is empty, or pp is lower than mp.
			err = f(pp)
			numNormalPostingsRead += 1
			if err != nil {
				break loop
			}

			if err = pitr.next(); err != nil {
				break loop
			}
		case pp.Uid == 0 || (mp.Uid > 0 && mp.Uid < pp.Uid):
			// Either pp is empty, or mp is lower than pp.
			if mp.Op != Del {
				err = f(mp)
				numNormalPostingsRead += 1
				if err != nil {
					break loop
				}
			} else {
				numDeletePostingsRead += 1
			}
			prevUid = mp.Uid
			midx++
		case pp.Uid == mp.Uid:
			if mp.Op != Del {
				err = f(mp)
				numNormalPostingsRead += 1
				if err != nil {
					break loop
				}
			} else {
				numDeletePostingsRead += 1
			}
			prevUid = mp.Uid
			if err = pitr.next(); err != nil {
				break loop
			}
			midx++
		default:
			log.Fatalf("Unhandled case during iteration of posting list.")
		}
	}
	if err == ErrStopIteration {
		return nil
	}
	return err
}

// IsEmpty returns true if there are no uids at the given timestamp after the given UID.
func (l *List) IsEmpty(readTs, afterUid uint64) (bool, error) {
	l.RLock()
	defer l.RUnlock()
	var count int
	err := l.iterate(readTs, afterUid, func(p *pb.Posting) error {
		count++
		return ErrStopIteration
	})
	if err != nil {
		return false, errors.Wrapf(err, "cannot iterate over list when calling List.IsEmpty")
	}
	return count == 0, nil
}

func (l *List) GetLength(readTs uint64) int {
	l.AssertRLock()

	length := l.mutationMap.listLen(readTs)
	if l.mutationMap.populateDeleteAll(readTs) == 0 {
		immutLen, err := l.immutableLayerLen()
		if err != nil {
			panic(err)
		}
		length += immutLen
	}

	return length
}

// This function gets the posting and length without iterating over it
func (l *List) getPostingAndLengthNoSort(readTs, afterUid, uid uint64) (int, bool, *pb.Posting) {
	l.AssertRLock()

	// We can't call GetLength() while indexing is going on.
	// TODO check if indexing is in progress only for the predicate of this list.
	if schema.State().IndexingInProgress() {
		return l.getPostingAndLength(readTs, afterUid, uid)
	}

	length := l.GetLength(readTs)

	found, pos, err := l.findPosting(readTs, uid)
	if err != nil {
		panic(err)
	}

	return length, found, pos
}

func (l *List) immutableLayerLen() (int, error) {
	l.AssertRLock()

	if len(l.plist.Splits) > 0 {
		length := 0
		for _, split := range l.plist.Splits {
			plist, err := l.readListPart(split)
			if err != nil {
				return 0, errors.Wrapf(err,
					"cannot read initial list part for list with base key %s",
					hex.EncodeToString(l.key))
			}
			length += codec.ExactLen(plist.Pack)
		}

		return length, nil
	}

	return codec.ExactLen(l.plist.Pack), nil
}

func (l *List) getPostingAndLength(readTs, afterUid, uid uint64) (int, bool, *pb.Posting) {
	l.AssertRLock()
	var count int
	var found bool
	var post *pb.Posting

	err := l.iterate(readTs, afterUid, func(p *pb.Posting) error {
		if p.Uid == uid {
			post = p
			found = true
		}
		count++
		return nil
	})
	if err != nil {
		return -1, false, nil
	}

	return count, found, post
}

func (l *List) length(readTs, afterUid uint64) int {
	l.AssertRLock()
	count := 0
	err := l.iterate(readTs, afterUid, func(p *pb.Posting) error {
		count++
		return nil
	})
	if err != nil {
		return -1
	}
	return count
}

// Length iterates over the mutation layer and counts number of elements.
func (l *List) Length(readTs, afterUid uint64) int {
	l.RLock()
	defer l.RUnlock()
	return l.length(readTs, afterUid)
}

// Rollup performs the rollup process, merging the immutable and mutable layers
// and outputting the resulting list so it can be written to disk.
// During this process, the list might be split into multiple lists if the main
// list or any of the existing parts become too big.
//
// A normal list has the following format:
// <key> -> <posting list with all the data for this list>
//
// A multi-part list is stored in multiple keys. The keys for the parts will be generated by
// appending the first UID in the part to the key. The list will have the following format:
// <key> -> <posting list that includes no postings but a list of each part's start UID>
// <key, 1> -> <first part of the list with all the data for this part>
// <key, next start UID> -> <second part of the list with all the data for this part>
// ...
// <key, last start UID> -> <last part of the list with all its data>
//
// The first part of a multi-part list always has start UID 1 and will be the last part
// to be deleted, at which point the entire list will be marked for deletion.
// As the list grows, existing parts might be split if they become too big.
//
// You can provide a readTs for Rollup. This should either be math.MaxUint64, or it should be a
// timestamp that was resevered for rollup. This would ensure that we read only till that time.
// If read ts is provided, Once the rollup is done, we check the maximum timestamp. We store the
// results at that max timestamp + 1. This mechanism allows us to make sure that
//
//   - Since we write at max timestamp + 1, we can side step any issues that arise by wal replay.
//
//   - Earlier one of the solution was to write at ts + 1. It didn't work as index transactions
//     don't conflict so they can get committed at consecutive timestamps.
//     This leads to some data being overwriten by rollup.
//
//   - No other transcation happens at readTs. This way we can be sure that we won't overwrite
//     any transaction that happened.
//
//   - Latest data. We wait until readTs - 1, so that we know that we are reading the latest data.
//     If we read stale data, it can cause to delete some old transactions.
//
//   - Even though we have reserved readTs for rollup, we don't store the data there. This is done
//     so that the rollup is written as close as possible to actual data. This can cause issues
//     if someone is reading data between two timestamps.
//
//   - Drop operation can cause issues if they are rolled up. Since we are storing results at ts + 1,
//     in dgraph.drop.op. When we do drop op, we delete the relevant data first using a mutation.
//     Then we write a record into the dgraph.drop.op. We use this record to figure out if a
//     drop was performed. This helps us during backup, when we want to know if we need to
//     read a given backup or not. A backup, which has a drop record, would render older backups
//     unnecessary.
//
//     If we rollup the dgraph.drop.op, and store result on ts + 1, it effectively copies the
//     original record into a new location. We want to see if there can be any issues in
//     backup/restore due to this. To ensure that there is no issue in writing on ts + 1,
//     we do the following analysis.
//
//     Analysis is done for drop op, but it would be the same for drop predicate and namespace.
//     Assume that there were two backups, at b1 and b2. We move rollup ts around to see if it
//     can cause any issues. There can be 3 cases:
//
//     1. b1 < ts < b2. In this case, we would have a drop record in b2. This is the same behaviour
//     as we would have writen on ts.
//
//     2. b1 = ts < b2. In this case, we would have a drop record in b1, and in b2. Originally, only
//     b1 would have a drop record. With this new approach, b2 would also have a drop record. This
//     is okay because last entry in b1 is drop, so it wouldn't have any data to be applied.
//
//     3. b1 < ts < ts + 1 = b2. In this case, we would have both drop drop records in b2. No issues
//     in this case.
//
//     This proves that writing rollups at ts + 1 would not cause any issues with dgraph.drop.op.
//     The only issue would come if a rollup happens at ts + k. If a backup happens in between
//     ts and ts + k, it could lead to some data being dropped during restore.
func (l *List) Rollup(alloc *z.Allocator, readTs uint64) ([]*bpb.KV, error) {
	l.RLock()
	defer l.RUnlock()
	out, err := l.rollup(readTs, true)
	if err != nil {
		return nil, errors.Wrapf(err, "failed when calling List.rollup")
	}
	if out == nil {
		return nil, nil
	}
	defer out.free()

	var kvs []*bpb.KV
	kv := MarshalPostingList(out.plist, alloc)
	kv.Version = out.newMinTs
	if readTs != math.MaxUint64 {
		kv.Version += 1
	}

	kv.Key = alloc.Copy(l.key)
	kvs = append(kvs, kv)

	for startUid, plist := range out.parts {
		// Any empty posting list would still have BitEmpty set. And the main posting list
		// would NOT have that posting list startUid in the splits list.
		kv, err := out.marshalPostingListPart(alloc, l.key, startUid, plist)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot marshaling posting list parts")
		}
		kvs = append(kvs, kv)
	}

	// Sort the KVs by their key so that the main part of the list is at the
	// start of the list and all other parts appear in the order of their start UID.
	sort.Slice(kvs, func(i, j int) bool {
		return bytes.Compare(kvs[i].Key, kvs[j].Key) <= 0
	})

	x.PrintRollup(out.plist, out.parts, l.key, kv.Version)
	x.VerifyPostingSplits(kvs, out.plist, out.parts, l.key)
	return kvs, nil
}

// ToBackupPostingList uses rollup to generate a single list with no splits.
// It's used during backup so that each backed up posting list is stored in a single key.
func (l *List) ToBackupPostingList(
	bl *pb.BackupPostingList, alloc *z.Allocator, buf *z.Buffer) (*bpb.KV, error) {

	bl.Reset()
	l.RLock()
	defer l.RUnlock()

	out, err := l.rollup(math.MaxUint64, false)
	if err != nil {
		return nil, errors.Wrapf(err, "failed when calling List.rollup")
	}
	// out is only nil when the list's minTs is greater than readTs but readTs
	// is math.MaxUint64 so that's not possible. Assert that's true.
	x.AssertTrue(out != nil)
	defer out.free()

	ol := out.plist

	// Encode uids to []byte instead of []uint64. This helps improve memory usage.
	buf.Reset()
	codec.DecodeToBuffer(buf, ol.Pack)
	bl.UidBytes = buf.Bytes()
	bl.Postings = ol.Postings
	bl.CommitTs = ol.CommitTs
	bl.Splits = ol.Splits

	val, err := x.MarshalToSizedBuffer(alloc.Allocate(proto.Size(bl)), bl)
	if err != nil {
		return nil, err
	}

	kv := y.NewKV(alloc)
	kv.Key = alloc.Copy(l.key)
	kv.Version = out.newMinTs
	kv.Value = val
	if isPlistEmpty(ol) {
		kv.UserMeta = alloc.Copy([]byte{BitEmptyPosting})
	} else {
		kv.UserMeta = alloc.Copy([]byte{BitCompletePosting})
	}
	return kv, nil
}

func (out *rollupOutput) marshalPostingListPart(alloc *z.Allocator,
	baseKey []byte, startUid uint64, plist *pb.PostingList) (*bpb.KV, error) {
	key, err := x.SplitKey(baseKey, startUid)
	if err != nil {
		return nil, errors.Wrapf(err,
			"cannot generate split key for list with base key %s and start UID %d",
			hex.EncodeToString(baseKey), startUid)
	}
	kv := MarshalPostingList(plist, alloc)
	kv.Version = out.newMinTs
	kv.Key = alloc.Copy(key)
	return kv, nil
}

// MarshalPostingList returns a KV with the marshalled posting list. The caller
// SHOULD SET the Key and Version for the returned KV.
func MarshalPostingList(plist *pb.PostingList, alloc *z.Allocator) *bpb.KV {
	x.VerifyPack(plist)
	kv := y.NewKV(alloc)
	if isPlistEmpty(plist) {
		kv.Value = nil
		kv.UserMeta = alloc.Copy([]byte{BitEmptyPosting})
		return kv
	}
	ref := plist.Pack.GetAllocRef()
	if plist.Pack != nil {
		// Set allocator to zero for marshal.
		plist.Pack.AllocRef = 0
	}

	out, err := x.MarshalToSizedBuffer(alloc.Allocate(proto.Size(plist)), plist)
	x.Check(err)
	if plist.Pack != nil {
		plist.Pack.AllocRef = ref
	}
	kv.Value = out
	kv.UserMeta = alloc.Copy([]byte{BitCompletePosting})
	return kv
}

const blockSize int = 256

type rollupOutput struct {
	plist    *pb.PostingList
	parts    map[uint64]*pb.PostingList
	newMinTs uint64
}

func (out *rollupOutput) free() {
	codec.FreePack(out.plist.Pack)
	for _, part := range out.parts {
		codec.FreePack(part.Pack)
	}
}

/*
// sanityCheck can be kept around for debugging, and can be called when deallocating Pack.
func sanityCheck(prefix string, out *rollupOutput) {
	seen := make(map[string]string)

	hb := func(which string, pack *pb.UidPack, block *pb.UidBlock) {
		paddr := fmt.Sprintf("%p", pack)
		baddr := fmt.Sprintf("%p", block)
		if pa, has := seen[baddr]; has {
			glog.Fatalf("[%s %s] Have already seen this block: %s in pa:%s. "+
				"Now found in pa: %s (num blocks: %d) as well. Block [base: %d. Len: %d] Full map size: %d",
				prefix, which, baddr, pa, paddr, len(pack.Blocks), block.Base, len(block.Deltas), len(seen))
		}
		seen[baddr] = which + "_" + paddr
	}

	if out.plist.Pack != nil {
		for _, block := range out.plist.Pack.Blocks {
			hb("main", out.plist.Pack, block)
		}
	}
	for startUid, part := range out.parts {
		if part.Pack != nil {
			for _, block := range part.Pack.Blocks {
				hb("part_"+strconv.Itoa(int(startUid)), part.Pack, block)
			}
		}
	}
}
*/

func (l *List) encode(out *rollupOutput, readTs uint64, split bool) error {
	var plist *pb.PostingList
	var startUid, endUid uint64
	var splitIdx int
	enc := codec.Encoder{BlockSize: blockSize}

	// Method to properly initialize the variables above
	// when a multi-part list boundary is crossed.
	initializeSplit := func() {
		enc = codec.Encoder{BlockSize: blockSize}

		// Load the corresponding part and set endUid to correctly detect the end of the list.
		startUid = l.plist.Splits[splitIdx]
		if splitIdx+1 == len(l.plist.Splits) {
			endUid = math.MaxUint64
		} else {
			endUid = l.plist.Splits[splitIdx+1] - 1
		}

		plist = &pb.PostingList{}
	}

	// If not a multi-part list, all UIDs go to the same encoder.
	if len(l.plist.Splits) == 0 || !split {
		plist = out.plist
		endUid = math.MaxUint64
	} else {
		initializeSplit()
	}

	err := l.iterate(readTs, 0, func(p *pb.Posting) error {
		if p.Uid > endUid && split {
			plist.Pack = enc.Done()
			out.parts[startUid] = plist

			splitIdx++
			initializeSplit()
		}

		enc.Add(p.Uid)
		if p.Facets != nil || p.PostingType != pb.Posting_REF {
			plist.Postings = append(plist.Postings, p)
		}
		return nil
	})
	// Finish  writing the last part of the list (or the whole list if not a multi-part list).
	if err != nil {
		return errors.Wrapf(err, "cannot iterate through the list")
	}
	plist.Pack = enc.Done()
	if plist.Pack != nil {
		if plist.Pack.BlockSize != uint32(blockSize) {
			return errors.Errorf("actual block size %d is different from expected value %d",
				plist.Pack.BlockSize, blockSize)
		}
	}
	if split && len(l.plist.Splits) > 0 {
		out.parts[startUid] = plist
	}
	return nil
}

// Merge all entries in mutation layer with commitTs <= l.commitTs into
// immutable layer. Note that readTs can be math.MaxUint64, so do NOT use it
// directly. It should only serve as the read timestamp for iteration.
func (l *List) rollup(readTs uint64, split bool) (*rollupOutput, error) {
	l.AssertRLock()

	// Pick all committed entries
	if l.minTs > readTs {
		// If we are already past the readTs, then skip the rollup.
		return nil, nil
	}

	out := &rollupOutput{
		plist: &pb.PostingList{
			Splits: l.plist.Splits,
		},
		parts: make(map[uint64]*pb.PostingList),
	}

	if len(out.plist.Splits) > 0 || l.mutationMap.len() > 0 {
		// In case there were splits, this would read all the splits from
		// Badger.
		if err := l.encode(out, readTs, split); err != nil {
			return nil, errors.Wrapf(err, "while encoding")
		}
	} else {
		// We already have a nicely packed posting list. Just use it.
		x.VerifyPack(l.plist)
		out.plist = l.plist
	}

	maxCommitTs := l.minTs
	{
		// We can't rely upon iterate to give us the max commit timestamp, because it can skip over
		// postings which had deletions to provide a sorted view of the list. Therefore, the safest
		// way to get the max commit timestamp is to pick all the relevant postings for the given
		// readTs and calculate the maxCommitTs.
		// If deleteBelowTs is greater than zero, there was a delete all marker. The list of
		// postings has been trimmed down.
		deleteBelowTs, mposts := l.pickPostings(readTs)
		maxCommitTs = x.Max(maxCommitTs, deleteBelowTs)
		for _, mp := range mposts {
			maxCommitTs = x.Max(maxCommitTs, mp.CommitTs)
		}
	}

	out.newMinTs = maxCommitTs
	if split {
		// Check if the list (or any of it's parts if it's been previously split) have
		// become too big. Split the list if that is the case.
		out.recursiveSplit()
		out.removeEmptySplits()
	} else {
		out.plist.Splits = nil
	}

	return out, nil
}

// ApproxLen returns an approximate count of the UIDs in the posting list.
func (l *List) ApproxLen() int {
	l.RLock()
	defer l.RUnlock()
	return l.mutationMap.len() + codec.ApproxLen(l.plist.Pack)
}

func (l *List) calculateUids() error {
	l.RLock()
	if l.mutationMap == nil || l.mutationMap.isUidsCalculated {
		l.RUnlock()
		return nil
	}
	res := make([]uint64, 0, l.ApproxLen())

	err := l.iterate(l.mutationMap.committedUidsTime, 0, func(p *pb.Posting) error {
		if p.PostingType == pb.Posting_REF {
			res = append(res, p.Uid)
		}
		return nil
	})

	l.RUnlock()

	if err != nil {
		return err
	}

	l.Lock()
	defer l.Unlock()

	l.mutationMap.calculatedUids = res
	l.mutationMap.isUidsCalculated = true

	return nil
}

func (l *List) canUseCalculatedUids() bool {
	if l.mutationMap == nil {
		return false
	}
	return l.mutationMap.isUidsCalculated && l.mutationMap.currentEntries == nil
}

// Uids returns the UIDs given some query params.
// We have to apply the filtering before applying (offset, count).
// WARNING: Calling this function just to get UIDs is expensive
func (l *List) Uids(opt ListOptions) (*pb.List, error) {
	if opt.First == 0 {
		opt.First = math.MaxInt32
	}

	getUidList := func() (*pb.List, error, bool) {
		if l.canUseCalculatedUids() {
			l.RLock()

			afterIdx := 0

			if opt.AfterUid != 0 {
				after := sort.Search(len(l.mutationMap.calculatedUids), func(i int) bool {
					return l.mutationMap.calculatedUids[i] > opt.AfterUid
				})
				if after >= len(l.mutationMap.calculatedUids) {
					l.RUnlock()
					return &pb.List{}, nil, false
				}

				afterIdx = after
			}

			copyArr := make([]uint64, len(l.mutationMap.calculatedUids)-afterIdx)
			copy(copyArr, l.mutationMap.calculatedUids[afterIdx:])
			out := &pb.List{Uids: copyArr}
			l.RUnlock()

			return out, nil, opt.Intersect != nil
		}
		// Pre-assign length to make it faster.
		l.RLock()
		defer l.RUnlock()
		// Use approximate length for initial capacity.
		res := make([]uint64, 0, l.ApproxLen())
		out := &pb.List{}

		if l.mutationMap.len() == 0 && opt.Intersect != nil && len(l.plist.Splits) == 0 {
			if opt.ReadTs < l.minTs {
				return out, errors.Wrapf(ErrTsTooOld, "While reading UIDs"), false
			}
			algo.IntersectCompressedWith(l.plist.Pack, opt.AfterUid, opt.Intersect, out)
			return out, nil, false
		}

		// If we need to intersect and the number of elements are small, in that case it's better to
		// just check each item is present or not.
		if opt.Intersect != nil && len(opt.Intersect.Uids) < l.ApproxLen() {
			// Cache the iterator as it makes the search space smaller each time.
			var pitr pIterator
			for _, uid := range opt.Intersect.Uids {
				ok, _, err := l.findPostingWithItr(opt.ReadTs, uid, pitr)
				if err != nil {
					return nil, err, false
				}
				if ok {
					res = append(res, uid)
				}
			}

			out.Uids = res
			return out, nil, false
		}

		// If we are going to iterate over the list, in that case we only need to read between min and max
		// of opt.Intersect.
		var uidMin, uidMax uint64 = 0, 0
		if opt.Intersect != nil && len(opt.Intersect.Uids) > 0 {
			uidMin = opt.Intersect.Uids[0]
			uidMax = opt.Intersect.Uids[len(opt.Intersect.Uids)-1]
		}

		err := l.iterate(opt.ReadTs, opt.AfterUid, func(p *pb.Posting) error {
			if p.PostingType == pb.Posting_REF {
				if p.Uid < uidMin {
					return nil
				}
				if p.Uid > uidMax && uidMax > 0 {
					return ErrStopIteration
				}
				res = append(res, p.Uid)

				if opt.First != 0 && len(res) > opt.First {
					return ErrStopIteration
				}
			}
			return nil
		})
		if err != nil {
			return out, errors.Wrapf(err, "cannot retrieve UIDs from list with key %s",
				hex.EncodeToString(l.key)), false
		}
		out.Uids = res
		return out, nil, true
	}

	// Do The intersection here as it's optimized.
	out, err, applyIntersectWith := getUidList()
	if err != nil || !applyIntersectWith || opt.First == 0 {
		return out, err
	}

	if opt.Intersect != nil && applyIntersectWith {
		algo.IntersectWith(out, opt.Intersect, out)
	}

	if opt.First != 0 {
		if opt.First < 0 {
			if len(out.Uids) > -opt.First {
				out.Uids = out.Uids[(len(out.Uids) + opt.First):]
			}
		} else if len(out.Uids) > opt.First {
			out.Uids = out.Uids[:opt.First]
		}
	}
	return out, nil
}

// Postings calls postFn with the postings that are common with
// UIDs in the opt ListOptions.
func (l *List) Postings(opt ListOptions, postFn func(*pb.Posting) error) error {
	l.RLock()
	defer l.RUnlock()

	err := l.iterate(opt.ReadTs, opt.AfterUid, func(p *pb.Posting) error {
		if p.PostingType != pb.Posting_REF {
			return nil
		}
		return postFn(p)
	})
	return errors.Wrapf(err, "cannot retrieve postings from list with key %s",
		hex.EncodeToString(l.key))
}

// AllUntaggedValues returns all the values in the posting list with no language tag.
func (l *List) AllUntaggedValues(readTs uint64) ([]types.Val, error) {
	l.RLock()
	defer l.RUnlock()

	var vals []types.Val
	err := l.iterate(readTs, 0, func(p *pb.Posting) error {
		if len(p.LangTag) == 0 {
			vals = append(vals, types.Val{
				Tid:   types.TypeID(p.ValType),
				Value: p.Value,
			})
		}
		return nil
	})
	return vals, errors.Wrapf(err, "cannot retrieve untagged values from list with key %s",
		hex.EncodeToString(l.key))
}

// allUntaggedFacets returns facets for all untagged values. Since works well only for
// fetching facets for list predicates as lang tag in not allowed for list predicates.
func (l *List) allUntaggedFacets(readTs uint64) ([]*pb.Facets, error) {
	l.AssertRLock()
	var facets []*pb.Facets
	err := l.iterate(readTs, 0, func(p *pb.Posting) error {
		if len(p.LangTag) == 0 {
			facets = append(facets, &pb.Facets{Facets: p.Facets})
		}
		return nil
	})

	return facets, errors.Wrapf(err, "cannot retrieve untagged facets from list with key %s",
		hex.EncodeToString(l.key))
}

// AllValues returns all the values in the posting list.
func (l *List) AllValues(readTs uint64) ([]types.Val, error) {
	l.RLock()
	defer l.RUnlock()

	var vals []types.Val
	err := l.iterate(readTs, 0, func(p *pb.Posting) error {
		vals = append(vals, types.Val{
			Tid:   types.TypeID(p.ValType),
			Value: p.Value,
		})
		return nil
	})
	return vals, errors.Wrapf(err, "cannot retrieve all values from list with key %s",
		hex.EncodeToString(l.key))
}

// GetLangTags finds the language tags of each posting in the list.
func (l *List) GetLangTags(readTs uint64) ([]string, error) {
	l.RLock()
	defer l.RUnlock()

	var tags []string
	err := l.iterate(readTs, 0, func(p *pb.Posting) error {
		tags = append(tags, string(p.LangTag))
		return nil
	})
	return tags, errors.Wrapf(err, "cannot retrieve language tags from list with key %s",
		hex.EncodeToString(l.key))
}

func (l *List) StaticValue(readTs uint64) (*pb.PostingList, error) {
	l.RLock()
	defer l.RUnlock()

	return l.findStaticValue(readTs), nil
}

func (l *List) findStaticValue(readTs uint64) *pb.PostingList {
	l.AssertRLock()

	if l.mutationMap == nil {
		// If mutation map is empty, check if there is some data, and return it.
		if l.plist != nil && len(l.plist.Postings) > 0 {
			return l.plist
		}
		return nil
	}

	// Return readTs is if it's present in the mutation. It's going to be the latest value.
	if l.mutationMap.currentEntries != nil && l.mutationMap.readTs == readTs {
		return l.mutationMap.currentEntries
	}

	// If maxTs < readTs then we need to read maxTs
	if l.maxTs <= readTs {
		if l.mutationMap.lastEntry != nil {
			return l.mutationMap.lastEntry
		}
		if mutation := l.mutationMap.get(l.maxTs); mutation != nil {
			return mutation
		}
	}

	// This means that maxTs > readTs. Go through the map to find the closest value to readTs
	var mutation *pb.PostingList
	ts_found := uint64(0)
	l.mutationMap.iterate(func(startTs uint64, mutation_i *pb.PostingList) {
		ts := mutation_i.CommitTs
		if ts > ts_found {
			ts_found = ts
			mutation = mutation_i
		}
	}, readTs)
	if mutation != nil {
		return mutation
	}

	// If we reach here, that means that there was no entry in mutation map which is less than readTs. That
	// means we need to return l.plist
	if l.plist != nil && len(l.plist.Postings) > 0 {
		return l.plist
	}
	return nil
}

// Value returns the default value from the posting list. The default value is
// defined as the value without a language tag.
// Value cannot be used to read from cache
func (l *List) Value(readTs uint64) (rval types.Val, rerr error) {
	l.RLock()
	defer l.RUnlock()
	return l.ValueWithLockHeld(readTs)
}

func (l *List) ValueWithLockHeld(readTs uint64) (rval types.Val, rerr error) {
	val, found, err := l.findValue(readTs, math.MaxUint64)
	if err != nil {
		return val, errors.Wrapf(err,
			"cannot retrieve default value from list with key %s", hex.EncodeToString(l.key))
	}
	if !found {
		return val, ErrNoValue
	}
	return val, nil
}

// ValueFor returns a value from posting list, according to preferred language list.
// If list is empty, value without language is returned; if such value is not
// available, value with smallest UID is returned.
// If list consists of one or more languages, first available value is returned.
// If no language from the list matches the values, processing is the same as for empty list.
func (l *List) ValueFor(readTs uint64, langs []string) (rval types.Val, rerr error) {
	l.RLock() // All public methods should acquire locks, while private ones should assert them.
	defer l.RUnlock()
	p, err := l.postingFor(readTs, langs)
	switch {
	case err == ErrNoValue:
		return rval, err
	case err != nil:
		return rval, errors.Wrapf(err, "cannot retrieve value with langs %v from list with key %s",
			langs, hex.EncodeToString(l.key))
	}
	return valueToTypesVal(p), nil
}

// PostingFor returns the posting according to the preferred language list.
func (l *List) PostingFor(readTs uint64, langs []string) (p *pb.Posting, rerr error) {
	l.RLock()
	defer l.RUnlock()
	return l.postingFor(readTs, langs)
}

func (l *List) postingFor(readTs uint64, langs []string) (p *pb.Posting, rerr error) {
	l.AssertRLock() // Avoid recursive locking by asserting a lock here.
	return l.postingForLangs(readTs, langs)
}

// ValueForTag returns the value in the posting list with the given language tag.
func (l *List) ValueForTag(readTs uint64, tag string) (rval types.Val, rerr error) {
	l.RLock()
	defer l.RUnlock()
	p, err := l.postingForTag(readTs, tag)
	if err != nil {
		return rval, err
	}
	return valueToTypesVal(p), nil
}

func valueToTypesVal(p *pb.Posting) (rval types.Val) {
	// This is ok because we dont modify the value of a posting. We create a newPosting
	// and add it to the PostingList to do a set.
	rval.Value = p.Value
	rval.Tid = types.TypeID(p.ValType)
	return
}

func (l *List) postingForLangs(readTs uint64, langs []string) (*pb.Posting, error) {
	l.AssertRLock()

	any := false
	// look for language in preferred order
	for _, lang := range langs {
		if lang == "." {
			any = true
			break
		}
		pos, err := l.postingForTag(readTs, lang)
		if err == nil {
			return pos, nil
		}
	}

	// look for value without language
	if any || len(langs) == 0 {
		found, pos, err := l.findPosting(readTs, math.MaxUint64)
		switch {
		case err != nil:
			return nil, errors.Wrapf(err,
				"cannot find value without language tag from list with key %s",
				hex.EncodeToString(l.key))
		case found:
			return pos, nil
		}
	}

	var found bool
	var pos *pb.Posting
	// last resort - return value with smallest lang UID.
	if any {
		err := l.iterate(readTs, 0, func(p *pb.Posting) error {
			if p.PostingType == pb.Posting_VALUE_LANG {
				pos = p
				found = true
				return ErrStopIteration
			}
			return nil
		})
		if err != nil {
			return nil, errors.Wrapf(err,
				"cannot retrieve value with the smallest lang UID from list with key %s",
				hex.EncodeToString(l.key))
		}
	}

	if found {
		return pos, nil
	}

	return pos, ErrNoValue
}

func (l *List) postingForTag(readTs uint64, tag string) (p *pb.Posting, rerr error) {
	l.AssertRLock()
	uid := farm.Fingerprint64([]byte(tag))
	found, p, err := l.findPosting(readTs, uid)
	if err != nil {
		return p, err
	}
	if !found {
		return p, ErrNoValue
	}

	return p, nil
}

func (l *List) findValue(readTs, uid uint64) (rval types.Val, found bool, err error) {
	l.AssertRLock()
	found, p, err := l.findPosting(readTs, uid)
	if !found {
		return rval, found, err
	}

	return valueToTypesVal(p), true, nil
}

func (l *List) FindPosting(readTs uint64, uid uint64) (found bool, pos *pb.Posting, err error) {
	return l.findPosting(readTs, uid)
}

func (l *List) findPostingWithItr(readTs uint64, uid uint64, pitr pIterator) (found bool, pos *pb.Posting, err error) {
	// Iterate starts iterating after the given argument, so we pass UID - 1
	// TODO Find what happens when uid = math.MaxUint64
	searchFurther, pos := l.mutationMap.findPosting(readTs, uid)
	if pos != nil {
		return true, pos, nil
	}
	if !searchFurther {
		return false, nil, nil
	}

	err = pitr.seek(l, uid-1, 0)
	if err != nil {
		return false, nil, errors.Wrapf(err,
			"cannot initialize iterator when calling List.iterate %s",
			l.mutationMap.print())
	}

	valid, err := pitr.valid()
	if err != nil {
		return false, nil, errors.Wrapf(err,
			"cannot initialize iterator when calling List.iterate %s",
			l.mutationMap.print())
	}
	if valid {
		pp := pitr.posting()
		if pp.Uid == uid {
			return true, pp, nil
		}
		return false, nil, nil
	}
	return false, nil, nil
}

func (l *List) findPosting(readTs uint64, uid uint64) (found bool, pos *pb.Posting, err error) {
	var pitr pIterator
	return l.findPostingWithItr(readTs, uid, pitr)
}

// Facets gives facets for the posting representing value.
func (l *List) Facets(readTs uint64, param *pb.FacetParams, langs []string,
	listType bool) ([]*pb.Facets, error) {
	l.RLock()
	defer l.RUnlock()

	var fcs []*pb.Facets
	if listType {
		fs, err := l.allUntaggedFacets(readTs)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot retrieve facets for predicate of list type")
		}

		for _, fcts := range fs {
			fcs = append(fcs, &pb.Facets{Facets: facets.CopyFacets(fcts.Facets, param)})
		}
		return fcs, nil
	}

	p, err := l.postingFor(readTs, langs)
	switch {
	case err == ErrNoValue:
		return nil, err
	case err != nil:
		return nil, errors.Wrapf(err, "cannot retrieve facet")
	}
	fcs = append(fcs, &pb.Facets{Facets: facets.CopyFacets(p.Facets, param)})
	return fcs, nil
}

// readListPart reads one split of a posting list from Badger.
func (l *List) readListPart(startUid uint64) (*pb.PostingList, error) {
	key, err := x.SplitKey(l.key, startUid)
	if err != nil {
		return nil, errors.Wrapf(err,
			"cannot generate key for list with base key %s and start UID %d",
			hex.EncodeToString(l.key), startUid)
	}
	txn := pstore.NewTransactionAt(l.minTs, false)
	defer txn.Discard()
	item, err := txn.Get(key)
	if err != nil {
		return nil, errors.Wrapf(err, "could not read list part with key %s",
			hex.EncodeToString(key))
	}
	part := &pb.PostingList{}
	if err := unmarshalOrCopy(part, item); err != nil {
		return nil, errors.Wrapf(err, "cannot unmarshal list part with key %s",
			hex.EncodeToString(key))
	}
	return part, nil
}

// shouldSplit returns true if the given plist should be split in two.
func shouldSplit(plist *pb.PostingList) bool {
	return proto.Size(plist) >= maxListSize && len(plist.Pack.Blocks) > 1
}

func (out *rollupOutput) updateSplits() {
	if out.plist == nil || len(out.parts) > 0 {
		out.plist = &pb.PostingList{}
	}
	out.plist.Splits = out.splits()
}

func (out *rollupOutput) recursiveSplit() {
	// Call splitUpList. Otherwise the map of startUids to parts won't be initialized.
	out.splitUpList()

	// Keep calling splitUpList until all the parts cannot be further split.
	for {
		needsSplit := false
		for _, part := range out.parts {
			if shouldSplit(part) {
				needsSplit = true
			}
		}

		if !needsSplit {
			return
		}
		out.splitUpList()
	}
}

// splitUpList checks the list and splits it in smaller parts if needed.
func (out *rollupOutput) splitUpList() {
	// Contains the posting lists that should be split.
	var lists []*pb.PostingList

	// If list is not split yet, insert the main list.
	if len(out.parts) == 0 {
		lists = append(lists, out.plist)
	}

	// Insert the split lists if they exist.
	for _, startUid := range out.splits() {
		part := out.parts[startUid]
		lists = append(lists, part)
	}

	for i, list := range lists {
		startUid := uint64(1)
		// If the list is split, select the right startUid for this list.
		if len(out.parts) > 0 {
			startUid = out.plist.Splits[i]
		}

		if shouldSplit(list) {
			// Split the list. Update out.splits with the new lists and add their
			// start UIDs to the list of new splits.
			startUids, pls := binSplit(startUid, list)
			for i, startUid := range startUids {
				out.parts[startUid] = pls[i]
			}
		}
	}

	out.updateSplits()
}

// binSplit takes the given plist and returns two new plists, each with
// half of the blocks and postings of the original as well as the new startUids
// for each of the new parts.
func binSplit(lowUid uint64, plist *pb.PostingList) ([]uint64, []*pb.PostingList) {
	midBlock := len(plist.Pack.Blocks) / 2
	midUid := plist.Pack.Blocks[midBlock].GetBase()

	// Generate posting list holding the first half of the current list's postings.
	lowPl := new(pb.PostingList)
	lowPl.Pack = &pb.UidPack{
		BlockSize: plist.Pack.BlockSize,
		Blocks:    plist.Pack.Blocks[:midBlock],
		AllocRef:  plist.Pack.AllocRef,
	}

	// Generate posting list holding the second half of the current list's postings.
	highPl := new(pb.PostingList)
	highPl.Pack = &pb.UidPack{
		BlockSize: plist.Pack.BlockSize,
		Blocks:    plist.Pack.Blocks[midBlock:],
		AllocRef:  plist.Pack.AllocRef,
	}

	// Add elements in plist.Postings to the corresponding list.
	pidx := sort.Search(len(plist.Postings), func(idx int) bool {
		return plist.Postings[idx].Uid >= midUid
	})
	lowPl.Postings = plist.Postings[:pidx]
	highPl.Postings = plist.Postings[pidx:]

	return []uint64{lowUid, midUid}, []*pb.PostingList{lowPl, highPl}
}

// removeEmptySplits updates the split list by removing empty posting lists' startUids.
func (out *rollupOutput) removeEmptySplits() {
	for startUid, plist := range out.parts {
		// Do not remove the first split for now, as every multi-part list should always
		// have a split starting with UID 1.
		if startUid == 1 {
			continue
		}

		if isPlistEmpty(plist) {
			delete(out.parts, startUid)
		}
	}
	out.updateSplits()

	if len(out.parts) == 1 && isPlistEmpty(out.parts[1]) {
		// Only the first split remains. If it's also empty, remove it as well.
		// This should mark the entire list for deletion. Please note that the
		// startUid of the first part is always one because a node can never have
		// its uid set to zero.
		if isPlistEmpty(out.parts[1]) {
			delete(out.parts, 1)
			out.plist.Splits = []uint64{}
		}
	}
}

// Returns the sorted list of start UIDs based on the keys in out.parts.
// out.parts is considered the source of truth so this method is considered
// safer than using out.plist.Splits directly.
func (out *rollupOutput) splits() []uint64 {
	var splits []uint64
	for startUid := range out.parts {
		splits = append(splits, startUid)
	}
	sortSplits(splits)
	return splits
}

// isPlistEmpty returns true if the given plist is empty. Plists with splits are
// considered non-empty.
func isPlistEmpty(plist *pb.PostingList) bool {
	if len(plist.Splits) > 0 {
		return false
	}
	if plist.Pack == nil || len(plist.Pack.Blocks) == 0 {
		return true
	}
	return false
}

func sortSplits(splits []uint64) {
	sort.Slice(splits, func(i, j int) bool {
		return splits[i] < splits[j]
	})
}

// PartSplits returns an empty array if the list has not been split into multiple parts.
// Otherwise, it returns an array containing the start UID of each part.
func (l *List) PartSplits() []uint64 {
	splits := make([]uint64, len(l.plist.Splits))
	copy(splits, l.plist.Splits)
	return splits
}

// FromBackupPostingList converts a posting list in the format used for backups to a
// normal posting list.
func FromBackupPostingList(bl *pb.BackupPostingList) *pb.PostingList {
	l := pb.PostingList{}
	if bl == nil {
		return &l
	}

	if len(bl.Uids) > 0 {
		l.Pack = codec.Encode(bl.Uids, blockSize)
	} else if len(bl.UidBytes) > 0 {
		l.Pack = codec.EncodeFromBuffer(bl.UidBytes, blockSize)
	}
	l.Postings = bl.Postings
	l.CommitTs = bl.CommitTs
	l.Splits = bl.Splits
	return &l
}
