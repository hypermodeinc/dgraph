/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package edgraph

import (
	"context"
	"fmt"
	"time"

	"github.com/dgraph-io/dgo/v240/protos/api"
	apiv25 "github.com/dgraph-io/dgo/v240/protos/api.v25"
	"github.com/hypermodeinc/dgraph/v24/dql"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/query"
	"github.com/hypermodeinc/dgraph/v24/schema"
	"github.com/hypermodeinc/dgraph/v24/worker"
	"github.com/hypermodeinc/dgraph/v24/x"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"google.golang.org/grpc/status"
)

type ServerV25 struct {
	apiv25.UnimplementedDgraphServer
}

func validateAlterReq(ctx context.Context) error {
	if err := x.HealthCheck(); err != nil {
		return err
	}
	if !isMutationAllowed(ctx) {
		return errors.Errorf("No mutations allowed by server.")
	}
	if _, err := hasAdminAuth(ctx, "Alter"); err != nil {
		return err
	}
	return nil
}

func executeDropAll(ctx context.Context, startTs uint64) error {
	if x.Config.BlockClusterWideDrop {
		glog.V(2).Info("Blocked drop-all because it is not permitted.")
		return errors.New("Drop all operation is not permitted.")
	}

	ctx = x.AttachJWTNamespace(ctx)

	m := &pb.Mutations{StartTs: startTs, DropOp: pb.Mutations_ALL}
	if _, err := query.ApplyMutations(ctx, m); err != nil {
		return err
	}

	// insert a helper record for backup & restore, indicating that drop_all was done
	if err := InsertDropRecord(ctx, "DROP_ALL;"); err != nil {
		return err
	}

	// insert empty GraphQL schema, so all alphas get notified to
	// reset their in-memory GraphQL schema
	if _, err := UpdateGQLSchema(ctx, "", ""); err != nil {
		return err
	}

	// recreate the admin account after a drop all operation
	InitializeAcl(nil)

	return nil
}

func executeDropAllInNs(ctx context.Context, startTs uint64, req *apiv25.AlterRequest) error {
	ctx = x.AttachJWTNamespace(ctx)

	nsID, err := getNamespaceIDFromName(ctx, req.NsName)
	if err != nil {
		return err
	}

	m := &pb.Mutations{
		StartTs:   startTs,
		DropOp:    pb.Mutations_ALL_IN_NS,
		DropValue: fmt.Sprintf("%#x", nsID),
	}
	if _, err := query.ApplyMutations(ctx, m); err != nil {
		return err
	}

	// TODO: handle this in backup and restore
	// insert a helper record for backup & restore, indicating that drop_all for a ns was done
	if err := InsertDropRecord(ctx, fmt.Sprintf("DROP_ALL_IN_NS;%#x", nsID)); err != nil {
		return err
	}

	// Attach the newly leased NsID in the context in order to create guardians/groot for it.
	m = &pb.Mutations{
		StartTs: worker.State.GetTimestamp(false),
		Schema:  schema.InitialSchema(nsID),
		Types:   schema.InitialTypes(nsID),
	}
	if _, err := query.ApplyMutations(ctx, m); err != nil {
		return err
	}

	err = x.RetryUntilSuccess(10, 100*time.Millisecond, func() error {
		return createGuardianAndGroot(x.AttachNamespace(ctx, nsID), nsID, "password")
	})
	if err != nil {
		return errors.Wrapf(err, "Failed to create guardian and groot: ")
	}

	return nil
}

func executeDropData(ctx context.Context, startTs uint64, req *apiv25.AlterRequest) error {
	nsID, err := getNamespaceIDFromName(x.AttachJWTNamespace(ctx), req.NsName)
	if err != nil {
		return err
	}

	// query the GraphQL schema and keep it in memory, so it can be inserted again
	_, graphQLSchema, err := GetGQLSchema(nsID)
	if err != nil {
		return err
	}

	m := &pb.Mutations{
		StartTs:   startTs,
		DropOp:    pb.Mutations_DATA,
		DropValue: fmt.Sprintf("%#x", nsID),
	}
	if _, err := query.ApplyMutations(x.AttachNamespace(ctx, nsID), m); err != nil {
		return err
	}

	// insert a helper record for backup & restore, indicating that drop_data was done
	if err := InsertDropRecord(x.AttachJWTNamespace(ctx), fmt.Sprintf("DROP_DATA;%#x", nsID)); err != nil {
		return err
	}

	// just reinsert the GraphQL schema, no need to alter dgraph schema as this was drop_data
	if _, err := UpdateGQLSchema(ctx, graphQLSchema, ""); err != nil {
		return err
	}

	// Since all data has been dropped, we need to recreate the admin account in the respective namespace.
	upsertGuardianAndGroot(nil, nsID)
	return nil
}

func executeDropPredicate(ctx context.Context, startTs uint64, req *apiv25.AlterRequest) error {
	if len(req.PredicateToDrop) == 0 {
		return errors.Errorf("PredicateToDrop cannot be empty")
	}

	nsID, err := getNamespaceIDFromName(x.AttachJWTNamespace(ctx), req.NsName)
	if err != nil {
		return err
	}

	// Pre-defined predicates cannot be dropped.
	attr := x.NamespaceAttr(nsID, req.PredicateToDrop)
	if x.IsPreDefinedPredicate(attr) {
		return errors.Errorf("predicate %s is pre-defined and is not allowed to be dropped", x.ParseAttr(attr))
	}

	nq := &api.NQuad{
		Namespace:   nsID,
		Subject:     x.Star,
		Predicate:   x.ParseAttr(attr),
		ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: x.Star}},
	}
	wnq := &dql.NQuad{NQuad: nq}
	edge, err := wnq.ToDeletePredEdge()
	if err != nil {
		return err
	}
	edges := []*pb.DirectedEdge{edge}

	m := &pb.Mutations{StartTs: startTs, Edges: edges}
	if _, err := query.ApplyMutations(x.AttachNamespace(ctx, nsID), m); err != nil {
		return err
	}

	// insert a helper record for backup & restore, indicating that drop_attr was done
	if err := InsertDropRecord(x.AttachJWTNamespace(ctx), "DROP_ATTR;"+attr); err != nil {
		return err
	}

	return nil
}

func executeDropType(ctx context.Context, startTs uint64, req *apiv25.AlterRequest) error {
	if len(req.TypeToDrop) == 0 {
		return errors.Errorf("TypeToDrop cannot be empty")
	}

	nsID, err := getNamespaceIDFromName(x.AttachJWTNamespace(ctx), req.NsName)
	if err != nil {
		return err
	}

	// Pre-defined types cannot be dropped.
	dropPred := x.NamespaceAttr(nsID, req.TypeToDrop)
	if x.IsPreDefinedType(dropPred) {
		return errors.Errorf("type %s is pre-defined and is not allowed to be dropped", req.TypeToDrop)
	}

	m := &pb.Mutations{DropOp: pb.Mutations_TYPE, DropValue: dropPred, StartTs: startTs}
	if _, err := query.ApplyMutations(x.AttachNamespace(ctx, nsID), m); err != nil {
		return err
	}

	return nil
}

func executeSetSchema(ctx context.Context, startTs uint64, req *apiv25.AlterRequest) error {
	nsID, err := getNamespaceIDFromName(x.AttachJWTNamespace(ctx), req.NsName)
	if err != nil {
		return err
	}

	result, err := parseSchemaFromAlterOperation(x.AttachNamespace(ctx, nsID), req.Schema)
	if err == errIndexingInProgress {
		// Make the client wait a bit.
		time.Sleep(time.Second)
		return err
	} else if err != nil {
		return err
	}

	glog.Infof("Got schema: %+v", result)

	m := &pb.Mutations{StartTs: startTs, Schema: result.Preds, Types: result.Types}
	if _, err := query.ApplyMutations(x.AttachNamespace(ctx, nsID), m); err != nil {
		return err
	}

	// wait for indexing to complete or context to be canceled.
	if err := worker.WaitForIndexing(ctx, !req.RunInBackground); err != nil {
		return err
	}

	return nil
}

// Alter handles requests to change the schema or remove parts or all of the data.
func (s *ServerV25) Alter(ctx context.Context, req *apiv25.AlterRequest) (*apiv25.AlterResponse, error) {
	ctx, span := otel.Tracer("").Start(ctx, "ServerV25.Alter")
	defer span.End()
	span.AddEvent("Alter operation", trace.WithAttributes(attribute.String("request", req.String())))

	// Always print out Alter operations because they are important and rare.
	glog.Infof("Received ALTER op: %+v", req)
	defer glog.Infof("ALTER op: %+v done", req)

	// For now, we only allow guadian of galaxies to do this operation in v25
	if err := AuthGuardianOfTheGalaxy(ctx); err != nil {
		s := status.Convert(err)
		return nil, status.Error(s.Code(),
			"v25.Alter can only be called by the guardian of the galaxy. "+s.Message())
	}

	if err := validateAlterReq(ctx); err != nil {
		return nil, err
	}

	// StartTs is not needed if the predicate to be dropped lies on this server
	// but is required if it lies on some other machine. Let's get it for safety.
	startTs := worker.State.GetTimestamp(false)

	empty := &apiv25.AlterResponse{}
	switch req.Op {
	case apiv25.AlterOp_DROP_ALL:
		return empty, executeDropAll(ctx, startTs)
	case apiv25.AlterOp_DROP_ALL_IN_NS:
		return empty, executeDropAllInNs(ctx, startTs, req)
	case apiv25.AlterOp_DROP_DATA_IN_NS:
		return empty, executeDropData(ctx, startTs, req)
	case apiv25.AlterOp_DROP_PREDICATE_IN_NS:
		return empty, executeDropPredicate(ctx, startTs, req)
	case apiv25.AlterOp_DROP_TYPE_IN_NS:
		return empty, executeDropType(ctx, startTs, req)
	case apiv25.AlterOp_SCHEMA_IN_NS:
		return empty, executeSetSchema(ctx, startTs, req)
	default:
		return empty, errors.Errorf("invalid operation in Alter Request")
	}
}
