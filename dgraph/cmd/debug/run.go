/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package debug

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	_ "net/http/pprof" // http profiler
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/badger/v4"
	bpb "github.com/dgraph-io/badger/v4/pb"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/codec"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/raftwal"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var (
	// Debug is the sub-command invoked when calling "dgraph debug"
	Debug x.SubCommand
	opt   flagOptions
)

type flagOptions struct {
	vals          bool
	keyLookup     string
	rollupKey     string
	keyHistory    bool
	predicate     string
	prefix        string
	readOnly      bool
	pdir          string
	itemMeta      bool
	jepsen        string
	readTs        uint64
	sizeHistogram bool
	noKeys        bool
	key           x.Sensitive
	onlySummary   bool
	parseKey      string

	// Options related to the WAL.
	wdir           string
	wtruncateUntil uint64
	wsetSnapshot   string
}

func init() {
	Debug.Cmd = &cobra.Command{
		Use:   "debug",
		Short: "Debug Dgraph instance",
		Run: func(cmd *cobra.Command, args []string) {
			run()
		},
		Annotations: map[string]string{"group": "debug"},
	}
	Debug.Cmd.SetHelpTemplate(x.NonRootTemplate)

	flag := Debug.Cmd.Flags()
	flag.BoolVar(&opt.itemMeta, "item", true, "Output item meta as well. Set to false for diffs.")
	flag.BoolVar(&opt.vals, "vals", false, "Output values along with keys.")
	flag.BoolVar(&opt.noKeys, "nokeys", false,
		"Ignore key_. Only consider amount when calculating total.")
	flag.StringVar(&opt.jepsen, "jepsen", "", "Disect Jepsen output. Can be linear/binary.")
	flag.Uint64Var(&opt.readTs, "at", math.MaxUint64, "Set read timestamp for all txns.")
	flag.BoolVarP(&opt.readOnly, "readonly", "o", true, "Open in read only mode.")
	flag.StringVarP(&opt.predicate, "pred", "r", "", "Only output specified predicate.")
	flag.StringVarP(&opt.prefix, "prefix", "", "", "Uses a hex prefix.")
	flag.StringVarP(&opt.keyLookup, "lookup", "l", "", "Hex of key to lookup.")
	flag.StringVar(&opt.rollupKey, "rollup", "", "Hex of key to rollup.")
	flag.BoolVarP(&opt.keyHistory, "history", "y", false, "Show all versions of a key.")
	flag.StringVarP(&opt.pdir, "postings", "p", "", "Directory where posting lists are stored.")
	flag.BoolVar(&opt.sizeHistogram, "histogram", false,
		"Show a histogram of the key and value sizes.")
	flag.BoolVar(&opt.onlySummary, "only-summary", false,
		"If true, only show the summary of the p directory.")

	// Flags related to WAL.
	flag.StringVarP(&opt.wdir, "wal", "w", "", "Directory where Raft write-ahead logs are stored.")
	flag.Uint64VarP(&opt.wtruncateUntil, "truncate", "t", 0,
		"Remove data from Raft entries until but not including this index.")
	flag.StringVarP(&opt.wsetSnapshot, "snap", "s", "",
		"Set snapshot term,index,readts to this. Value must be comma-separated list containing"+
			" the value for these vars in that order.")
	flag.StringVar(&opt.parseKey, "parse_key", "", "Parse hex key.")
	x.RegisterEncFlag(flag)
}

func toInt(o *pb.Posting) int {
	from := types.Val{
		Tid:   types.TypeID(o.ValType),
		Value: o.Value,
	}
	out, err := types.Convert(from, types.StringID)
	x.Check(err)
	val := out.Value.(string)
	a, err := strconv.Atoi(val)
	if err != nil {
		return 0
	}
	return a
}

func uidToVal(itr *badger.Iterator, prefix string) map[uint64]int {
	keys := make(map[uint64]int)
	var lastKey []byte
	for itr.Rewind(); itr.Valid(); {
		item := itr.Item()
		if bytes.Equal(lastKey, item.Key()) {
			itr.Next()
			continue
		}
		lastKey = append(lastKey[:0], item.Key()...)
		pk, err := x.Parse(item.Key())
		x.Check(err)
		if !pk.IsData() || !strings.HasPrefix(x.ParseAttr(pk.Attr), prefix) {
			continue
		}
		if pk.IsSchema() {
			continue
		}
		if pk.StartUid > 0 {
			// This key is part of a multi-part posting list. Skip it and only read
			// the main key, which is the entry point to read the whole list.
			continue
		}

		pl, err := posting.ReadPostingList(item.KeyCopy(nil), itr)
		if err != nil {
			log.Fatalf("Unable to read posting list: %v", err)
		}
		err = pl.Iterate(math.MaxUint64, 0, func(o *pb.Posting) error {
			from := types.Val{
				Tid:   types.TypeID(o.ValType),
				Value: o.Value,
			}
			out, err := types.Convert(from, types.StringID)
			x.Check(err)
			key := out.Value.(string)
			k, err := strconv.Atoi(key)
			x.Check(err)
			keys[pk.Uid] = k
			// fmt.Printf("Type: %v Uid=%d key=%s. commit=%d hex %x\n",
			// 	o.ValType, pk.Uid, key, o.CommitTs, lastKey)
			return nil
		})
		x.Checkf(err, "during iterate")
	}
	return keys
}

func seekTotal(db *badger.DB, readTs uint64) int {
	txn := db.NewTransactionAt(readTs, false)
	defer txn.Discard()

	iopt := badger.DefaultIteratorOptions
	iopt.AllVersions = true
	iopt.PrefetchValues = false
	itr := txn.NewIterator(iopt)
	defer itr.Close()

	keys := uidToVal(itr, "key_")
	fmt.Printf("Got keys: %+v\n", keys)
	vals := uidToVal(itr, "amount_")
	var total int
	for _, val := range vals {
		total += val
	}
	fmt.Printf("Got vals: %+v. Total: %d\n", vals, total)
	if opt.noKeys {
		// Ignore the key_ predicate. Only consider the amount_ predicate. Useful when tablets are
		// being moved around.
		keys = vals
	}

	total = 0
	for uid, key := range keys {
		a := vals[uid]
		fmt.Printf("uid: %-5d %x key: %d amount: %d\n", uid, uid, key, a)
		total += a
	}
	fmt.Printf("Total @ %d = %d\n", readTs, total)
	return total
}

func findFirstValidTxn(db *badger.DB) uint64 {
	readTs := opt.readTs
	var wrong uint64
	for {
		min, max := getMinMax(db, readTs-1)
		if max <= min {
			fmt.Printf("Can't find it. Max: %d\n", max)
			return 0
		}
		readTs = max
		if total := seekTotal(db, readTs); total != 100 {
			fmt.Printf("===> VIOLATION at ts: %d\n", readTs)
			showAllPostingsAt(db, readTs)
			wrong = readTs
		} else {
			fmt.Printf("===> Found first correct version at %d\n", readTs)
			showAllPostingsAt(db, readTs)
			return wrong
		}
	}
}

func findFirstInvalidTxn(db *badger.DB, lowTs, highTs uint64) uint64 {
	fmt.Println()
	if highTs-lowTs < 1 {
		fmt.Printf("Checking at lowTs: %d\n", lowTs)
		if total := seekTotal(db, lowTs); total != 100 {
			fmt.Printf("==> VIOLATION at ts: %d\n", lowTs)
			return lowTs
		}
		fmt.Printf("No violation found at ts: %d\n", lowTs)
		return 0
	}

	midTs := (lowTs + highTs) / 2
	fmt.Printf("Checking. low=%d. high=%d. mid=%d\n", lowTs, highTs, midTs)
	if total := seekTotal(db, midTs); total == 100 {
		// If no failure, move to higher ts.
		return findFirstInvalidTxn(db, midTs+1, highTs)
	}
	// Found an error.
	return findFirstInvalidTxn(db, lowTs, midTs)
}

func showAllPostingsAt(db *badger.DB, readTs uint64) {
	txn := db.NewTransactionAt(readTs, false)
	defer txn.Discard()

	itr := txn.NewIterator(badger.DefaultIteratorOptions)
	defer itr.Close()

	type account struct {
		Key int
		Amt int
	}
	keys := make(map[uint64]*account)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "SHOWING all postings at %d\n", readTs)
	for itr.Rewind(); itr.Valid(); itr.Next() {
		item := itr.Item()
		if item.Version() != readTs {
			continue
		}

		pk, err := x.Parse(item.Key())
		x.Check(err)
		if !pk.IsData() {
			continue
		}

		var acc *account
		attr := x.ParseAttr(pk.Attr)
		if strings.HasPrefix(attr, "key_") || strings.HasPrefix(attr, "amount_") {
			var has bool
			acc, has = keys[pk.Uid]
			if !has {
				acc = &account{}
				keys[pk.Uid] = acc
			}
		}
		fmt.Fprintf(&buf, "  key: %+v hex: %x\n", pk, item.Key())
		val, err := item.ValueCopy(nil)
		x.Check(err)
		var plist pb.PostingList
		x.Check(proto.Unmarshal(val, &plist))

		x.AssertTrue(len(plist.Postings) <= 1)
		var num int
		for _, p := range plist.Postings {
			num = toInt(p)
			appendPosting(&buf, p)
		}
		if num > 0 && acc != nil {
			switch {
			case strings.HasPrefix(attr, "key_"):
				acc.Key = num
			case strings.HasPrefix(attr, "amount_"):
				acc.Amt = num
			}
		}
	}
	for uid, acc := range keys {
		fmt.Fprintf(&buf, "Uid: %d %x Key: %d Amount: %d\n", uid, uid, acc.Key, acc.Amt)
	}
	fmt.Println(buf.String())
}

func getMinMax(db *badger.DB, readTs uint64) (uint64, uint64) {
	var min, max uint64 = math.MaxUint64, 0
	txn := db.NewTransactionAt(readTs, false)
	defer txn.Discard()

	iopt := badger.DefaultIteratorOptions
	iopt.AllVersions = true
	itr := txn.NewIterator(iopt)
	defer itr.Close()

	for itr.Rewind(); itr.Valid(); itr.Next() {
		item := itr.Item()
		if min > item.Version() {
			min = item.Version()
		}
		if max < item.Version() {
			max = item.Version()
		}
	}
	return min, max
}

func jepsen(db *badger.DB) {
	min, max := getMinMax(db, opt.readTs)
	fmt.Printf("min=%d. max=%d\n", min, max)

	var ts uint64
	switch opt.jepsen {
	case "binary":
		ts = findFirstInvalidTxn(db, min, max)
	case "linear":
		ts = findFirstValidTxn(db)
	}
	fmt.Println()
	if ts == 0 {
		fmt.Println("Nothing found. Exiting.")
		return
	}
	showAllPostingsAt(db, ts)
	seekTotal(db, ts-1)

	for range 5 {
		// Get a few previous commits.
		_, ts = getMinMax(db, ts-1)
		showAllPostingsAt(db, ts)
		seekTotal(db, ts-1)
	}
}

func history(lookup []byte, itr *badger.Iterator) {
	var buf bytes.Buffer
	pk, err := x.Parse(lookup)
	x.Check(err)
	fmt.Fprintf(&buf, "==> key: %x. PK: %+v\n", lookup, pk)
	for ; itr.Valid(); itr.Next() {
		item := itr.Item()
		if !bytes.Equal(item.Key(), lookup) {
			break
		}

		fmt.Fprintf(&buf, "ts: %d", item.Version())
		x.Check2(buf.WriteString(" {item}"))
		if item.IsDeletedOrExpired() {
			x.Check2(buf.WriteString("{deleted}"))
		}
		if item.DiscardEarlierVersions() {
			x.Check2(buf.WriteString("{discard}"))
		}
		val, err := item.ValueCopy(nil)
		x.Check(err)

		meta := item.UserMeta()
		if meta&posting.BitCompletePosting > 0 {
			x.Check2(buf.WriteString("{complete}"))
		}
		if meta&posting.BitDeltaPosting > 0 {
			x.Check2(buf.WriteString("{delta}"))
		}
		if meta&posting.BitEmptyPosting > 0 {
			x.Check2(buf.WriteString("{empty}"))
		}
		fmt.Fprintln(&buf)
		if meta&posting.BitDeltaPosting > 0 {
			plist := &pb.PostingList{}
			x.Check(proto.Unmarshal(val, plist))
			for _, p := range plist.Postings {
				appendPosting(&buf, p)
			}
		}
		if meta&posting.BitCompletePosting > 0 {
			var plist pb.PostingList
			x.Check(proto.Unmarshal(val, &plist))

			for _, p := range plist.Postings {
				appendPosting(&buf, p)
			}

			fmt.Fprintf(&buf, " Num uids = %d. Size = %d\n",
				codec.ExactLen(plist.Pack), proto.Size(plist.Pack))
			dec := codec.Decoder{Pack: plist.Pack}
			for uids := dec.Seek(0, codec.SeekStart); len(uids) > 0; uids = dec.Next() {
				for _, uid := range uids {
					fmt.Fprintf(&buf, " Uid = %d\n", uid)
				}
			}
		}
		x.Check2(buf.WriteString("\n"))
	}
	fmt.Println(buf.String())
}

func appendPosting(w io.Writer, o *pb.Posting) {
	fmt.Fprintf(w, " Uid: %d Op: %d ", o.Uid, o.Op)

	if len(o.Value) > 0 {
		fmt.Fprintf(w, " Type: %v. ", o.ValType)
		from := types.Val{
			Tid:   types.TypeID(o.ValType),
			Value: o.Value,
		}
		out, err := types.Convert(from, types.StringID)
		if err != nil {
			fmt.Fprintf(w, " Value: %q Error: %v", o.Value, err)
		} else {
			fmt.Fprintf(w, " String Value: %q", out.Value)
		}
	}
	fmt.Fprintln(w, "")
}
func rollupKey(db *badger.DB) {
	txn := db.NewTransactionAt(opt.readTs, false)
	defer txn.Discard()

	key, err := hex.DecodeString(opt.rollupKey)
	x.Check(err)

	iopts := badger.DefaultIteratorOptions
	iopts.AllVersions = true
	iopts.PrefetchValues = false
	itr := txn.NewKeyIterator(key, iopts)
	defer itr.Close()

	itr.Rewind()
	if !itr.Valid() {
		log.Fatalf("Unable to seek to key: %s", hex.Dump(key))
	}

	item := itr.Item()
	// Don't need to do anything if the bitdelta is not set.
	if item.UserMeta()&posting.BitDeltaPosting == 0 {
		fmt.Printf("First item has UserMeta:[b%04b]. Nothing to do\n", item.UserMeta())
		return
	}
	pl, err := posting.ReadPostingList(item.KeyCopy(nil), itr)
	x.Check(err)

	alloc := z.NewAllocator(32<<20, "Debug.RollupKey")
	defer alloc.Release()

	// Setting kvs at their original value as we can't give a new timestamp in debug mode.
	kvs, err := pl.Rollup(alloc, math.MaxUint64)
	x.Check(err)

	wb := db.NewManagedWriteBatch()
	x.Check(wb.WriteList(&bpb.KVList{Kv: kvs}))
	x.Check(wb.Flush())
}

func lookup(db *badger.DB) {
	txn := db.NewTransactionAt(opt.readTs, false)
	defer txn.Discard()

	iopts := badger.DefaultIteratorOptions
	iopts.AllVersions = true
	iopts.PrefetchValues = false
	itr := txn.NewIterator(iopts)
	defer itr.Close()

	key, err := hex.DecodeString(opt.keyLookup)
	if err != nil {
		log.Fatal(err)
	}
	itr.Seek(key)
	if !itr.Valid() {
		log.Fatalf("Unable to seek to key: %s", hex.Dump(key))
	}

	if opt.keyHistory {
		history(key, itr)
		return
	}

	var buf bytes.Buffer
	item := itr.Item()
	if item.UserMeta()&posting.BitSchemaPosting > 0 {
		// Schema is stored as pb.SchemaUpdate, we should not try to read it as a posting list
		fmt.Fprintf(&buf, "Key: %x\n", item.Key())
		schemaBytes, err := item.ValueCopy(nil)
		x.Check(err)

		var s pb.SchemaUpdate
		x.Check(proto.Unmarshal(schemaBytes, &s))
		fmt.Fprintf(&buf, "Value: %+v\n", s)
	} else {
		fmt.Fprintf(&buf, "Key: %x", item.Key())
		pl, err := posting.ReadPostingList(item.KeyCopy(nil), itr)
		if err != nil {
			log.Fatal(err)
		}
		pl.RLock()
		c := pl.GetLength(math.MaxUint64)
		pl.RUnlock()
		fmt.Fprintf(&buf, " Length: %d", c)

		splits := pl.PartSplits()
		isMultiPart := len(splits) > 0
		fmt.Fprintf(&buf, " Is multi-part list? %v", isMultiPart)
		if isMultiPart {
			fmt.Fprintf(&buf, " Start UID of parts: %v\n", splits)
		}

		err = pl.Iterate(math.MaxUint64, 0, func(o *pb.Posting) error {
			appendPosting(&buf, o)
			return nil
		})
		if err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println(buf.String())
}

// Current format is like:
// {i} attr: name term: [8] woods  ts: 535 item: [28, b0100] sz: 81 dcnt: 3 key: 00000...6f6f6473
// Fix the TestBulkLoadMultiShard accordingly, if the format changes.
func printKeys(db *badger.DB) {
	var prefix []byte
	if len(opt.predicate) > 0 {
		prefix = x.PredicatePrefix(opt.predicate)
	} else if len(opt.prefix) > 0 {
		p, err := hex.DecodeString(opt.prefix)
		x.Check(err)
		prefix = p
	}
	fmt.Printf("prefix = %s\n", hex.Dump(prefix))
	stream := db.NewStreamAt(opt.readTs)
	stream.Prefix = prefix
	var total uint64
	stream.KeyToList = func(key []byte, itr *badger.Iterator) (*bpb.KVList, error) {
		item := itr.Item()
		pk, err := x.Parse(key)
		x.Check(err)

		var buf bytes.Buffer
		// Don't use a switch case here. Because multiple of these can be true. In particular,
		// IsSchema can be true alongside IsData.
		if pk.IsData() {
			x.Check2(buf.WriteString("{d}"))
		}
		if pk.IsIndex() {
			x.Check2(buf.WriteString("{i}"))
		}
		if pk.IsCountOrCountRev() {
			x.Check2(buf.WriteString("{c}"))
		}
		if pk.IsSchema() {
			x.Check2(buf.WriteString("{s}"))
		}
		if pk.IsReverse() {
			x.Check2(buf.WriteString("{r}"))
		}
		ns, attr := x.ParseNamespaceAttr(pk.Attr)
		x.Check2(buf.WriteString(fmt.Sprintf(" ns: %#x ", ns)))
		x.Check2(buf.WriteString(" attr: " + attr))
		if len(pk.Term) > 0 {
			fmt.Fprintf(&buf, " term: [%d] [%v] ", pk.Term[0], pk.Term[1:])
		}
		if pk.Uid > 0 {
			fmt.Fprintf(&buf, " uid: %d ", pk.Uid)
		}
		if pk.StartUid > 0 {
			fmt.Fprintf(&buf, " startUid: %d ", pk.StartUid)
		}

		if opt.itemMeta {
			fmt.Fprintf(&buf, " ts: %d", item.Version())
			fmt.Fprintf(&buf, " item: [%d, b%04b]", item.EstimatedSize(), item.UserMeta())
		}

		var sz, deltaCount int64
		pl, err := posting.GetNew(key, db, opt.readTs, false)
		if err == nil {
			pl.RLock()
			c := pl.GetLength(math.MaxUint64)
			fmt.Fprintf(&buf, " countValue: [%d]", c)
			pl.RUnlock()
		}
	LOOP:
		for ; itr.ValidForPrefix(prefix); itr.Next() {
			item := itr.Item()
			if !bytes.Equal(item.Key(), key) {
				break
			}
			if item.IsDeletedOrExpired() {
				x.Check2(buf.WriteString(" {v.del}"))
				break
			}
			switch item.UserMeta() {
			// This is rather a default case as one of the 4 bit must be set.
			case posting.BitCompletePosting, posting.BitEmptyPosting, posting.BitSchemaPosting:
				sz += item.EstimatedSize()
				break LOOP
			case posting.BitDeltaPosting:
				sz += item.EstimatedSize()
				deltaCount++
			default:
				fmt.Printf("No user meta found for key: %s\n", hex.EncodeToString(key))
			}
			if item.DiscardEarlierVersions() {
				x.Check2(buf.WriteString(" {v.las}"))
				break
			}
		}
		var invalidSz, invalidCount uint64
		// skip all the versions of key
		for ; itr.ValidForPrefix(prefix); itr.Next() {
			item := itr.Item()
			if !bytes.Equal(item.Key(), key) {
				break
			}
			invalidSz += uint64(item.EstimatedSize())
			invalidCount++
		}

		fmt.Fprintf(&buf, " sz: %d dcnt: %d", sz, deltaCount)
		if invalidCount > 0 {
			fmt.Fprintf(&buf, " isz: %d icount: %d", invalidSz, invalidCount)
		}
		fmt.Fprintf(&buf, " key: %s", hex.EncodeToString(key))
		// If total size is more than 1 GB or we have more than 1 million keys, flag this key.
		if uint64(sz)+invalidSz > (1<<30) || uint64(deltaCount)+invalidCount > 10e6 {
			fmt.Fprintf(&buf, " [HEAVY]")
		}
		buf.WriteRune('\n')
		list := &bpb.KVList{}
		list.Kv = append(list.Kv, &bpb.KV{
			Value: buf.Bytes(),
		})
		// Don't call fmt.Println here. It is much slower.
		return list, nil
	}

	w := bufio.NewWriterSize(os.Stdout, 16<<20)
	stream.Send = func(buf *z.Buffer) error {
		var count int
		err := buf.SliceIterate(func(s []byte) error {
			var kv bpb.KV
			if err := proto.Unmarshal(s, &kv); err != nil {
				return err
			}
			x.Check2(w.Write(kv.Value))
			count++
			return nil
		})
		atomic.AddUint64(&total, uint64(count))
		return err
	}
	x.Check(stream.Orchestrate(context.Background()))
	x.Check(w.Flush())
	fmt.Println()
	fmt.Printf("Found %d keys\n", atomic.LoadUint64(&total))
}

// Creates bounds for an histogram. The bounds are powers of two of the form
// [2^min_exponent, ..., 2^max_exponent].
func getHistogramBounds(minExponent, maxExponent uint32) []float64 {
	var bounds []float64
	for i := minExponent; i <= maxExponent; i++ {
		bounds = append(bounds, float64(int(1)<<i))
	}
	return bounds
}

// HistogramData stores the information needed to represent the sizes of the keys and values
// as a histogram.
type HistogramData struct {
	Bounds         []float64
	Count          int64
	CountPerBucket []int64
	Min            int64
	Max            int64
	Sum            int64
}

// NewHistogramData returns a new instance of HistogramData with properly initialized fields.
func NewHistogramData(bounds []float64) *HistogramData {
	return &HistogramData{
		Bounds:         bounds,
		CountPerBucket: make([]int64, len(bounds)+1),
		Max:            0,
		Min:            math.MaxInt64,
	}
}

// Update changes the Min and Max fields if value is less than or greater than the current values.
func (histogram *HistogramData) Update(value int64) {
	if value > histogram.Max {
		histogram.Max = value
	}
	if value < histogram.Min {
		histogram.Min = value
	}

	histogram.Sum += value
	histogram.Count++

	for index := 0; index <= len(histogram.Bounds); index++ {
		// Allocate value in the last buckets if we reached the end of the Bounds array.
		if index == len(histogram.Bounds) {
			histogram.CountPerBucket[index]++
			break
		}

		if value < int64(histogram.Bounds[index]) {
			histogram.CountPerBucket[index]++
			break
		}
	}
}

// PrintHistogram prints the histogram data in a human-readable format.
func (histogram *HistogramData) PrintHistogram() {
	if histogram == nil {
		return
	}

	fmt.Printf("Min value: %d\n", histogram.Min)
	fmt.Printf("Max value: %d\n", histogram.Max)
	fmt.Printf("Mean: %.2f\n", float64(histogram.Sum)/float64(histogram.Count))
	fmt.Printf("%24s %9s\n", "Range", "Count")

	numBounds := len(histogram.Bounds)
	for index, count := range histogram.CountPerBucket {
		if count == 0 {
			continue
		}

		// The last bucket represents the bucket that contains the range from
		// the last bound up to infinity so it's processed differently than the
		// other buckets.
		if index == len(histogram.CountPerBucket)-1 {
			lowerBound := int(histogram.Bounds[numBounds-1])
			fmt.Printf("[%10d, %10s) %9d\n", lowerBound, "infinity", count)
			continue
		}

		upperBound := int(histogram.Bounds[index])
		lowerBound := 0
		if index > 0 {
			lowerBound = int(histogram.Bounds[index-1])
		}

		fmt.Printf("[%10d, %10d) %9d\n", lowerBound, upperBound, count)
	}
}

func sizeHistogram(db *badger.DB) {
	txn := db.NewTransactionAt(opt.readTs, false)
	defer txn.Discard()

	iopts := badger.DefaultIteratorOptions
	iopts.PrefetchValues = false
	itr := txn.NewIterator(iopts)
	defer itr.Close()

	// Generate distribution bounds. Key sizes are not greater than 2^16 while
	// value sizes are not greater than 1GB (2^30).
	keyBounds := getHistogramBounds(5, 16)
	valueBounds := getHistogramBounds(5, 30)

	// Initialize exporter.
	keySizeHistogram := NewHistogramData(keyBounds)
	valueSizeHistogram := NewHistogramData(valueBounds)

	// Collect key and value sizes.
	var prefix []byte
	if len(opt.predicate) > 0 {
		prefix = x.PredicatePrefix(opt.predicate)
	}
	var loop int
	for itr.Seek(prefix); itr.ValidForPrefix(prefix); itr.Next() {
		item := itr.Item()

		keySizeHistogram.Update(int64(len(item.Key())))
		valueSizeHistogram.Update(item.ValueSize())

		loop++
	}

	fmt.Printf("prefix = %s\n", hex.Dump(prefix))
	fmt.Printf("Found %d keys\n", loop)
	fmt.Printf("\nHistogram of key sizes (in bytes)\n")
	keySizeHistogram.PrintHistogram()
	fmt.Printf("\nHistogram of value sizes (in bytes)\n")
	valueSizeHistogram.PrintHistogram()
}

func printAlphaProposal(buf *bytes.Buffer, pr *pb.Proposal, pending map[uint64]bool) {
	if pr == nil {
		return
	}

	switch {
	case pr.Mutations != nil:
		fmt.Fprintf(buf, " Mutation . StartTs: %d . Edges: %d .",
			pr.Mutations.StartTs, len(pr.Mutations.Edges))
		if len(pr.Mutations.Edges) > 0 {
			pending[pr.Mutations.StartTs] = true
		} else {
			fmt.Fprintf(buf, " Mutation: %+v .", pr.Mutations)
		}
		fmt.Fprintf(buf, " Pending txns: %d .", len(pending))
	case len(pr.Kv) > 0:
		fmt.Fprintf(buf, " KV . Size: %d ", len(pr.Kv))
	case pr.State != nil:
		fmt.Fprintf(buf, " State . %+v ", pr.State)
	case pr.Delta != nil:
		fmt.Fprintf(buf, " Delta .")
		sort.Slice(pr.Delta.Txns, func(i, j int) bool {
			ti := pr.Delta.Txns[i]
			tj := pr.Delta.Txns[j]
			return ti.StartTs < tj.StartTs
		})
		fmt.Fprintf(buf, " Max: %d .", pr.Delta.GetMaxAssigned())
		for _, txn := range pr.Delta.Txns {
			delete(pending, txn.StartTs)
		}
		// There could be many thousands of txns within a single delta. We
		// don't need to print out every single entry, so just show the
		// first 10.
		if len(pr.Delta.Txns) >= 10 {
			fmt.Fprintf(buf, " Num txns: %d .", len(pr.Delta.Txns))
			pr.Delta.Txns = pr.Delta.Txns[:10]
		}
		for _, txn := range pr.Delta.Txns {
			fmt.Fprintf(buf, " %d → %d .", txn.StartTs, txn.CommitTs)
		}
		fmt.Fprintf(buf, " Pending txns: %d .", len(pending))
	case pr.Snapshot != nil:
		fmt.Fprintf(buf, " Snapshot . %+v ", pr.Snapshot)
	}
}

func printZeroProposal(buf *bytes.Buffer, zpr *pb.ZeroProposal) {
	if zpr == nil {
		return
	}

	switch {
	case len(zpr.SnapshotTs) > 0:
		fmt.Fprintf(buf, " Snapshot: %+v .", zpr.SnapshotTs)
	case zpr.Member != nil:
		fmt.Fprintf(buf, " Member: %+v .", zpr.Member)
	case zpr.Tablet != nil:
		fmt.Fprintf(buf, " Tablet: %+v .", zpr.Tablet)
	case zpr.MaxUID > 0:
		fmt.Fprintf(buf, " MaxUID: %d .", zpr.MaxUID)
	case zpr.MaxNsID > 0:
		fmt.Fprintf(buf, " MaxNsID: %d .", zpr.MaxNsID)
	case zpr.MaxRaftId > 0:
		fmt.Fprintf(buf, " MaxRaftId: %d .", zpr.MaxRaftId)
	case zpr.MaxTxnTs > 0:
		fmt.Fprintf(buf, " MaxTxnTs: %d .", zpr.MaxTxnTs)
	case zpr.Txn != nil:
		txn := zpr.Txn
		fmt.Fprintf(buf, " Txn %d → %d .", txn.StartTs, txn.CommitTs)
	default:
		fmt.Fprintf(buf, " Proposal: %+v .", zpr)
	}
}

func printSummary(db *badger.DB) {
	nsFromKey := func(key []byte) uint64 {
		pk, err := x.Parse(key)
		if err != nil {
			// Some of the keys are badger's internal and couldn't be parsed.
			// Hence, the error is expected in that case.
			fmt.Printf("Unable to parse key: %#x\n", key)
			return x.RootNamespace
		}
		return x.ParseNamespace(pk.Attr)
	}
	banned := db.BannedNamespaces()
	bannedNs := make(map[uint64]struct{})
	for _, ns := range banned {
		bannedNs[ns] = struct{}{}
	}

	tables := db.Tables()
	levelSizes := make([]uint64, len(db.Levels()))
	nsSize := make(map[uint64]uint64)
	for _, tab := range tables {
		levelSizes[tab.Level] += uint64(tab.OnDiskSize)
		if nsFromKey(tab.Left) == nsFromKey(tab.Right) {
			nsSize[nsFromKey(tab.Left)] += uint64(tab.OnDiskSize)
		}
	}

	fmt.Println("[SUMMARY]")
	totalSize := uint64(0)
	for i, sz := range levelSizes {
		fmt.Printf("Level %d size: %12s\n", i, humanize.IBytes(sz))
		totalSize += sz
	}
	fmt.Printf("Total SST size: %12s\n", humanize.IBytes(totalSize))
	fmt.Println()
	for ns, sz := range nsSize {
		fmt.Printf("Namespace %#x size: %12s", ns, humanize.IBytes(sz))
		if _, ok := bannedNs[ns]; ok {
			fmt.Printf(" (banned)")
		}
		fmt.Println()
	}
	fmt.Println()
}

func run() {
	go func() {
		for i := 8080; i < 9080; i++ {
			fmt.Printf("Listening for /debug HTTP requests at port: %d\n", i)
			if err := http.ListenAndServe(fmt.Sprintf("localhost:%d", i), nil); err != nil {
				fmt.Println("Port busy. Trying another one...")
				continue
			}
		}
	}()

	if opt.parseKey != "" {
		k, err := hex.DecodeString(opt.parseKey)
		if err != nil {
			log.Fatalf("error while decoding hex key: %v\n", err)
		}
		pk, err := x.Parse(k)
		if err != nil {
			log.Fatalf("error while parsing key: %v\n", err)
		}
		if pk.IsData() {
			fmt.Printf("{d}")
		}
		if pk.IsIndex() {
			fmt.Printf("{i}")
		}
		if pk.IsCountOrCountRev() {
			fmt.Printf("{c}")
		}
		if pk.IsSchema() {
			fmt.Printf("{s}")
		}
		if pk.IsReverse() {
			fmt.Printf("{r}")
		}
		fmt.Printf(" Key: %+v\n", pk)
		return
	}

	var err error
	dir := opt.pdir
	isWal := false
	if len(dir) == 0 {
		dir = opt.wdir
		isWal = true
	}
	keys, err := x.GetEncAclKeys(Debug.Conf)
	x.Check(err)
	opt.key = keys.EncKey

	if isWal {
		store, err := raftwal.InitEncrypted(dir, opt.key)
		x.Check(err)
		defer func() { x.Check(store.Close()) }()
		if err := handleWal(store); err != nil {
			fmt.Printf("\nGot error while handling WAL: %v\n", err)
		}
		return
	}

	bopts := badger.DefaultOptions(dir).
		WithReadOnly(opt.readOnly).
		WithEncryptionKey(opt.key).
		WithBlockCacheSize(1 << 30).
		WithIndexCacheSize(1 << 30).
		WithNamespaceOffset(x.NamespaceOffset) // We don't want to see the banned data.

	x.AssertTruef(len(bopts.Dir) > 0, "No posting or wal dir specified.")
	fmt.Printf("Opening DB: %s\n", bopts.Dir)

	db, err := badger.OpenManaged(bopts)
	x.Check(err)
	// Not using posting list cache
	posting.Init(db, 0, false)
	defer db.Close()

	printSummary(db)
	if opt.onlySummary {
		return
	}

	// Commenting the following out because on large Badger DBs, this can take a LONG time.
	// min, max := getMinMax(db, opt.readTs)
	// fmt.Printf("Min commit: %d. Max commit: %d, w.r.t %d\n", min, max, opt.readTs)

	switch {
	case len(opt.rollupKey) > 0:
		rollupKey(db)
	case len(opt.keyLookup) > 0:
		lookup(db)
	case len(opt.jepsen) > 0:
		jepsen(db)
	case opt.vals:
		total := seekTotal(db, opt.readTs)
		fmt.Printf("Total: %d\n", total)
	case opt.sizeHistogram:
		sizeHistogram(db)
	default:
		printKeys(db)
	}
}
