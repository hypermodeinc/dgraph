/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"context"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/dql"
	"github.com/hypermodeinc/dgraph/v24/protos/pb"
	"github.com/hypermodeinc/dgraph/v24/types/facets"
	"github.com/hypermodeinc/dgraph/v24/worker"
	"github.com/hypermodeinc/dgraph/v24/x"
)

// ApplyMutations performs the required edge expansions and forwards the results to the
// worker to perform the mutations.
func ApplyMutations(ctx context.Context, m *pb.Mutations) (*api.TxnContext, error) {
	// In expandEdges, for non * type prredicates, we prepend the namespace directly and for
	// * type predicates, we fetch the predicates and prepend the namespace.
	edges, err := ExpandEdges(ctx, m)
	if err != nil {
		return nil, errors.Wrapf(err, "While adding pb.edges")
	}
	m.Edges = edges

	err = checkIfDeletingAclOperation(ctx, m.Edges)
	if err != nil {
		return nil, err
	}
	tctx, err := worker.MutateOverNetwork(ctx, m)
	if err != nil {
		span := trace.SpanFromContext(ctx)
		if span.IsRecording() {
			span.AddEvent("MutateOverNetwork Error", trace.WithAttributes(
				attribute.String("error", err.Error()),
				attribute.String("mutation", m.String())))
		}
	}
	return tctx, err
}

func ExpandEdges(ctx context.Context, m *pb.Mutations) ([]*pb.DirectedEdge, error) {
	edges := make([]*pb.DirectedEdge, 0, 2*len(m.Edges))
	namespace, err := x.ExtractNamespace(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "While expanding edges")
	}
	isGalaxyQuery := x.IsGalaxyOperation(ctx)

	// Reset the namespace to the original.
	defer func(ns uint64) {
		x.AttachNamespace(ctx, ns)
	}(namespace)

	for _, edge := range m.Edges {
		x.AssertTrue(edge.Op == pb.DirectedEdge_DEL || edge.Op == pb.DirectedEdge_SET)
		if isGalaxyQuery {
			// The caller should make sure that the directed edges contain the namespace we want
			// to insert into. Now, attach the namespace in the context, so that further query
			// proceeds as if made from the user of 'namespace'.
			namespace = edge.GetNamespace()
			x.AttachNamespace(ctx, namespace)
		}

		var preds []string
		if edge.Attr != x.Star {
			preds = []string{x.NamespaceAttr(namespace, edge.Attr)}
		} else {
			sg := &SubGraph{}
			sg.DestUIDs = &pb.List{Uids: []uint64{edge.GetEntity()}}
			sg.ReadTs = m.StartTs
			types, err := getNodeTypes(ctx, sg)
			if err != nil {
				return nil, err
			}
			preds = append(preds, getPredicatesFromTypes(namespace, types)...)
			preds = append(preds, x.StarAllPredicates(namespace)...)
			// AllowedPreds are used only with ACL. Do not delete all predicates but
			// delete predicates to which the mutation has access
			if edge.AllowedPreds != nil {
				// Take intersection of preds and AllowedPreds
				intersectPreds := make([]string, 0)
				hashMap := make(map[string]bool)
				for _, allowedPred := range edge.AllowedPreds {
					hashMap[allowedPred] = true
				}
				for _, pred := range preds {
					if _, found := hashMap[pred]; found {
						intersectPreds = append(intersectPreds, pred)
					}
				}
				preds = intersectPreds
			}
		}

		for _, pred := range preds {
			// Do not return reverse edges.
			if x.ParseAttr(pred)[0] == '~' {
				continue
			}
			edgeCopy := *edge
			edgeCopy.Attr = pred
			edges = append(edges, &edgeCopy)
		}
	}

	return edges, nil
}

func verifyUid(ctx context.Context, uid uint64) error {
	if uid <= worker.MaxLeaseId() {
		return nil
	}
	deadline := time.Now().Add(3 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			lease := worker.MaxLeaseId()
			if uid <= lease {
				return nil
			}
			if time.Now().After(deadline) {
				err := errors.Errorf("Uid: [%d] cannot be greater than lease: [%d]", uid, lease)
				glog.V(2).Infof("verifyUid returned error: %v", err)
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func ExtractBlankUIDs(ctx context.Context, gmuList []*dql.Mutation) (map[string]uint64, error) {
	newUids := make(map[string]uint64)
	var err error
	for _, gmu := range gmuList {
		for _, nq := range gmu.Set {
			// We dont want to assign uids to these.
			if nq.Subject == x.Star && nq.ObjectValue.GetDefaultVal() == x.Star {
				return newUids, errors.New("predicate deletion should be called via alter")
			}

			if len(nq.Subject) == 0 {
				return nil, errors.Errorf("subject must not be empty for nquad: %+v", nq)
			}
			var uid uint64
			if strings.HasPrefix(nq.Subject, "_:") {
				newUids[nq.Subject] = 0
			} else if uid, err = dql.ParseUid(nq.Subject); err != nil {
				return newUids, err
			}
			if err = verifyUid(ctx, uid); err != nil {
				return newUids, err
			}

			if len(nq.ObjectId) > 0 {
				var uid uint64
				if strings.HasPrefix(nq.ObjectId, "_:") {
					newUids[nq.ObjectId] = 0
				} else if uid, err = dql.ParseUid(nq.ObjectId); err != nil {
					return newUids, err
				}
				if err = verifyUid(ctx, uid); err != nil {
					return newUids, err
				}
			}
		}
	}

	return newUids, nil
}

// AssignUids tries to assign unique ids to each identity in the subjects and objects in the
// format of _:xxx. An identity, e.g. _:a, will only be assigned one uid regardless how many times
// it shows up in the subjects or objects
func AssignUids(ctx context.Context, gmuList []*dql.Mutation) (map[string]uint64, error) {
	newUids, err := ExtractBlankUIDs(ctx, gmuList)
	if err != nil {
		return newUids, err
	}

	num := &pb.Num{Val: uint64(len(newUids)), Type: pb.Num_UID}
	if int(num.Val) > 0 {
		var res *pb.AssignedIds
		// TODO: Optimize later by prefetching. Also consolidate all the UID requests into a single
		// pending request from this server to zero.
		if res, err = worker.AssignUidsOverNetwork(ctx, num); err != nil {
			return newUids, err
		}
		curId := res.StartId
		// assign generated ones now
		for k := range newUids {
			x.AssertTruef(curId != 0 && curId <= res.EndId, "not enough uids generated")
			newUids[k] = curId
			curId++
		}
	}
	return newUids, nil
}

// ToDirectedEdges converts the dql.Mutation input into a set of directed edges.
func ToDirectedEdges(gmuList []*dql.Mutation, newUids map[string]uint64) (
	edges []*pb.DirectedEdge, err error) {

	// Wrapper for a pointer to protos.Nquad
	var wnq *dql.NQuad

	parse := func(nq *api.NQuad, op pb.DirectedEdge_Op) error {
		wnq = &dql.NQuad{NQuad: nq}
		if len(nq.Subject) == 0 {
			return nil
		}
		// Get edge from nquad using newUids.
		var edge *pb.DirectedEdge
		edge, err = wnq.ToEdgeUsing(newUids)
		if err != nil {
			return errors.Wrap(err, "")
		}
		edge.Op = op
		edges = append(edges, edge)
		return nil
	}

	for _, gmu := range gmuList {
		// We delete first and then we set. Order of the mutation is important.
		for _, nq := range gmu.Del {
			if nq.Subject == x.Star && nq.ObjectValue.GetDefaultVal() == x.Star {
				return edges, errors.New("Predicate deletion should be called via alter")
			}
			if err := parse(nq, pb.DirectedEdge_DEL); err != nil {
				return edges, err
			}
			if gmu.AllowedPreds != nil {
				for _, e := range edges {
					e.AllowedPreds = gmu.AllowedPreds
				}
			}
		}
		for _, nq := range gmu.Set {
			if err := facets.SortAndValidate(nq.Facets); err != nil {
				return edges, err
			}
			if err := parse(nq, pb.DirectedEdge_SET); err != nil {
				return edges, err
			}
		}
	}

	return edges, nil
}

func checkIfDeletingAclOperation(ctx context.Context, edges []*pb.DirectedEdge) error {
	// Don't need to make any checks if ACL is not enabled
	if !x.WorkerConfig.AclEnabled {
		return nil
	}
	namespace, err := x.ExtractNamespace(ctx)
	if err != nil {
		return errors.Wrapf(err, "While checking ACL delete operation")
	}

	// If the guardian or groot node is not present, then the request cannot be a delete operation
	// on guardian or groot node.
	guardianUid, ok := x.GuardiansUid.Load(namespace)
	if !ok {
		return nil
	}
	grootsUid, ok := x.GrootUid.Load(namespace)
	if !ok {
		return nil
	}

	isDeleteAclOperation := false
	for _, edge := range edges {
		if !x.IsReservedPredicate(edge.Attr) {
			continue
		}
		// Disallow deleting of guardians group
		if edge.Entity == guardianUid && edge.Op == pb.DirectedEdge_DEL {
			isDeleteAclOperation = true
			break
		}
		// Disallow deleting of groot user
		if edge.Entity == grootsUid && edge.Op == pb.DirectedEdge_DEL {
			isDeleteAclOperation = true
			break
		}
	}
	if isDeleteAclOperation {
		return errors.Errorf("Reserved Properties of guardians group and groot user cannot be deleted.")
	}
	return nil
}
