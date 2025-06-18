/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package bulk

import (
	"fmt"
	"log"
	"math"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/badger/v4"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	wk "github.com/hypermodeinc/dgraph/v25/worker"
	"github.com/hypermodeinc/dgraph/v25/x"
)

type schemaStore struct {
	sync.RWMutex
	schemaMap map[string]*pb.SchemaUpdate
	types     []*pb.TypeUpdate
	*state
}

func newSchemaStore(initial *schema.ParsedSchema, opt *BulkOptions, state *state) *schemaStore {
	if opt == nil {
		log.Fatalf("Cannot create schema store with nil options.")
	}

	s := &schemaStore{
		schemaMap: map[string]*pb.SchemaUpdate{},
		state:     state,
	}

	// Initialize only for the default namespace. Initialization for other namespaces will be done
	// whenever we see data for a new namespace.
	s.checkAndSetInitialSchema(x.RootNamespace)

	s.types = initial.Types
	// This is from the schema read from the schema file.
	for _, sch := range initial.Preds {
		p := sch.Predicate
		sch.Predicate = "" // Predicate is stored in the (badger) key, so not needed in the value.
		if _, ok := s.schemaMap[p]; ok {
			fmt.Printf("Predicate %q already exists in schema\n", p)
			continue
		}
		s.checkAndSetInitialSchema(x.ParseNamespace(p))
		s.schemaMap[p] = sch
	}

	return s
}

func (s *schemaStore) getSchema(pred string) *pb.SchemaUpdate {
	s.RLock()
	defer s.RUnlock()
	return s.schemaMap[pred]
}

func (s *schemaStore) setSchemaAsList(pred string) {
	s.Lock()
	defer s.Unlock()
	sch, ok := s.schemaMap[pred]
	if !ok {
		return
	}
	sch.List = true
}

// checkAndSetInitialSchema initializes the schema for namespace if it does not already exist.
func (s *schemaStore) checkAndSetInitialSchema(namespace uint64) {
	if _, ok := s.namespaces.Load(namespace); ok {
		return
	}
	s.Lock()
	defer s.Unlock()

	if _, ok := s.namespaces.Load(namespace); ok {
		return
	}
	// Load all initial predicates. Some predicates that might not be used when
	// the alpha is started (e.g ACL predicates) might be included but it's
	// better to include them in case the input data contains triples with these
	// predicates.
	for _, update := range schema.CompleteInitialSchema(namespace) {
		s.schemaMap[update.Predicate] = update
	}
	s.types = append(s.types, schema.CompleteInitialTypes(namespace)...)

	if s.opt.StoreXids {
		s.schemaMap[x.NamespaceAttr(namespace, "xid")] = &pb.SchemaUpdate{
			ValueType: pb.Posting_STRING,
			Tokenizer: []string{"hash"},
		}
	}
	s.namespaces.Store(namespace, struct{}{})
}

func (s *schemaStore) validateType(de *pb.DirectedEdge, objectIsUID bool) {
	if objectIsUID {
		de.ValueType = pb.Posting_UID
	}

	s.RLock()
	sch, ok := s.schemaMap[de.Attr]
	s.RUnlock()
	if !ok {
		s.Lock()
		sch, ok = s.schemaMap[de.Attr]
		if !ok {
			sch = &pb.SchemaUpdate{ValueType: de.ValueType}
			if objectIsUID {
				sch.List = true
			}
			s.schemaMap[de.Attr] = sch
		}
		s.Unlock()
	}

	err := wk.ValidateAndConvert(de, sch)
	if err != nil {
		log.Fatalf("RDF doesn't match schema: %v", err)
	}
}

func (s *schemaStore) getPredicates(db *badger.DB) []string {
	txn := db.NewTransactionAt(math.MaxUint64, false)
	defer txn.Discard()

	opts := badger.DefaultIteratorOptions
	opts.PrefetchValues = false
	itr := txn.NewIterator(opts)
	defer itr.Close()

	m := make(map[string]struct{})
	for itr.Rewind(); itr.Valid(); {
		item := itr.Item()
		pk, err := x.Parse(item.Key())
		x.Check(err)
		m[pk.Attr] = struct{}{}
		itr.Seek(pk.SkipPredicate())
		continue
	}

	var preds []string
	for pred := range m {
		preds = append(preds, pred)
	}
	return preds
}

func (s *schemaStore) write(db *badger.DB, preds []string) {
	w := posting.NewTxnWriter(db)
	for _, pred := range preds {
		sch, ok := s.schemaMap[pred]
		if !ok {
			continue
		}
		k := x.SchemaKey(pred)
		v, err := proto.Marshal(sch)
		x.Check(err)
		// Write schema and types always at timestamp 1, s.state.writeTs may not be equal to 1
		// if bulk loader was restarted or other similar scenarios.
		x.Check(w.SetAt(k, v, posting.BitSchemaPosting, 1))
	}

	// Write all the types as all groups should have access to all the types.
	for _, typ := range s.types {
		k := x.TypeKey(typ.TypeName)
		v, err := proto.Marshal(typ)
		x.Check(err)
		x.Check(w.SetAt(k, v, posting.BitSchemaPosting, 1))
	}

	x.Check(w.Flush())
}
