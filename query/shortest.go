/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"container/heap"
	"context"
	"math"
	"sync"

	"github.com/pkg/errors"

	"github.com/hypermodeinc/dgraph/v25/algo"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/types/facets"
	"github.com/hypermodeinc/dgraph/v25/x"
)

type pathInfo struct {
	uid   uint64
	attr  string
	facet *pb.Facets
}

type route struct {
	route       *[]pathInfo
	totalWeight float64
}

type queueItem struct {
	uid  uint64  // uid of the node.
	cost float64 // cost of taking the path till this uid.
	// number of hops taken to reach this node. This is useful in finding out if we need to
	// expandOut after poping an element from the heap. We only expandOut if item.hop > numHops
	// otherwise expanding would be useless.
	hop   int
	index int
	path  route // used in k shortest path.
}

var pathPool = sync.Pool{
	New: func() interface{} {
		return &[]pathInfo{}
	},
}

var errStop = errors.Errorf("STOP")
var errFacet = errors.Errorf("Skip the edge")

type priorityQueue []*queueItem

func (r *route) indexOf(uid uint64) int {
	for i, val := range *r.route {
		if val.uid == uid {
			return i
		}
	}
	return -1
}

func (h priorityQueue) Len() int { return len(h) }

func (h priorityQueue) Less(i, j int) bool { return h[i].cost < h[j].cost }

func (h priorityQueue) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *priorityQueue) Push(val interface{}) {
	n := len(*h)
	item := val.(*queueItem)
	item.index = n
	*h = append(*h, item)
}

func (h *priorityQueue) Pop() interface{} {
	old := *h
	n := len(old)
	val := old[n-1]
	*h = old[0 : n-1]
	val.index = -1
	return val
}

type mapItem struct {
	attr  string
	cost  float64
	facet *pb.Facets
}

// We manintain a map from UID to nodeInfo for Djikstras.
type nodeInfo struct {
	mapItem
	parent uint64
	// Pointer to the item in heap. Used to update priority
	node *queueItem
}

func (sg *SubGraph) getCost(matrix, list int) (cost float64,
	fcs *pb.Facets, rerr error) {

	cost = 1.0
	if len(sg.facetsMatrix) <= matrix {
		return cost, fcs, rerr
	}
	fcsList := sg.facetsMatrix[matrix].FacetsList
	if len(fcsList) <= list {
		rerr = errFacet
		return cost, fcs, rerr
	}
	fcs = fcsList[list]
	if len(fcs.Facets) == 0 {
		rerr = errFacet
		return cost, fcs, rerr
	}
	if len(fcs.Facets) > 1 {
		rerr = errors.Errorf("Expected 1 but got %d facets", len(fcs.Facets))
		return cost, fcs, rerr
	}
	tv, err := facets.ValFor(fcs.Facets[0])
	if err != nil {
		return 0.0, nil, err
	}
	switch {
	case tv.Tid == types.IntID:
		cost = float64(tv.Value.(int64))
	case tv.Tid == types.FloatID:
		cost = tv.Value.(float64)
	default:
		rerr = errFacet
	}
	return cost, fcs, rerr
}

func (sg *SubGraph) expandOut(ctx context.Context,
	adjacencyMap map[uint64]map[uint64]mapItem, next chan bool, rch chan error) {

	var numEdges uint64
	var exec []*SubGraph
	var err error
	in := []uint64{sg.Params.From}
	sg.SrcUIDs = &pb.List{Uids: in}
	sg.uidMatrix = []*pb.List{{Uids: in}}
	sg.DestUIDs = sg.SrcUIDs

	for _, child := range sg.Children {
		child.SrcUIDs = sg.DestUIDs
		exec = append(exec, child)
	}
	dummy := &SubGraph{}
	for {
		isNext := <-next
		if !isNext {
			return
		}
		rrch := make(chan error, len(exec))
		for _, subgraph := range exec {
			go ProcessGraph(ctx, subgraph, dummy, rrch)
		}

		for range exec {
			select {
			case err = <-rrch:
				if err != nil {
					rch <- err
					return
				}
			case <-ctx.Done():
				rch <- ctx.Err()
				return
			}
		}

		for _, subgraph := range exec {
			select {
			case <-ctx.Done():
				rch <- ctx.Err()
				return
			default:
				if subgraph.UnknownAttr {
					continue
				}

				// Call updateUidMatrix to ensure that entries in the uidMatrix are updated after
				// intersecting with DestUIDs. This should ideally be called during query
				// processing but doesn't seem to be called for shortest path queries. So we call
				// it explicitly here to ensure the results are correct.
				subgraph.updateUidMatrix()
				// Send the destuids in res chan.
				for mIdx, fromUID := range subgraph.SrcUIDs.Uids {
					// This can happen when trying to go traverse a predicate of type password
					// for example.
					if mIdx >= len(subgraph.uidMatrix) {
						continue
					}

					for lIdx, toUID := range subgraph.uidMatrix[mIdx].Uids {
						if adjacencyMap[fromUID] == nil {
							adjacencyMap[fromUID] = make(map[uint64]mapItem)
						}
						// The default cost we'd use is 1.
						cost, facet, err := subgraph.getCost(mIdx, lIdx)
						switch {
						case err == errFacet:
							// Ignore the edge and continue.
							continue
						case err != nil:
							rch <- err
							return
						}

						// TODO - This simplify overrides the adjacency matrix. What happens if the
						// cost along the second attribute is more than that along the first.
						adjacencyMap[fromUID][toUID] = mapItem{
							cost:  cost,
							facet: facet,
							attr:  subgraph.Attr,
						}
						numEdges++
					}
				}
			}
		}

		if numEdges > x.Config.LimitQueryEdge {
			// If we've seen too many edges, stop the query.
			rch <- errors.Errorf("Exceeded query edge limit = %v. Found %v edges.",
				x.Config.LimitMutationsNquad, numEdges)
			return
		}

		// modify the exec and attach child nodes.
		var out []*SubGraph
		for _, subgraph := range exec {
			if len(subgraph.DestUIDs.Uids) == 0 {
				continue
			}
			select {
			case <-ctx.Done():
				rch <- ctx.Err()
				return
			default:
				for _, child := range sg.Children {
					temp := new(SubGraph)
					temp.copyFiltersRecurse(child)

					temp.SrcUIDs = subgraph.DestUIDs
					// Remove those nodes which we have already traversed. As this cannot be
					// in the path again.
					algo.ApplyFilter(temp.SrcUIDs, func(uid uint64, i int) bool {
						_, ok := adjacencyMap[uid]
						return !ok
					})
					subgraph.Children = append(subgraph.Children, temp)
					out = append(out, temp)
				}
			}
		}

		if len(out) == 0 {
			rch <- errStop
			return
		}
		rch <- nil
		exec = out
	}
}

func (sg *SubGraph) copyFiltersRecurse(otherSubgraph *SubGraph) {
	*sg = *otherSubgraph
	sg.Children = []*SubGraph{}
	sg.Filters = []*SubGraph{}
	for _, fc := range otherSubgraph.Filters {
		tempChild := new(SubGraph)
		tempChild.copyFiltersRecurse(fc)
		sg.Filters = append(sg.Filters, tempChild)
	}
}

func runKShortestPaths(ctx context.Context, sg *SubGraph) ([]*SubGraph, error) {
	var err error
	if sg.Params.Alias != "shortest" {
		return nil, errors.Errorf("Invalid shortest path query")
	}

	numPaths := sg.Params.NumPaths
	var kroutes []route
	pq := make(priorityQueue, 0)

	// Initialize and push the source node.
	srcNode := &queueItem{
		uid:  sg.Params.From,
		cost: 0,
		hop:  0,
		path: route{route: &[]pathInfo{{uid: sg.Params.From}}},
	}
	heap.Push(&pq, srcNode)

	numHops := 0
	maxHops := math.MaxInt32
	if sg.Params.ExploreDepth != nil {
		maxHops = int(*sg.Params.ExploreDepth)
	}
	if maxHops == 0 {
		return nil, nil
	}

	minWeight := sg.Params.MinWeight
	maxWeight := sg.Params.MaxWeight
	next := make(chan bool, 2)
	expandErr := make(chan error, 2)
	adjacencyMap := make(map[uint64]map[uint64]mapItem)
	go sg.expandOut(ctx, adjacencyMap, next, expandErr)

	// In k shortest path we can't have this. We store the path till a node in every
	// node.
	// map to store the min cost and parent of nodes.
	var stopExpansion bool
	for pq.Len() > 0 {
		item := heap.Pop(&pq).(*queueItem)
		if item.uid == sg.Params.To {
			// Ignore paths that do not meet the minimum weight requirement.
			if item.cost < minWeight {
				continue
			}

			// Add path to list after making a copy of the path in itemRoute. A copy of
			// *item.path.route is required because it has to be put back in the sync pool and a
			// future reuse can alter the item already present in kroute because it is a pointer.
			itemRoute := make([]pathInfo, len(*item.path.route))
			copy(itemRoute, *item.path.route)
			newRoute := item.path
			newRoute.route = &itemRoute
			newRoute.totalWeight = item.cost
			kroutes = append(kroutes, newRoute)
			if len(kroutes) == numPaths {
				// We found the required number of paths.
				break
			}
		}
		if item.hop > numHops-1 && numHops < maxHops {
			// Explore the next level by calling processGraph and add them
			// to the queue.
			if !stopExpansion {
				next <- true
				select {
				case err = <-expandErr:
					if err != nil {
						if err == errStop {
							stopExpansion = true
						} else {
							return nil, err
						}
					}
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				numHops++
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		neighbours := adjacencyMap[item.uid]
		for toUid, info := range neighbours {
			cost := info.cost
			// Skip neighbour if the cost is greater than the maximum weight allowed.
			if item.cost+cost > maxWeight {
				continue
			}
			// Skip neighbour if it present in current path to remove cyclical paths
			if len(*item.path.route) > 0 && item.path.indexOf(toUid) != -1 {
				continue
			}
			curPath := pathPool.Get().(*[]pathInfo)
			if curPath == nil {
				return nil, errors.Errorf("Sync pool returned a nil pointer")
			}
			if cap(*curPath) < len(*item.path.route)+1 {
				// We can't use it due to insufficient capacity. Put it back.
				pathPool.Put(curPath)
				newSlice := make([]pathInfo, len(*item.path.route)+1)
				curPath = &newSlice
			} else {
				// Use the curPath from pathPool. Set length appropriately.
				*curPath = (*curPath)[:len(*item.path.route)+1]
			}
			n := copy(*curPath, *item.path.route)
			(*curPath)[n] = pathInfo{
				uid:   toUid,
				attr:  info.attr,
				facet: info.facet,
			}
			node := &queueItem{
				uid:  toUid,
				cost: item.cost + cost,
				hop:  item.hop + 1,
				path: route{route: curPath},
			}
			if pq.Len() > sg.Params.MaxHeapSize {
				pq.Pop()
			}
			heap.Push(&pq, node)
		}
		// Return the popped nodes path to pool.
		pathPool.Put(item.path.route)
	}

	next <- false

	if len(kroutes) == 0 {
		sg.DestUIDs = &pb.List{}
		return nil, nil
	}
	var res []uint64
	for _, it := range *kroutes[0].route {
		res = append(res, it.uid)
	}
	sg.DestUIDs.Uids = res
	shortestSg := createkroutesubgraph(ctx, kroutes)
	return shortestSg, nil
}

// Djikstras algorithm pseudocode for reference.
//
// 1  function Dijkstra(Graph, source):
// 2      dist[source] ← 0                                    // Initialization
// 3
// 4      create vertex set Q
// 5
// 6      for each vertex v in Graph:
// 7          if v ≠ source
// 8              dist[v] ← INFINITY                          // Unknown distance from source to v
// 9              prev[v] ← UNDEFINED                         // Predecessor of v
// 10
// 11         Q.add_with_priority(v, dist[v])
// 12
// 13
// 14     while Q is not empty:                              // The main loop
// 15         u ← Q.extract_min()                            // Remove and return best vertex
// 16         for each neighbor v of u:                       // only v that is still in Q
// 17             alt = dist[u] + length(u, v)
// 18             if alt < dist[v]
// 19                 dist[v] ← alt
// 20                 prev[v] ← u
// 21                 Q.decrease_priority(v, alt)
// 22
// 23     return dist[], prev[]
func shortestPath(ctx context.Context, sg *SubGraph) ([]*SubGraph, error) {
	var err error
	if sg.Params.Alias != "shortest" {
		return nil, errors.Errorf("Invalid shortest path query")
	}
	if sg.Params.From == 0 || sg.Params.To == 0 {
		return nil, nil
	}
	numPaths := sg.Params.NumPaths
	if numPaths == 0 {
		// Return 1 path by default.
		numPaths = 1
	}

	if numPaths > 1 {
		return runKShortestPaths(ctx, sg)
	}
	pq := make(priorityQueue, 0)

	// Initialize and push the source node.
	srcNode := &queueItem{
		uid:  sg.Params.From,
		cost: 0,
		hop:  0,
	}
	heap.Push(&pq, srcNode)

	numHops := 0
	maxHops := math.MaxInt32
	if sg.Params.ExploreDepth != nil {
		maxHops = int(*sg.Params.ExploreDepth)
	}
	if maxHops == 0 {
		return nil, nil
	}

	// next is a channel on to which we send a signal so as to perform another level of expansion.
	next := make(chan bool, 2)
	expandErr := make(chan error, 2)
	adjacencyMap := make(map[uint64]map[uint64]mapItem)
	// TODO - Check if this goroutine actually improves performance. It doesn't look like it
	// because we need to fill the adjacency map before we can make progress.
	go sg.expandOut(ctx, adjacencyMap, next, expandErr)

	// map to store the min cost and parent of nodes.
	dist := make(map[uint64]nodeInfo)
	dist[srcNode.uid] = nodeInfo{
		parent: 0,
		node:   srcNode,
		mapItem: mapItem{
			cost: 0,
		},
	}

	var stopExpansion bool
	var totalWeight float64

	// We continue to pop from the priority queue either
	// 1. Till we get the destination node in which case we would have gotten to it through the
	//    shortest path.
	// 2. We have expanded maxHops number of times.
	for pq.Len() > 0 {
		item := heap.Pop(&pq).(*queueItem)
		if item.uid == sg.Params.To {
			break
		}

		if numHops < maxHops && item.hop > numHops-1 {
			// Explore the next level by calling processGraph and add them to the queue.
			if !stopExpansion {
				next <- true
				select {
				case err = <-expandErr:
					if err != nil {
						// errStop is returned when ProcessGraph doesn't return any more results
						// and we can't expand anymore.
						if err == errStop {
							stopExpansion = true
						} else {
							return nil, err
						}
					}
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				numHops++
			}
		}

		neighbours := adjacencyMap[item.uid]
		for toUID, neighbour := range neighbours {
			d, ok := dist[toUID]
			// Cost of reaching this neighbour node from srcNode is item.cost + neighbour.cost
			nodeCost := item.cost + neighbour.cost
			if ok && d.cost <= nodeCost {
				continue
			}

			var node *queueItem
			if !ok {
				// This is the first time we're seeing this node. So
				// create a new node and add it to the heap and map.
				node = &queueItem{
					uid:  toUID,
					cost: nodeCost,
					hop:  item.hop + 1,
				}
				if pq.Len() > sg.Params.MaxHeapSize {
					pq.Pop()
				}
				heap.Push(&pq, node)
			} else {
				// We've already seen this node. So, just update the cost
				// and fix the priority in the heap and map.
				node = dist[toUID].node
				node.cost = nodeCost
				node.hop = item.hop + 1
				heap.Fix(&pq, node.index)
			}
			dist[toUID] = nodeInfo{
				parent: item.uid,
				node:   node,
				mapItem: mapItem{
					cost:  nodeCost,
					attr:  neighbour.attr,
					facet: neighbour.facet,
				},
			}
		}
	}

	// Send next as false so that the expandOut goroutine exits.
	next <- false
	// Go through the distance map to find the path.
	var result []uint64
	cur := sg.Params.To
	totalWeight = dist[cur].cost
	// The length of the path can be greater than numHops hence we loop over the dist map till we
	// reach sg.Params.From node. See test TestShortestPathWithDepth/depth_2_numpaths_1
	for range dist {
		result = append(result, cur)
		if cur == sg.Params.From {
			break
		}
		cur = dist[cur].parent
	}
	if cur != sg.Params.From {
		sg.DestUIDs = &pb.List{}
		return nil, nil
	}

	l := len(result)
	// Reverse the list.
	for i := range l / 2 {
		result[i], result[l-i-1] = result[l-i-1], result[i]
	}
	// Put the path in DestUIDs of the root.
	sg.DestUIDs.Uids = result

	shortestSg := createPathSubgraph(ctx, dist, totalWeight, result)
	return []*SubGraph{shortestSg}, nil
}

func createPathSubgraph(ctx context.Context, dist map[uint64]nodeInfo, totalWeight float64,
	result []uint64) *SubGraph {
	shortestSg := new(SubGraph)
	shortestSg.Params = params{
		Alias:    "_path_",
		Shortest: true,
	}
	shortestSg.pathMeta = &pathMetadata{
		weight: totalWeight,
	}
	curUid := result[0]
	shortestSg.SrcUIDs = &pb.List{Uids: []uint64{curUid}}
	shortestSg.DestUIDs = &pb.List{Uids: []uint64{curUid}}
	shortestSg.uidMatrix = []*pb.List{{Uids: []uint64{curUid}}}

	curNode := shortestSg
	for i := range len(result) - 1 {
		curUid := result[i]
		childUid := result[i+1]
		node := new(SubGraph)
		nodeInfo := dist[childUid]
		node.Params = params{
			Shortest: true,
		}
		if nodeInfo.facet != nil {
			// For consistent later processing.
			node.Params.Facet = &pb.FacetParams{}
		}
		node.Attr = nodeInfo.attr
		node.facetsMatrix = []*pb.FacetsList{{FacetsList: []*pb.Facets{nodeInfo.facet}}}
		node.SrcUIDs = &pb.List{Uids: []uint64{curUid}}
		node.DestUIDs = &pb.List{Uids: []uint64{childUid}}
		node.uidMatrix = []*pb.List{{Uids: []uint64{childUid}}}

		curNode.Children = append(curNode.Children, node)
		curNode = node
	}

	node := new(SubGraph)
	node.Params = params{
		Shortest: true,
	}
	uid := result[len(result)-1]
	node.SrcUIDs = &pb.List{Uids: []uint64{uid}}
	node.uidMatrix = []*pb.List{{Uids: []uint64{uid}}}
	curNode.Children = append(curNode.Children, node)

	return shortestSg
}

func createkroutesubgraph(ctx context.Context, kroutes []route) []*SubGraph {
	var res []*SubGraph
	for _, it := range kroutes {
		shortestSg := new(SubGraph)
		shortestSg.Params = params{
			Alias:    "_path_",
			Shortest: true,
		}
		shortestSg.pathMeta = &pathMetadata{
			weight: it.totalWeight,
		}
		curUid := (*it.route)[0].uid
		shortestSg.SrcUIDs = &pb.List{Uids: []uint64{curUid}}
		shortestSg.DestUIDs = &pb.List{Uids: []uint64{curUid}}
		shortestSg.uidMatrix = []*pb.List{{Uids: []uint64{curUid}}}

		curNode := shortestSg
		i := 0
		for ; i < len(*it.route)-1; i++ {
			curUid := (*it.route)[i].uid
			childUid := (*it.route)[i+1].uid
			node := new(SubGraph)
			node.Params = params{
				Shortest: true,
			}
			if (*it.route)[i+1].facet != nil {
				// For consistent later processing.
				node.Params.Facet = &pb.FacetParams{}
			}
			node.Attr = (*it.route)[i+1].attr
			node.facetsMatrix = []*pb.FacetsList{{FacetsList: []*pb.Facets{(*it.route)[i+1].facet}}}
			node.SrcUIDs = &pb.List{Uids: []uint64{curUid}}
			node.DestUIDs = &pb.List{Uids: []uint64{childUid}}
			node.uidMatrix = []*pb.List{{Uids: []uint64{childUid}}}

			curNode.Children = append(curNode.Children, node)
			curNode = node
		}

		node := new(SubGraph)
		node.Params = params{
			Shortest: true,
		}
		uid := (*it.route)[i].uid
		node.SrcUIDs = &pb.List{Uids: []uint64{uid}}
		node.uidMatrix = []*pb.List{{Uids: []uint64{uid}}}
		curNode.Children = append(curNode.Children, node)

		res = append(res, shortestSg)
	}
	return res
}
