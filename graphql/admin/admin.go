/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package admin

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	badgerpb "github.com/dgraph-io/badger/v4/pb"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/edgraph"
	"github.com/hypermodeinc/dgraph/v25/graphql/resolve"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/query"
	"github.com/hypermodeinc/dgraph/v25/worker"
	"github.com/hypermodeinc/dgraph/v25/x"
)

const (
	errMsgServerNotReady = "Unavailable: Server not ready."

	errNoGraphQLSchema = "Not resolving %s. There's no GraphQL schema in Dgraph. " +
		"Use the /admin API to add a GraphQL schema"
	errResolverNotFound = "%s was not executed because no suitable resolver could be found - " +
		"this indicates a resolver or validation bug. Please let us know by filing an issue."

	// GraphQL schema for /admin endpoint.
	graphqlAdminSchema = `
	"""
	The Int64 scalar type represents a signed 64-bit numeric non-fractional value.
	Int64 can represent values in range [-(2^63),(2^63 - 1)].
	"""
	scalar Int64

    """
	The UInt64 scalar type represents an unsigned 64-bit numeric non-fractional value.
	UInt64 can represent values in range [0,(2^64 - 1)].
	"""
    scalar UInt64

	"""
	The DateTime scalar type represents date and time as a string in RFC3339 format.
	For example: "1985-04-12T23:20:50.52Z" represents 20 mins 50.52 secs after the 23rd hour of Apr 12th 1985 in UTC.
	"""
	scalar DateTime

	"""
	Data about the GraphQL schema being served by Dgraph.
	"""
	type GQLSchema @dgraph(type: "dgraph.graphql") {
		id: ID!

		"""
		Input schema (GraphQL types) that was used in the latest schema update.
		"""
		schema: String!  @dgraph(pred: "dgraph.graphql.schema")

		"""
		The GraphQL schema that was generated from the 'schema' field.
		This is the schema that is being served by Dgraph at /graphql.
		"""
		generatedSchema: String!
	}

	"""
	A NodeState is the state of an individual node in the Dgraph cluster.
	"""
	type NodeState {

		"""
		Node type : either 'alpha' or 'zero'.
		"""
		instance: String

		"""
		Address of the node.
		"""
		address: String

		"""
		Node health status : either 'healthy' or 'unhealthy'.
		"""
		status: String

		"""
		The group this node belongs to in the Dgraph cluster.
		See : https://dgraph.io/docs/deploy/#cluster-setup.
		"""
		group: String

		"""
		Version of the Dgraph binary.
		"""
		version: String

		"""
		Time in nanoseconds since the node started.
		"""
		uptime: Int64

		"""
		Time in Unix epoch time that the node was last contacted by another Zero or Alpha node.
		"""
		lastEcho: Int64

		"""
		List of ongoing operations in the background.
		"""
		ongoing: [String]

		"""
		List of predicates for which indexes are built in the background.
		"""
		indexing: [String]

		"""
		List of Features that are enabled.
		"""
		ee_features: [String]
	}

	type MembershipState {
		counter: UInt64
		groups: [ClusterGroup]
		zeros: [Member]
		maxUID: UInt64
		maxNsID: UInt64
		maxTxnTs: UInt64
		maxRaftId: UInt64
		removed: [Member]
		cid: String
		"""
		Contains list of namespaces. Note that this is not stored in proto's MembershipState and
		computed at the time of query.
		"""
		namespaces: [UInt64]
	}

	type ClusterGroup {
		id: UInt64
		members: [Member]
		tablets: [Tablet]
		snapshotTs: UInt64
		checksum: UInt64
	}

	type Member {
		id: UInt64
		groupId: UInt64
		addr: String
		leader: Boolean
		amDead: Boolean
		lastUpdate: UInt64
		clusterInfoOnly: Boolean
		forceGroupId: Boolean
	}

	type Tablet {
		groupId: UInt64
		predicate: String
		force: Boolean
		space: Int
		remove: Boolean
		readOnly: Boolean
		moveTs: UInt64
	}

	directive @dgraph(type: String, pred: String) on OBJECT | INTERFACE | FIELD_DEFINITION
	directive @id on FIELD_DEFINITION
	directive @secret(field: String!, pred: String) on OBJECT | INTERFACE


	type UpdateGQLSchemaPayload {
		gqlSchema: GQLSchema
	}

	input UpdateGQLSchemaInput {
		set: GQLSchemaPatch!
	}

	input GQLSchemaPatch {
		schema: String!
	}

	input ExportInput {
		"""
		Data format for the export, e.g. "rdf" or "json" (default: "rdf")
		"""
		format: String

		"""
		Namespace for the export in multi-tenant cluster. Users from guardians of galaxy can export
		all namespaces by passing a negative value or specific namespaceId to export that namespace.
		"""
		namespace: Int

		"""
		Destination for the export: e.g. Minio or S3 bucket or /absolute/path
		"""
		destination: String

		"""
		Access key credential for the destination.
		"""
		accessKey: String

		"""
		Secret key credential for the destination.
		"""
		secretKey: String

		"""
		AWS session token, if required.
		"""
		sessionToken: String

		"""
		Set to true to allow backing up to S3 or Minio bucket that requires no credentials.
		"""
		anonymous: Boolean
	}

	input TaskInput {
		id: String!
	}

	type Response {
		code: String
		message: String
	}

	type ExportPayload {
		response: Response
		taskId: String
	}

	type DrainingPayload {
		response: Response
	}

	type ShutdownPayload {
		response: Response
	}

	type TaskPayload {
		kind: TaskKind
		status: TaskStatus
		lastUpdated: DateTime
	}

	enum TaskStatus {
		Queued
		Running
		Failed
		Success
		Unknown
	}

	enum TaskKind {
		Backup
		Export
		Unknown
	}

	input ConfigInput {
		"""
		Estimated memory the caches can take. Actual usage by the process would be
		more than specified here. The caches will be updated according to the
		cache_percentage flag.
		"""
		cacheMb: Float

		"""
		True value of logDQLRequest enables logging of all the requests coming to alphas.
		False value of logDQLRequest disables above.
		"""
		logDQLRequest: Boolean
	}

	type ConfigPayload {
		response: Response
	}

	type Config {
		cacheMb: Float
	}

	input RemoveNodeInput {
		"""
		ID of the node to be removed.
		"""
		nodeId: UInt64!

		"""
		ID of the group from which the node is to be removed.
		"""
		groupId: UInt64!
	}

	type RemoveNodePayload {
		response: Response
	}

	input MoveTabletInput {
		"""
		Namespace in which the predicate exists.
		"""
		namespace: UInt64

		"""
		Name of the predicate to move.
		"""
		tablet: String!

		"""
		ID of the destination group where the predicate is to be moved.
		"""
		groupId: UInt64!
	}

	type MoveTabletPayload {
		response: Response
	}

	enum AssignKind {
		UID
		TIMESTAMP
		NAMESPACE_ID
	}

	input AssignInput {
		"""
		Choose what to assign: UID, TIMESTAMP or NAMESPACE_ID.
		"""
		what: AssignKind!

		"""
		How many to assign.
		"""
		num: UInt64!
	}

	type AssignedIds {
		"""
		The first UID, TIMESTAMP or NAMESPACE_ID assigned.
		"""
		startId: UInt64

		"""
		The last UID, TIMESTAMP or NAMESPACE_ID assigned.
		"""
		endId: UInt64

		"""
		TIMESTAMP for read-only transactions.
		"""
		readOnly: UInt64
	}

	type AssignPayload {
		response: AssignedIds
	}

	` + adminTypes + `

	type Query {
		getGQLSchema: GQLSchema
		health: [NodeState]
		state: MembershipState
		config: Config
		task(input: TaskInput!): TaskPayload
		` + adminQueries + `
	}

	type Mutation {

		"""
		Update the Dgraph cluster to serve the input schema.  This may change the GraphQL
		schema, the types and predicates in the Dgraph schema, and cause indexes to be recomputed.
		"""
		updateGQLSchema(input: UpdateGQLSchemaInput!) : UpdateGQLSchemaPayload

		"""
		Starts an export of all data in the cluster.  Export format should be 'rdf' (the default
		if no format is given), or 'json'.
		See : https://dgraph.io/docs/deploy/#export-database
		"""
		export(input: ExportInput!): ExportPayload

		"""
		Set (or unset) the cluster draining mode.  In draining mode no further requests are served.
		"""
		draining(enable: Boolean): DrainingPayload

		"""
		Shutdown this node.
		"""
		shutdown: ShutdownPayload

		"""
		Alter the node's config.
		"""
		config(input: ConfigInput!): ConfigPayload

		"""
		Remove a node from the cluster.
		"""
		removeNode(input: RemoveNodeInput!): RemoveNodePayload

		"""
		Move a predicate from one group to another.
		"""
		moveTablet(input: MoveTabletInput!): MoveTabletPayload

		"""
		Lease UIDs, Timestamps or Namespace IDs in advance.
		"""
		assign(input: AssignInput!): AssignPayload

		` + adminMutations + `
	}
 `
)

var (
	// gogQryMWs are the middlewares which should be applied to queries served by
	// admin server for superadmin unless some exceptional behaviour is required
	gogQryMWs = resolve.QueryMiddlewares{
		resolve.IpWhitelistingMW4Query,
		resolve.GuardianOfTheGalaxyAuthMW4Query,
		resolve.LoggingMWQuery,
	}
	// gogMutMWs are the middlewares which should be applied to mutations
	// served by admin server for superadmin unless some exceptional behaviour is required
	gogMutMWs = resolve.MutationMiddlewares{
		resolve.IpWhitelistingMW4Mutation,
		resolve.GuardianOfTheGalaxyAuthMW4Mutation,
		resolve.LoggingMWMutation,
	}
	// gogAclMutMWs are the middlewares which should be applied to mutations
	// served by the admin server for superadmin with ACL enabled.
	gogAclMutMWs = resolve.MutationMiddlewares{
		resolve.IpWhitelistingMW4Mutation,
		resolve.AclOnlyMW4Mutation,
		resolve.GuardianOfTheGalaxyAuthMW4Mutation,
		resolve.LoggingMWMutation,
	}
	// stdAdminQryMWs are the middlewares which should be applied to queries served by admin
	// server unless some exceptional behaviour is required
	stdAdminQryMWs = resolve.QueryMiddlewares{
		resolve.IpWhitelistingMW4Query, // good to apply ip whitelisting before Guardian auth
		resolve.GuardianAuthMW4Query,
		resolve.LoggingMWQuery,
	}
	// stdAdminMutMWs are the middlewares which should be applied to mutations served by
	// admin server unless some exceptional behaviour is required
	stdAdminMutMWs = resolve.MutationMiddlewares{
		resolve.IpWhitelistingMW4Mutation, // good to apply ip whitelisting before Guardian auth
		resolve.GuardianAuthMW4Mutation,
		resolve.LoggingMWMutation,
	}
	// minimalAdminQryMWs is the minimal set of middlewares that should be applied to any query
	// served by the admin server
	minimalAdminQryMWs = resolve.QueryMiddlewares{
		resolve.IpWhitelistingMW4Query,
		resolve.LoggingMWQuery,
	}
	// minimalAdminMutMWs is the minimal set of middlewares that should be applied to any mutation
	// served by the admin server
	minimalAdminMutMWs = resolve.MutationMiddlewares{
		resolve.IpWhitelistingMW4Mutation,
		resolve.LoggingMWMutation,
	}
	adminQueryMWConfig = map[string]resolve.QueryMiddlewares{
		"health":       minimalAdminQryMWs, // dgraph checks Guardian auth for health
		"state":        minimalAdminQryMWs, // dgraph checks Guardian auth for state
		"config":       gogQryMWs,
		"listBackups":  gogQryMWs,
		"getGQLSchema": stdAdminQryMWs,
		// for queries and mutations related to User/Group, dgraph handles Guardian auth,
		// so no need to apply GuardianAuth Middleware
		"queryUser":      minimalAdminQryMWs,
		"queryGroup":     minimalAdminQryMWs,
		"getUser":        minimalAdminQryMWs,
		"getCurrentUser": minimalAdminQryMWs,
		"getGroup":       minimalAdminQryMWs,
	}
	adminMutationMWConfig = map[string]resolve.MutationMiddlewares{
		"backup":          gogMutMWs,
		"config":          gogMutMWs,
		"draining":        gogMutMWs,
		"export":          stdAdminMutMWs, // dgraph handles the export for other namespaces by superadmin
		"login":           minimalAdminMutMWs,
		"restore":         gogMutMWs,
		"shutdown":        gogMutMWs,
		"removeNode":      gogMutMWs,
		"moveTablet":      gogMutMWs,
		"assign":          gogMutMWs,
		"updateGQLSchema": stdAdminMutMWs,
		"addNamespace":    gogAclMutMWs,
		"deleteNamespace": gogAclMutMWs,
		"resetPassword":   gogAclMutMWs,
		// for queries and mutations related to User/Group, dgraph handles Guardian auth,
		// so no need to apply GuardianAuth Middleware
		"addUser":     minimalAdminMutMWs,
		"addGroup":    minimalAdminMutMWs,
		"updateUser":  minimalAdminMutMWs,
		"updateGroup": minimalAdminMutMWs,
		"deleteUser":  minimalAdminMutMWs,
		"deleteGroup": minimalAdminMutMWs,
	}
	// mainHealthStore stores the health of the main GraphQL server.
	mainHealthStore = &GraphQLHealthStore{}
	// adminServerVar stores a pointer to the adminServer. It is used for lazy loading schema.
	adminServerVar *adminServer
)

func SchemaValidate(sch string) error {
	schHandler, err := schema.NewHandler(sch, false)
	if err != nil {
		return err
	}

	_, err = schema.FromString(schHandler.GQLSchema(), x.RootNamespace)
	return err
}

// GraphQLHealth is used to report the health status of a GraphQL server.
// It is required for kubernetes probing.
type GraphQLHealth struct {
	Healthy   bool
	StatusMsg string
}

// GraphQLHealthStore stores GraphQLHealth in a thread-safe way.
type GraphQLHealthStore struct {
	v atomic.Value
}

func (g *GraphQLHealthStore) GetHealth() GraphQLHealth {
	v := g.v.Load()
	if v == nil {
		return GraphQLHealth{Healthy: false, StatusMsg: "init"}
	}
	return v.(GraphQLHealth)
}

func (g *GraphQLHealthStore) up() {
	g.v.Store(GraphQLHealth{Healthy: true, StatusMsg: "up"})
}

func (g *GraphQLHealthStore) updatingSchema() {
	g.v.Store(GraphQLHealth{Healthy: true, StatusMsg: "updating schema"})
}

type adminServer struct {
	rf       resolve.ResolverFactory
	resolver *resolve.RequestResolver

	// The mutex that locks schema update operations
	mux sync.RWMutex

	// The GraphQL server that's being admin'd
	gqlServer IServeGraphQL

	gqlSchemas *worker.GQLSchemaStore
	// When the schema changes, we use these to create a new RequestResolver for
	// the main graphql endpoint (gqlServer) and thus refresh the API.
	fns               *resolve.ResolverFns
	withIntrospection bool
	globalEpoch       map[uint64]*uint64
}

// NewServers initializes the GraphQL servers.  It sets up an empty server for the
// main /graphql endpoint and an admin server.  The result is mainServer, adminServer.
func NewServers(withIntrospection bool, globalEpoch map[uint64]*uint64,
	closer *z.Closer) (IServeGraphQL, IServeGraphQL, *GraphQLHealthStore) {
	gqlSchema, err := schema.FromString("", x.RootNamespace)
	if err != nil {
		x.Panic(err)
	}

	resolvers := resolve.New(gqlSchema, resolverFactoryWithErrorMsg(errNoGraphQLSchema))
	e := globalEpoch[x.RootNamespace]
	mainServer := NewServer()
	mainServer.Set(x.RootNamespace, e, resolvers)

	fns := &resolve.ResolverFns{
		Qrw: resolve.NewQueryRewriter(),
		Arw: resolve.NewAddRewriter,
		Urw: resolve.NewUpdateRewriter,
		Drw: resolve.NewDeleteRewriter(),
		Ex:  resolve.NewDgraphExecutor(),
	}
	adminResolvers := newAdminResolver(mainServer, fns, withIntrospection, globalEpoch, closer)
	e = globalEpoch[x.RootNamespace]
	adminServer := NewServer()
	adminServer.Set(x.RootNamespace, e, adminResolvers)

	return mainServer, adminServer, mainHealthStore
}

// newAdminResolver creates a GraphQL request resolver for the /admin endpoint.
func newAdminResolver(
	defaultGqlServer IServeGraphQL,
	fns *resolve.ResolverFns,
	withIntrospection bool,
	epoch map[uint64]*uint64,
	closer *z.Closer) *resolve.RequestResolver {

	adminSchema, err := schema.FromString(graphqlAdminSchema, x.RootNamespace)
	if err != nil {
		x.Panic(err)
	}

	rf := newAdminResolverFactory()

	server := &adminServer{
		rf:                rf,
		resolver:          resolve.New(adminSchema, rf),
		fns:               fns,
		withIntrospection: withIntrospection,
		globalEpoch:       epoch,
		gqlSchemas:        worker.NewGQLSchemaStore(),
		gqlServer:         defaultGqlServer,
	}
	adminServerVar = server // store the admin server in package variable

	prefix := x.DataKey(x.AttrInRootNamespace(worker.GqlSchemaPred), 0)
	// Remove uid from the key, to get the correct prefix
	prefix = prefix[:len(prefix)-8]
	// Listen for graphql schema changes in group 1.
	go worker.SubscribeForUpdates([][]byte{prefix}, x.IgnoreBytes, func(kvs *badgerpb.KVList) {

		kv := x.KvWithMaxVersion(kvs, [][]byte{prefix})
		glog.Infof("Updating GraphQL schema from subscription.")

		// Unmarshal the incoming posting list.
		pl := &pb.PostingList{}
		err := proto.Unmarshal(kv.GetValue(), pl)
		if err != nil {
			glog.Errorf("Unable to unmarshal the posting list for graphql schema update %s", err)
			return
		}

		if len(pl.Postings) == 2 && string(pl.Postings[0].Value) == "_STAR_ALL" {
			pl.Postings = pl.Postings[1:]
		}

		// There should be only one posting.
		if len(pl.Postings) != 1 {
			glog.Errorf("Only one posting is expected in the graphql schema posting list but got %d",
				len(pl.Postings))
			return
		}

		pk, err := x.Parse(kv.GetKey())
		if err != nil {
			glog.Errorf("Unable to find uid of updated schema %s", err)
			return
		}
		ns, _ := x.ParseNamespaceAttr(pk.Attr)

		newSchema := &worker.GqlSchema{
			ID:      query.UidToHex(pk.Uid),
			Version: kv.GetVersion(),
			Schema:  string(pl.Postings[0].Value),
		}

		server.mux.RLock()
		currentSchema, ok := server.gqlSchemas.GetCurrent(ns)
		if ok {
			schemaChanged := newSchema.Schema == currentSchema.Schema
			if newSchema.Version <= currentSchema.Version || schemaChanged {
				glog.Infof("namespace: %d. Skipping GraphQL schema update. "+
					"newSchema.Version: %d, oldSchema.Version: %d, schemaChanged: %v.",
					ns, newSchema.Version, currentSchema.Version, schemaChanged)
				server.mux.RUnlock()
				return
			}
		}
		server.mux.RUnlock()

		var gqlSchema schema.Schema
		// on drop_all, we will receive an empty string as the schema update
		if newSchema.Schema != "" {
			gqlSchema, err = generateGQLSchema(newSchema, ns)
			if err != nil {
				glog.Errorf("namespace: %d. Error processing GraphQL schema: %s.", ns, err)
				return
			}
		}

		server.mux.Lock()
		defer server.mux.Unlock()

		server.incrementSchemaUpdateCounter(ns)
		// if the schema hasn't been loaded yet, then we don't need to load it here
		currentSchema, ok = server.gqlSchemas.GetCurrent(ns)
		if !(ok && currentSchema.Loaded) {
			// this just set schema in admin server, so that next invalid badger subscription update gets rejected upfront
			server.gqlSchemas.Set(ns, newSchema)
			glog.Infof("namespace: %d. Skipping in-memory GraphQL schema update, "+
				"it will be lazy-loaded later.", ns)
			return
		}

		// update this schema in both admin and graphql server
		newSchema.Loaded = true
		server.gqlSchemas.Set(ns, newSchema)
		server.resetSchema(ns, gqlSchema)

		glog.Infof("namespace: %d. Successfully updated GraphQL schema. "+
			"Serving New GraphQL API.", ns)
	}, 1, closer)

	go server.initServer()

	return server.resolver
}

func newAdminResolverFactory() resolve.ResolverFactory {
	adminMutationResolvers := map[string]resolve.MutationResolverFunc{
		"addNamespace":    resolveAddNamespace,
		"backup":          resolveBackup,
		"config":          resolveUpdateConfig,
		"deleteNamespace": resolveDeleteNamespace,
		"draining":        resolveDraining,
		"export":          resolveExport,
		"login":           resolveLogin,
		"resetPassword":   resolveResetPassword,
		"restore":         resolveRestore,
		"shutdown":        resolveShutdown,
		"removeNode":      resolveRemoveNode,
		"moveTablet":      resolveMoveTablet,
		"assign":          resolveAssign,
		"restoreTenant":   resolveTenantRestore,
	}

	rf := resolverFactoryWithErrorMsg(errResolverNotFound).
		WithQueryMiddlewareConfig(adminQueryMWConfig).
		WithMutationMiddlewareConfig(adminMutationMWConfig).
		WithQueryResolver("health", func(q schema.Query) resolve.QueryResolver {
			return resolve.QueryResolverFunc(resolveHealth)
		}).
		WithQueryResolver("state", func(q schema.Query) resolve.QueryResolver {
			return resolve.QueryResolverFunc(resolveState)
		}).
		WithQueryResolver("config", func(q schema.Query) resolve.QueryResolver {
			return resolve.QueryResolverFunc(resolveGetConfig)
		}).
		WithQueryResolver("listBackups", func(q schema.Query) resolve.QueryResolver {
			return resolve.QueryResolverFunc(resolveListBackups)
		}).
		WithQueryResolver("task", func(q schema.Query) resolve.QueryResolver {
			return resolve.QueryResolverFunc(resolveTask)
		}).
		WithQueryResolver("getGQLSchema", func(q schema.Query) resolve.QueryResolver {
			return resolve.QueryResolverFunc(
				func(ctx context.Context, query schema.Query) *resolve.Resolved {
					return &resolve.Resolved{Err: errors.Errorf(errMsgServerNotReady), Field: q}
				})
		}).
		WithMutationResolver("updateGQLSchema", func(m schema.Mutation) resolve.MutationResolver {
			return resolve.MutationResolverFunc(
				func(ctx context.Context, m schema.Mutation) (*resolve.Resolved, bool) {
					return &resolve.Resolved{Err: errors.Errorf(errMsgServerNotReady), Field: m},
						false
				})
		})
	for gqlMut, resolver := range adminMutationResolvers {
		// gotta force go to evaluate the right function at each loop iteration
		// otherwise you get variable capture issues
		func(f resolve.MutationResolver) {
			rf.WithMutationResolver(gqlMut, func(m schema.Mutation) resolve.MutationResolver {
				return f
			})
		}(resolver)
	}

	return rf.WithSchemaIntrospection()
}

func getCurrentGraphQLSchema(namespace uint64) (*worker.GqlSchema, error) {
	uid, graphQLSchema, err := edgraph.GetGQLSchema(namespace)
	if err != nil {
		return nil, err
	}

	return &worker.GqlSchema{ID: uid, Schema: graphQLSchema}, nil
}

func generateGQLSchema(sch *worker.GqlSchema, ns uint64) (schema.Schema, error) {
	schHandler, err := schema.NewHandler(sch.Schema, false)
	if err != nil {
		return nil, err
	}
	sch.GeneratedSchema = schHandler.GQLSchema()
	generatedSchema, err := schema.FromString(sch.GeneratedSchema, ns)
	if err != nil {
		return nil, err
	}
	generatedSchema.SetMeta(schHandler.MetaInfo())

	return generatedSchema, nil
}

func (as *adminServer) initServer() {
	// Nothing else should be able to lock before here.  The admin resolvers aren't yet
	// set up (they all just error), so we will obtain the lock here without contention.
	// We then setup the admin resolvers and they must wait until we are done before the
	// first admin calls will go through.
	as.mux.Lock()
	defer as.mux.Unlock()

	// It takes a few seconds for the Dgraph cluster to be up and running.
	// Before that, trying to read the GraphQL schema will result in error:
	// "Please retry again, server is not ready to accept requests."
	// 5 seconds is a pretty reliable wait for a fresh instance to read the
	// schema on a first try.
	waitFor := 5 * time.Second

	for {
		<-time.After(waitFor)

		sch, err := getCurrentGraphQLSchema(x.RootNamespace)
		if err != nil {
			glog.Errorf("namespace: %d. Error reading GraphQL schema: %s.", x.RootNamespace, err)
			continue
		}
		sch.Loaded = true
		as.gqlSchemas.Set(x.RootNamespace, sch)
		// adding the actual resolvers for updateGQLSchema and getGQLSchema only after server has
		// current GraphQL schema, if there was any.
		as.addConnectedAdminResolvers()
		mainHealthStore.up()

		if sch.Schema == "" {
			glog.Infof("namespace: %d. No GraphQL schema in Dgraph; serving empty GraphQL API",
				x.RootNamespace)
			break
		}

		generatedSchema, err := generateGQLSchema(sch, x.RootNamespace)
		if err != nil {
			glog.Errorf("namespace: %d. Error processing GraphQL schema: %s.",
				x.RootNamespace, err)
			break
		}
		as.incrementSchemaUpdateCounter(x.RootNamespace)
		as.resetSchema(x.RootNamespace, generatedSchema)

		glog.Infof("namespace: %d. Successfully loaded GraphQL schema.  Serving GraphQL API.",
			x.RootNamespace)

		break
	}
}

// addConnectedAdminResolvers sets up the real resolvers
func (as *adminServer) addConnectedAdminResolvers() {

	qryRw := resolve.NewQueryRewriter()
	dgEx := resolve.NewDgraphExecutor()

	as.rf.WithMutationResolver("updateGQLSchema",
		func(m schema.Mutation) resolve.MutationResolver {
			return &updateSchemaResolver{admin: as}
		}).
		WithQueryResolver("getGQLSchema",
			func(q schema.Query) resolve.QueryResolver {
				return &getSchemaResolver{admin: as}
			}).
		WithQueryResolver("queryGroup",
			func(q schema.Query) resolve.QueryResolver {
				return resolve.NewQueryResolver(qryRw, dgEx)
			}).
		WithQueryResolver("queryUser",
			func(q schema.Query) resolve.QueryResolver {
				return resolve.NewQueryResolver(qryRw, dgEx)
			}).
		WithQueryResolver("getGroup",
			func(q schema.Query) resolve.QueryResolver {
				return resolve.NewQueryResolver(qryRw, dgEx)
			}).
		WithQueryResolver("getCurrentUser",
			func(q schema.Query) resolve.QueryResolver {
				return resolve.NewQueryResolver(&currentUserResolver{baseRewriter: qryRw}, dgEx)
			}).
		WithQueryResolver("getUser",
			func(q schema.Query) resolve.QueryResolver {
				return resolve.NewQueryResolver(qryRw, dgEx)
			}).
		WithMutationResolver("addUser",
			func(m schema.Mutation) resolve.MutationResolver {
				return resolve.NewDgraphResolver(resolve.NewAddRewriter(), dgEx)
			}).
		WithMutationResolver("addGroup",
			func(m schema.Mutation) resolve.MutationResolver {
				return resolve.NewDgraphResolver(NewAddGroupRewriter(), dgEx)
			}).
		WithMutationResolver("updateUser",
			func(m schema.Mutation) resolve.MutationResolver {
				return resolve.NewDgraphResolver(resolve.NewUpdateRewriter(), dgEx)
			}).
		WithMutationResolver("updateGroup",
			func(m schema.Mutation) resolve.MutationResolver {
				return resolve.NewDgraphResolver(NewUpdateGroupRewriter(), dgEx)
			}).
		WithMutationResolver("deleteUser",
			func(m schema.Mutation) resolve.MutationResolver {
				return resolve.NewDgraphResolver(resolve.NewDeleteRewriter(), dgEx)
			}).
		WithMutationResolver("deleteGroup",
			func(m schema.Mutation) resolve.MutationResolver {
				return resolve.NewDgraphResolver(resolve.NewDeleteRewriter(), dgEx)
			})
}

func resolverFactoryWithErrorMsg(msg string) resolve.ResolverFactory {
	errFunc := func(name string) error { return errors.Errorf(msg, name) }
	qErr :=
		resolve.QueryResolverFunc(func(ctx context.Context, query schema.Query) *resolve.Resolved {
			return &resolve.Resolved{Err: errFunc(query.ResponseName()), Field: query}
		})

	mErr := resolve.MutationResolverFunc(
		func(ctx context.Context, mutation schema.Mutation) (*resolve.Resolved, bool) {
			return &resolve.Resolved{Err: errFunc(mutation.ResponseName()), Field: mutation}, false
		})

	return resolve.NewResolverFactory(qErr, mErr)
}

func (as *adminServer) getGlobalEpoch(ns uint64) *uint64 {
	e := as.globalEpoch[ns]
	if e == nil {
		e = new(uint64)
		as.globalEpoch[ns] = e
	}
	return e
}

func (as *adminServer) incrementSchemaUpdateCounter(ns uint64) {
	// Increment the Epoch when you get a new schema. So, that subscription's local epoch
	// will match against global epoch to terminate the current subscriptions.
	atomic.AddUint64(as.getGlobalEpoch(ns), 1)
}

func (as *adminServer) resetSchema(ns uint64, gqlSchema schema.Schema) {
	// set status as updating schema
	mainHealthStore.updatingSchema()

	var resolverFactory resolve.ResolverFactory
	// gqlSchema can be nil in following cases:
	// * after DROP_ALL
	// * if the schema hasn't yet been set even once for a non-Galaxy namespace
	// If schema is nil then do not attach Resolver for
	// introspection operations, and set GQL schema to empty.
	if gqlSchema == nil {
		resolverFactory = resolverFactoryWithErrorMsg(errNoGraphQLSchema)
		gqlSchema, _ = schema.FromString("", ns)
	} else {
		resolverFactory = resolverFactoryWithErrorMsg(errResolverNotFound).
			WithConventionResolvers(gqlSchema, as.fns)
		// If the schema is a Federated Schema then attach "_service" resolver
		if gqlSchema.IsFederated() {
			resolverFactory.WithQueryResolver("_service", func(s schema.Query) resolve.QueryResolver {
				return resolve.QueryResolverFunc(func(ctx context.Context, query schema.Query) *resolve.Resolved {
					as.mux.RLock()
					defer as.mux.RUnlock()
					sch, ok := as.gqlSchemas.GetCurrent(ns)
					if !ok {
						return resolve.EmptyResult(query,
							fmt.Errorf("error while getting the schema for ns %d", ns))
					}
					handler, err := schema.NewHandler(sch.Schema, true)
					if err != nil {
						return resolve.EmptyResult(query, err)
					}
					data := handler.GQLSchemaWithoutApolloExtras()
					return resolve.DataResult(query,
						map[string]interface{}{"_service": map[string]interface{}{"sdl": data}},
						nil)
				})
			})
		}

		if as.withIntrospection {
			resolverFactory.WithSchemaIntrospection()
		}
	}

	resolvers := resolve.New(gqlSchema, resolverFactory)
	as.gqlServer.Set(ns, as.getGlobalEpoch(ns), resolvers)

	// reset status to up, as now we are serving the new schema
	mainHealthStore.up()
}

func (as *adminServer) lazyLoadSchema(namespace uint64) error {
	// if the schema is already in memory, no need to fetch it from disk
	if currentSchema, ok := as.gqlSchemas.GetCurrent(namespace); ok && currentSchema.Loaded {
		return nil
	}

	// otherwise, fetch the schema from disk
	sch, err := getCurrentGraphQLSchema(namespace)
	if err != nil {
		glog.Errorf("namespace: %d. Error reading GraphQL schema: %s.", namespace, err)
		return errors.Wrap(err, "failed to lazy-load GraphQL schema")
	}

	var generatedSchema schema.Schema
	if sch.Schema == "" {
		// if there was no schema stored in Dgraph, we still need to attach resolvers to the main
		// graphql server which should just return errors for any incoming request.
		// generatedSchema will be nil in this case
		glog.Infof("namespace: %d. No GraphQL schema in Dgraph; serving empty GraphQL API",
			namespace)
	} else {
		generatedSchema, err = generateGQLSchema(sch, namespace)
		if err != nil {
			glog.Errorf("namespace: %d. Error processing GraphQL schema: %s.", namespace, err)
			return errors.Wrap(err, "failed to lazy-load GraphQL schema")
		}
	}

	as.mux.Lock()
	defer as.mux.Unlock()
	sch.Loaded = true
	as.gqlSchemas.Set(namespace, sch)
	as.resetSchema(namespace, generatedSchema)

	glog.Infof("namespace: %d. Successfully lazy-loaded GraphQL schema.", namespace)
	return nil
}

func LazyLoadSchema(namespace uint64) error {
	return adminServerVar.lazyLoadSchema(namespace)
}

func inputArgError(err error) error {
	return schema.GQLWrapf(err, "couldn't parse input argument")
}

func response(code, msg string) map[string]interface{} {
	return map[string]interface{}{
		"response": map[string]interface{}{"code": code, "message": msg}}
}

// DestinationFields is used by both export and backup to specify destination
type DestinationFields struct {
	Destination  string
	AccessKey    string
	SecretKey    pb.Sensitive
	SessionToken pb.Sensitive
	Anonymous    bool
}
