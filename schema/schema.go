/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"sync"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"golang.org/x/net/trace"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/badger/v4"
	badgerpb "github.com/dgraph-io/badger/v4/pb"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/tok"
	"github.com/hypermodeinc/dgraph/v25/tok/hnsw"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var (
	pstate *state
	pstore *badger.DB
)

// We maintain two schemas for a predicate if a background task is building indexes
// for that predicate. Now, we need to use the new schema for mutations whereas
// a query schema for queries. While calling functions in this package, we need
// to set the context correctly as to which schema should be returned.
// Query schema is defined as (old schema - tokenizers to drop based on new schema).
type contextKey int

const (
	IsWrite           contextKey = iota
	IsUniqueDgraphXid            = true
)

// GetWriteContext returns a context that sets the schema context for writing.
func GetWriteContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, IsWrite, true)
}

func (s *state) init() {
	s.predicate = make(map[string]*pb.SchemaUpdate)
	s.types = make(map[string]*pb.TypeUpdate)
	s.elog = trace.NewEventLog("Dgraph", "Schema")
	s.mutSchema = make(map[string]*pb.SchemaUpdate)
}

type state struct {
	sync.RWMutex
	// Map containing predicate to type information.
	predicate map[string]*pb.SchemaUpdate
	types     map[string]*pb.TypeUpdate
	elog      trace.EventLog
	// mutSchema holds the schema update that is being applied in the background.
	mutSchema map[string]*pb.SchemaUpdate
}

// State returns the struct holding the current schema.
func State() *state {
	return pstate
}

func (s *state) DeleteAll() {
	s.Lock()
	defer s.Unlock()

	for pred := range s.predicate {
		delete(s.predicate, pred)
	}

	for typ := range s.types {
		delete(s.types, typ)
	}

	for pred := range s.mutSchema {
		delete(s.mutSchema, pred)
	}
}

// Delete updates the schema in memory and disk
func (s *state) Delete(attr string, ts uint64) error {
	s.Lock()
	defer s.Unlock()

	glog.Infof("Deleting schema for predicate: [%s]", attr)
	txn := pstore.NewTransactionAt(ts, true)
	defer txn.Discard()
	if err := txn.Delete(x.SchemaKey(attr)); err != nil {
		return err
	}
	// Delete is called rarely so sync write should be fine.
	if err := txn.CommitAt(ts, nil); err != nil {
		return err
	}

	delete(s.predicate, attr)
	delete(s.mutSchema, attr)
	return nil
}

// DeleteType updates the schema in memory and disk
func (s *state) DeleteType(typeName string, ts uint64) error {
	if s == nil {
		return nil
	}

	s.Lock()
	defer s.Unlock()

	glog.Infof("Deleting type definition for type: [%s]", typeName)
	txn := pstore.NewTransactionAt(ts, true)
	defer txn.Discard()
	if err := txn.Delete(x.TypeKey(typeName)); err != nil {
		return err
	}
	// Delete is called rarely so sync write should be fine.
	if err := txn.CommitAt(ts, nil); err != nil {
		return err
	}

	delete(s.types, typeName)
	return nil
}

// Namespaces returns the active namespaces based on the current types.
func (s *state) Namespaces() map[uint64]struct{} {
	if s == nil {
		return nil
	}

	s.RLock()
	defer s.RUnlock()

	ns := make(map[uint64]struct{})
	for typ := range s.types {
		ns[x.ParseNamespace(typ)] = struct{}{}
	}
	return ns
}

// DeletePredsForNs deletes the predicate information for the namespace from the schema.
func (s *state) DeletePredsForNs(delNs uint64) {
	if s == nil {
		return
	}
	s.Lock()
	defer s.Unlock()
	for pred := range s.predicate {
		ns := x.ParseNamespace(pred)
		if ns == delNs {
			delete(s.predicate, pred)
			delete(s.mutSchema, pred)
		}
	}
	for typ := range s.types {
		ns := x.ParseNamespace(typ)
		if ns == delNs {
			delete(s.types, typ)
		}
	}
}

func logUpdate(schema *pb.SchemaUpdate, pred string) string {
	if schema == nil {
		return ""
	}

	typ := types.TypeID(schema.ValueType).Name()
	if schema.List {
		typ = fmt.Sprintf("[%s]", typ)
	}
	return fmt.Sprintf("Setting schema for attr %s: %v, tokenizer: %v, directive: %v, count: %v\n",
		pred, typ, schema.Tokenizer, schema.Directive, schema.Count)
}

func logTypeUpdate(typ *pb.TypeUpdate, typeName string) string {
	return fmt.Sprintf("Setting type definition for type %s: %v\n", typeName, typ)
}

// Set sets the schema for the given predicate in memory.
// Schema mutations must flow through the update function, which are synced to the db.
func (s *state) Set(pred string, schema *pb.SchemaUpdate) {
	if schema == nil {
		return
	}

	s.Lock()
	defer s.Unlock()
	s.predicate[pred] = schema
	s.elog.Printf(logUpdate(schema, pred))
}

// SetMutSchema sets the mutation schema for the given predicate.
func (s *state) SetMutSchema(pred string, schema *pb.SchemaUpdate) {
	s.Lock()
	defer s.Unlock()
	s.mutSchema[pred] = schema
}

// DeleteMutSchema deletes the schema for given predicate from mutSchema.
func (s *state) DeleteMutSchema(pred string) {
	s.Lock()
	defer s.Unlock()
	delete(s.mutSchema, pred)
}

// GetIndexingPredicates returns the list of predicates for which we are building indexes.
func GetIndexingPredicates() []string {
	s := State()
	s.Lock()
	defer s.Unlock()
	if len(s.mutSchema) == 0 {
		return nil
	}

	ps := make([]string, 0, len(s.mutSchema))
	for p := range s.mutSchema {
		ps = append(ps, p)
	}
	return ps
}

// SetType sets the type for the given predicate in memory.
// schema mutations must flow through the update function, which are synced to the db.
func (s *state) SetType(typeName string, typ *pb.TypeUpdate) {
	s.Lock()
	defer s.Unlock()
	s.types[typeName] = typ
	s.elog.Printf(logTypeUpdate(typ, typeName))
}

// Get gets the schema for the given predicate.
func (s *state) Get(ctx context.Context, pred string) (pb.SchemaUpdate, bool) {
	isWrite, _ := ctx.Value(IsWrite).(bool)
	s.RLock()
	defer s.RUnlock()
	// If this is write context, mutSchema will have the updated schema.
	// If mutSchema doesn't have the predicate key, we use the schema from s.predicate.
	if isWrite {
		if schema, ok := s.mutSchema[pred]; ok {
			return *schema, true
		}
	}

	schema, ok := s.predicate[pred]
	if !ok {
		return pb.SchemaUpdate{}, false
	}
	return *schema, true
}

// GetType gets the type definition for the given type name.
func (s *state) GetType(typeName string) (pb.TypeUpdate, bool) {
	s.RLock()
	defer s.RUnlock()
	typ, has := s.types[typeName]
	if !has {
		return pb.TypeUpdate{}, false
	}
	return *typ, true
}

// TypeOf returns the schema type of predicate
func (s *state) TypeOf(pred string) (types.TypeID, error) {
	s.RLock()
	defer s.RUnlock()
	if schema, ok := s.predicate[pred]; ok {
		return types.TypeID(schema.ValueType), nil
	}
	return types.UndefinedID, errors.Errorf("Schema not defined for predicate: %v.", pred)
}

// IsIndexed returns whether the predicate is indexed or not
func (s *state) IsIndexed(ctx context.Context, pred string) bool {
	isWrite, _ := ctx.Value(IsWrite).(bool)
	s.RLock()
	defer s.RUnlock()
	if isWrite {
		// TODO(Aman): we could return the query schema if it is a delete.
		if schema, ok := s.mutSchema[pred]; ok &&
			(len(schema.Tokenizer) > 0 || len(schema.IndexSpecs) > 0) {
			return true
		}
	}

	if schema, ok := s.predicate[pred]; ok {
		return len(schema.Tokenizer) > 0 || len(schema.IndexSpecs) > 0
	}

	return false
}

// Predicates returns the list of predicates for given group
func (s *state) Predicates() []string {
	if s == nil {
		return nil
	}

	s.RLock()
	defer s.RUnlock()
	var out []string
	for k := range s.predicate {
		out = append(out, k)
	}
	return out
}

// Types returns the list of types.
func (s *state) Types() []string {
	if s == nil {
		return nil
	}

	s.RLock()
	defer s.RUnlock()
	var out []string
	for k := range s.types {
		out = append(out, k)
	}
	return out
}

// Tokenizer returns the tokenizer for given predicate
func (s *state) Tokenizer(ctx context.Context, pred string) []tok.Tokenizer {
	isWrite, _ := ctx.Value(IsWrite).(bool)
	s.RLock()
	defer s.RUnlock()
	var su *pb.SchemaUpdate
	if isWrite {
		if schema, ok := s.mutSchema[pred]; ok {
			su = schema
		}
	}
	if su == nil {
		if schema, ok := s.predicate[pred]; ok {
			su = schema
		}
	}
	if su == nil {
		// This may happen when some query that needs indexing over this predicate is executing
		// while the predicate is dropped from the state (using drop operation).
		glog.Errorf("Schema state not found for %s.", pred)
		return nil
	}
	tokenizers := make([]tok.Tokenizer, 0, len(su.Tokenizer))
	for _, it := range su.Tokenizer {
		t, found := tok.GetTokenizer(it)
		x.AssertTruef(found, "Invalid tokenizer %s", it)
		tokenizers = append(tokenizers, t)
	}
	return tokenizers
}

// FactoryCreateSpec(ctx, pred) returns the list of versioned
// FactoryCreateSpec instances for given predicate.
// The FactoryCreateSpec type defines the IndexFactory instance(s)
// for given predicate along with their options, if specified.
func (s *state) FactoryCreateSpec(ctx context.Context, pred string) ([]*tok.FactoryCreateSpec, error) {
	isWrite, _ := ctx.Value(IsWrite).(bool)
	s.RLock()
	defer s.RUnlock()
	var su *pb.SchemaUpdate
	if isWrite {
		if schema, ok := s.mutSchema[pred]; ok {
			su = schema
		}
	}
	if su == nil {
		if schema, ok := s.predicate[pred]; ok {
			su = schema
		}
	}
	if su == nil {
		// This may happen when some query that needs indexing over this predicate is executing
		// while the predicate is dropped from the state (using drop operation).
		glog.Errorf("Schema state not found for %s.", pred)
		return nil, errors.Errorf("Schema state not found for %s.", pred)
	}
	creates := make([]*tok.FactoryCreateSpec, 0, len(su.IndexSpecs))
	for _, vs := range su.IndexSpecs {
		c, err := tok.GetFactoryCreateSpecFromSpec(vs)
		if err != nil {
			return nil, err
		}
		creates = append(creates, c)
	}
	return creates, nil
}

// VectorIndexNames returns the indexes for given predicate
func (s *state) VectorIndexes(ctx context.Context, pred string) []string {
	var names []string
	vectorIndexes, err := s.FactoryCreateSpec(ctx, pred)
	if err != nil {
		glog.Errorf("Vector Tokenizers error for %s: %s", pred, err)
	}
	for _, v := range vectorIndexes {
		names = append(names, v.Name())
	}
	return names
}

// TokenizerNames returns the tokenizer names for given predicate
func (s *state) TokenizerNames(ctx context.Context, pred string) []string {
	var names []string
	tokenizers := s.Tokenizer(ctx, pred)
	for _, t := range tokenizers {
		names = append(names, t.Name())
	}
	return names
}

// HasTokenizer is a convenience func that checks if a given tokenizer is found in pred.
// Returns true if found, else false.
func (s *state) HasTokenizer(ctx context.Context, id byte, pred string) bool {
	for _, t := range s.Tokenizer(ctx, pred) {
		if t.Identifier() == id {
			return true
		}
	}
	return false
}

// IsReversed returns whether the predicate has reverse edge or not
func (s *state) IsReversed(ctx context.Context, pred string) bool {
	isWrite, _ := ctx.Value(IsWrite).(bool)
	s.RLock()
	defer s.RUnlock()
	if isWrite {
		if schema, ok := s.mutSchema[pred]; ok && schema.Directive == pb.SchemaUpdate_REVERSE {
			return true
		}
	}
	if schema, ok := s.predicate[pred]; ok {
		return schema.Directive == pb.SchemaUpdate_REVERSE
	}
	return false
}

// HasCount returns whether we want to mantain a count index for the given predicate or not.
func (s *state) HasCount(ctx context.Context, pred string) bool {
	isWrite, _ := ctx.Value(IsWrite).(bool)
	s.RLock()
	defer s.RUnlock()
	if isWrite {
		if schema, ok := s.mutSchema[pred]; ok && schema.Count {
			return true
		}
	}
	if schema, ok := s.predicate[pred]; ok {
		return schema.Count
	}
	return false
}

func (s *state) PredicatesToDelete(pred string) []string {
	s.RLock()
	defer s.RUnlock()
	preds := make([]string, 0)
	if schema, ok := s.predicate[pred]; ok {
		preds = append(preds, pred)

		if schema.ValueType == pb.Posting_VFLOAT && len(schema.IndexSpecs) != 0 {
			preds = append(preds, pred+hnsw.VecEntry)
			preds = append(preds, pred+hnsw.VecKeyword)
			preds = append(preds, pred+hnsw.VecDead)
			for i := range 1000 {
				preds = append(preds, fmt.Sprintf("%s%s_%d", pred, hnsw.VecEntry, i))
				preds = append(preds, fmt.Sprintf("%s%s_%d", pred, hnsw.VecKeyword, i))
				preds = append(preds, fmt.Sprintf("%s%s_%d", pred, hnsw.VecDead, i))
			}
		}
	}
	return preds
}

// IsList returns whether the predicate is of list type.
func (s *state) IsList(pred string) bool {
	s.RLock()
	defer s.RUnlock()
	if schema, ok := s.predicate[pred]; ok {
		return schema.List
	}
	return false
}

func (s *state) HasUpsert(pred string) bool {
	s.RLock()
	defer s.RUnlock()
	if schema, ok := s.predicate[pred]; ok {
		return schema.Upsert
	}
	return false
}

func (s *state) HasLang(pred string) bool {
	s.RLock()
	defer s.RUnlock()
	if schema, ok := s.predicate[pred]; ok {
		return schema.Lang
	}
	return false
}

func (s *state) HasNoConflict(pred string) bool {
	s.RLock()
	defer s.RUnlock()
	return s.predicate[pred].GetNoConflict()
}

// IndexingInProgress checks whether indexing is going on for a given predicate.
func (s *state) IndexingInProgress() bool {
	s.RLock()
	defer s.RUnlock()
	return len(s.mutSchema) > 0
}

// Init resets the schema state, setting the underlying DB to the given pointer.
func Init(ps *badger.DB) {
	pstore = ps
	reset()
}

// Load reads the latest schema for the given predicate from the DB.
func Load(predicate string) error {
	if len(predicate) == 0 {
		return errors.Errorf("Empty predicate")
	}
	State().DeleteMutSchema(predicate)
	key := x.SchemaKey(predicate)
	txn := pstore.NewTransactionAt(math.MaxUint64, false)
	defer txn.Discard()
	item, err := txn.Get(key)
	if err == badger.ErrKeyNotFound || err == badger.ErrBannedKey {
		return nil
	}
	if err != nil {
		return err
	}
	var s pb.SchemaUpdate
	err = item.Value(func(val []byte) error {
		x.Check(proto.Unmarshal(val, &s))
		return nil
	})
	if err != nil {
		return err
	}
	State().Set(predicate, &s)
	State().elog.Printf(logUpdate(&s, predicate))
	glog.Infoln(logUpdate(&s, predicate))
	return nil
}

// LoadFromDb reads schema information from db and stores it in memory
func LoadFromDb(ctx context.Context) error {
	// Reset the state because with the introduction of incremental restore,
	// it can't be assumed that the state would be empty before loading the
	// schema from the DB as we don't do drop all in case of incremental restores.
	State().DeleteAll()
	if err := loadFromDB(ctx, loadSchema); err != nil {
		return err
	}
	return loadFromDB(ctx, loadType)
}

const (
	loadSchema int = iota
	loadType
)

// loadFromDb iterates through the DB and loads all the stored schema updates.
func loadFromDB(ctx context.Context, loadType int) error {
	stream := pstore.NewStreamAt(math.MaxUint64)

	switch loadType {
	case loadSchema:
		stream.Prefix = x.SchemaPrefix()
		stream.LogPrefix = "LoadFromDb Schema"
	case loadType:
		stream.Prefix = x.TypePrefix()
		stream.LogPrefix = "LoadFromDb Type"
	default:
		glog.Fatalf("Invalid load type")
	}

	stream.KeyToList = func(key []byte, itr *badger.Iterator) (*badgerpb.KVList, error) {
		item := itr.Item()
		pk, err := x.Parse(key)
		if err != nil {
			glog.Errorf("Error while parsing key %s: %v", hex.Dump(key), err)
			return nil, nil
		}
		if len(pk.Attr) == 0 {
			glog.Warningf("Empty Attribute: %+v for Key: %x\n", pk, key)
			return nil, nil
		}

		switch loadType {
		case loadSchema:
			var s pb.SchemaUpdate
			err := item.Value(func(val []byte) error {
				if len(val) == 0 {
					s = pb.SchemaUpdate{Predicate: pk.Attr, ValueType: pb.Posting_DEFAULT}
				} else {
					x.Checkf(proto.Unmarshal(val, &s), "Error while loading schema from db")
				}
				State().Set(pk.Attr, &s)
				return nil
			})
			return nil, err
		case loadType:
			var t pb.TypeUpdate
			err := item.Value(func(val []byte) error {
				if len(val) == 0 {
					t = pb.TypeUpdate{TypeName: pk.Attr}
				} else {
					x.Checkf(proto.Unmarshal(val, &t), "Error while loading types from db")
				}
				State().SetType(pk.Attr, &t)
				return nil
			})
			return nil, err
		}
		glog.Fatalf("Invalid load type")
		return nil, errors.New("shouldn't reach here")
	}
	return stream.Orchestrate(ctx)
}

// InitialTypes returns the type updates to insert at the beginning of
// Dgraph's execution. It looks at the worker options to determine which
// types to insert.
func InitialTypes(namespace uint64) []*pb.TypeUpdate {
	return initialTypesInternal(namespace, false)
}

// CompleteInitialTypes returns all the type updates regardless of the worker
// options. This is useful in situations where the worker options are not known
// in advance or it is required to consider all initial pre-defined types. An
// example of such situation is while allowing type updates to go through during
// alter if they are same as existing pre-defined types. This is useful for
// live loading a previously exported schema.
func CompleteInitialTypes(namespace uint64) []*pb.TypeUpdate {
	return initialTypesInternal(namespace, true)
}

// NOTE: whenever defining a new type here, please also add it in x/keys.go: preDefinedTypeMap
func initialTypesInternal(namespace uint64, all bool) []*pb.TypeUpdate {
	var initialTypes []*pb.TypeUpdate
	initialTypes = append(initialTypes,
		&pb.TypeUpdate{
			TypeName: "dgraph.graphql",
			Fields: []*pb.SchemaUpdate{
				{
					Predicate: "dgraph.graphql.schema",
					ValueType: pb.Posting_STRING,
				},
				{
					Predicate: "dgraph.graphql.xid",
					ValueType: pb.Posting_STRING,
				},
			},
		},
		&pb.TypeUpdate{
			TypeName: "dgraph.graphql.persisted_query",
			Fields: []*pb.SchemaUpdate{
				{
					Predicate: "dgraph.graphql.p_query",
					ValueType: pb.Posting_STRING,
				},
			},
		})

	if namespace == x.RootNamespace {
		initialTypes = append(initialTypes,
			&pb.TypeUpdate{
				TypeName: "dgraph.namespace",
				Fields: []*pb.SchemaUpdate{
					{
						Predicate: "dgraph.namespace.name",
						ValueType: pb.Posting_STRING,
					},
					{
						Predicate: "dgraph.namespace.id",
						ValueType: pb.Posting_INT,
					},
				},
			})
	}

	if all || x.WorkerConfig.AclEnabled {
		// These type definitions are required for deleteUser and deleteGroup GraphQL API to work
		// properly.
		initialTypes = append(initialTypes,
			&pb.TypeUpdate{
				TypeName: "dgraph.type.User",
				Fields: []*pb.SchemaUpdate{
					{
						Predicate: "dgraph.xid",
						ValueType: pb.Posting_STRING,
					},
					{
						Predicate: "dgraph.password",
						ValueType: pb.Posting_PASSWORD,
					},
					{
						Predicate: "dgraph.user.group",
						ValueType: pb.Posting_UID,
					},
				},
			},
			&pb.TypeUpdate{
				TypeName: "dgraph.type.Group",
				Fields: []*pb.SchemaUpdate{
					{
						Predicate: "dgraph.xid",
						ValueType: pb.Posting_STRING,
					},
					{
						Predicate: "dgraph.acl.rule",
						ValueType: pb.Posting_UID,
					},
				},
			},
			&pb.TypeUpdate{
				TypeName: "dgraph.type.Rule",
				Fields: []*pb.SchemaUpdate{
					{
						Predicate: "dgraph.rule.predicate",
						ValueType: pb.Posting_STRING,
					},
					{
						Predicate: "dgraph.rule.permission",
						ValueType: pb.Posting_INT,
					},
				},
			})
	}

	for _, typ := range initialTypes {
		typ.TypeName = x.NamespaceAttr(namespace, typ.TypeName)
		for _, fields := range typ.Fields {
			fields.Predicate = x.NamespaceAttr(namespace, fields.Predicate)
		}
	}
	return initialTypes
}

// InitialSchema returns the schema updates to insert at the beginning of
// Dgraph's execution. It looks at the worker options to determine which
// attributes to insert.
func InitialSchema(namespace uint64) []*pb.SchemaUpdate {
	return initialSchemaInternal(namespace, false)
}

// CompleteInitialSchema returns all the schema updates regardless of the worker
// options. This is useful in situations where the worker options are not known
// in advance and it's better to create all the reserved predicates and remove
// them later than miss some of them. An example of such situation is during bulk
// loading.
func CompleteInitialSchema(namespace uint64) []*pb.SchemaUpdate {
	return initialSchemaInternal(namespace, true)
}

func initialSchemaInternal(namespace uint64, all bool) []*pb.SchemaUpdate {
	var initialSchema []*pb.SchemaUpdate

	initialSchema = append(initialSchema, []*pb.SchemaUpdate{
		{
			Predicate: "dgraph.type",
			ValueType: pb.Posting_STRING,
			Directive: pb.SchemaUpdate_INDEX,
			Tokenizer: []string{"exact"},
			List:      true,
		},
		{
			Predicate: "dgraph.drop.op",
			ValueType: pb.Posting_STRING,
		},
		{
			Predicate: "dgraph.graphql.schema",
			ValueType: pb.Posting_STRING,
		},
		{
			Predicate: "dgraph.graphql.xid",
			ValueType: pb.Posting_STRING,
			Directive: pb.SchemaUpdate_INDEX,
			Tokenizer: []string{"exact"},
			Upsert:    true,
		},
		{
			Predicate: "dgraph.graphql.p_query",
			ValueType: pb.Posting_STRING,
			Directive: pb.SchemaUpdate_INDEX,
			Tokenizer: []string{"sha256"},
		},
	}...)

	if namespace == x.RootNamespace {
		initialSchema = append(initialSchema, []*pb.SchemaUpdate{
			{
				Predicate: "dgraph.namespace.name",
				ValueType: pb.Posting_STRING,
				Directive: pb.SchemaUpdate_INDEX,
				Tokenizer: []string{"exact"},
				Unique:    true,
				Upsert:    true,
			},
			{
				Predicate: "dgraph.namespace.id",
				ValueType: pb.Posting_INT,
				Directive: pb.SchemaUpdate_INDEX,
				Tokenizer: []string{"int"},
				Unique:    true,
				Upsert:    true,
			},
		}...)
	}

	if all || x.WorkerConfig.AclEnabled {
		// propose the schema update for acl predicates
		initialSchema = append(initialSchema, []*pb.SchemaUpdate{
			{
				Predicate: "dgraph.xid",
				ValueType: pb.Posting_STRING,
				Directive: pb.SchemaUpdate_INDEX,
				Upsert:    true,
				Unique:    IsUniqueDgraphXid,
				Tokenizer: []string{"exact"},
			},
			{
				Predicate: "dgraph.password",
				ValueType: pb.Posting_PASSWORD,
			},
			{
				Predicate: "dgraph.user.group",
				Directive: pb.SchemaUpdate_REVERSE,
				ValueType: pb.Posting_UID,
				List:      true,
			},
			{
				Predicate: "dgraph.acl.rule",
				ValueType: pb.Posting_UID,
				List:      true,
			},
			{
				Predicate: "dgraph.rule.predicate",
				ValueType: pb.Posting_STRING,
				Directive: pb.SchemaUpdate_INDEX,
				Tokenizer: []string{"exact"},
				Upsert:    true, // Not really sure if this will work.
			},
			{
				Predicate: "dgraph.rule.permission",
				ValueType: pb.Posting_INT,
			},
		}...)
	}
	for _, sch := range initialSchema {
		sch.Predicate = x.NamespaceAttr(namespace, sch.Predicate)
	}
	return initialSchema
}

// CheckAndModifyPreDefPredicate returns true if the initial update for the pre-defined
// predicate is different from the passed update. It may also modify certain predicates
// under specific conditions.
// If the passed update is not a pre-defined predicate, it returns false.
func CheckAndModifyPreDefPredicate(update *pb.SchemaUpdate) bool {
	// Return false for non-pre-defined predicates.
	if !x.IsPreDefinedPredicate(update.Predicate) {
		return false
	}
	ns := x.ParseNamespace(update.Predicate)
	initialSchema := CompleteInitialSchema(ns)
	for _, original := range initialSchema {
		if original.Predicate != update.Predicate {
			continue
		}

		// For the dgraph.xid predicate, only the Unique field is allowed to be changed.
		// Previously, the Unique attribute was not applied to the dgraph.xid predicate.
		// For users upgrading from a lower version, we will set Unique to true.
		if update.Predicate == x.NamespaceAttr(ns, "dgraph.xid") && !update.Unique {
			if isDgraphXidChangeValid(original, update) {
				update.Unique = true
				return false
			}
		}
		return !proto.Equal(original, update)
	}
	return true
}

// isDgraphXidChangeValid returns true if the change in the dgraph.xid predicate is valid.
func isDgraphXidChangeValid(original, update *pb.SchemaUpdate) bool {
	changed := compareSchemaUpdates(original, update)
	return len(changed) == 1 && changed[0] == "Unique"
}

func compareSchemaUpdates(original, update *pb.SchemaUpdate) []string {
	var changes []string
	vOriginal := reflect.ValueOf(*original)
	vUpdate := reflect.ValueOf(*update)

	// Iterate through the fields of the original schema
	for i := range vOriginal.NumField() {
		field := vOriginal.Type().Field(i)

		// Skip unexported fields
		if !field.IsExported() {
			continue
		}

		fieldName := field.Name
		valueOriginal := vOriginal.Field(i)
		valueUpdate := vUpdate.Field(i)

		if !reflect.DeepEqual(valueOriginal.Interface(), valueUpdate.Interface()) {
			changes = append(changes, fieldName)
		}
	}

	return changes
}

// IsPreDefTypeChanged returns true if the initial update for the pre-defined
// type is different than the passed update.
// If the passed update is not a pre-defined type than it just returns false.
func IsPreDefTypeChanged(update *pb.TypeUpdate) bool {
	// Return false for non-pre-defined types.
	if !x.IsPreDefinedType(update.TypeName) {
		return false
	}

	initialTypes := CompleteInitialTypes(x.ParseNamespace(update.TypeName))
	for _, original := range initialTypes {
		if original.TypeName != update.TypeName {
			continue
		}
		if len(original.Fields) != len(update.Fields) {
			return true
		}
		for i, field := range original.Fields {
			if field.Predicate != update.Fields[i].Predicate {
				return true
			}
		}
	}

	return false
}

func reset() {
	pstate = new(state)
	pstate.init()
}
