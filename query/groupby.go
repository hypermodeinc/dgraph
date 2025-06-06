/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/pkg/errors"

	"github.com/hypermodeinc/dgraph/v25/algo"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/types"
)

type groupPair struct {
	key  types.Val
	attr string
}

type groupResult struct {
	keys       []groupPair
	aggregates []groupPair
	uids       []uint64
}

func (grp *groupResult) aggregateChild(child *SubGraph) error {
	fieldName := child.Params.Alias
	if child.Params.DoCount {
		if child.Attr != "uid" {
			return errors.Errorf("Only uid predicate is allowed in count within groupby")
		}
		if fieldName == "" {
			fieldName = "count"
		}
		grp.aggregates = append(grp.aggregates, groupPair{
			attr: fieldName,
			key: types.Val{
				Tid:   types.IntID,
				Value: int64(len(grp.uids)),
			},
		})
		return nil
	}
	if child.SrcFunc != nil && isAggregatorFn(child.SrcFunc.Name) {
		if fieldName == "" {
			fieldName = fmt.Sprintf("%s(%s)", child.SrcFunc.Name, child.Attr)
		}
		finalVal, err := aggregateGroup(grp, child)
		if err != nil {
			return err
		}
		grp.aggregates = append(grp.aggregates, groupPair{
			attr: fieldName,
			key:  finalVal,
		})
	}
	return nil
}

type groupResults struct {
	group []*groupResult
}

type groupElements struct {
	entities *pb.List
	key      types.Val
}

type uniq struct {
	elements map[string]groupElements
	attr     string
}

type dedup struct {
	groups []*uniq
}

func (d *dedup) getGroup(attr string) *uniq {
	var res *uniq
	// Looping last to first is better in this case.
	for i := len(d.groups) - 1; i >= 0; i-- {
		it := d.groups[i]
		if attr == it.attr {
			res = it
			break
		}
	}
	if res == nil {
		// Create a new entry.
		res = &uniq{
			attr:     attr,
			elements: make(map[string]groupElements),
		}
		d.groups = append(d.groups, res)
	}
	return res
}

func (d *dedup) addValue(attr string, value types.Val, uid uint64) {
	cur := d.getGroup(attr)
	// Create the string key.
	var strKey string
	if value.Tid == types.UidID {
		strKey = strconv.FormatUint(value.Value.(uint64), 10)
	} else {
		valC := types.Val{Tid: types.StringID, Value: ""}
		err := types.Marshal(value, &valC)
		if err != nil {
			return
		}
		strKey = valC.Value.(string)
	}

	if _, ok := cur.elements[strKey]; !ok {
		// If this is the first element of the group.
		cur.elements[strKey] = groupElements{
			key:      value,
			entities: &pb.List{Uids: []uint64{}},
		}
	}
	curEntity := cur.elements[strKey].entities
	curEntity.Uids = append(curEntity.Uids, uid)
}

func aggregateGroup(grp *groupResult, child *SubGraph) (types.Val, error) {
	ag := aggregator{
		name: child.SrcFunc.Name,
	}
	for _, uid := range grp.uids {
		idx := sort.Search(len(child.SrcUIDs.Uids), func(i int) bool {
			return child.SrcUIDs.Uids[i] >= uid
		})
		if idx == len(child.SrcUIDs.Uids) || child.SrcUIDs.Uids[idx] != uid {
			continue
		}

		if len(child.valueMatrix[idx].Values) == 0 {
			continue
		}
		v := child.valueMatrix[idx].Values[0]
		val, err := convertWithBestEffort(v, child.Attr)
		if err != nil {
			continue
		}
		if err := ag.Apply(val); err != nil {
			return types.Val{}, err
		}
	}
	return ag.Value()
}

// formGroup creates all possible groups with the list of uids that belong to that
// group.
func (res *groupResults) formGroups(dedupMap dedup, cur *pb.List, groupVal []groupPair) {
	l := len(groupVal)
	if len(dedupMap.groups) == 0 || (l != 0 && len(cur.Uids) == 0) {
		// This group is already empty or no group can be formed. So stop.
		return
	}

	if l == len(dedupMap.groups) {
		a := make([]uint64, len(cur.Uids))
		b := make([]groupPair, len(groupVal))
		copy(a, cur.Uids)
		copy(b, groupVal)
		res.group = append(res.group, &groupResult{
			uids: a,
			keys: b,
		})
		return
	}

	for _, v := range dedupMap.groups[l].elements {
		temp := new(pb.List)
		groupVal = append(groupVal, groupPair{
			key:  v.key,
			attr: dedupMap.groups[l].attr,
		})
		if l != 0 {
			algo.IntersectWith(cur, v.entities, temp)
		} else {
			temp.Uids = make([]uint64, len(v.entities.Uids))
			copy(temp.Uids, v.entities.Uids)
		}
		res.formGroups(dedupMap, temp, groupVal)
		groupVal = groupVal[:len(groupVal)-1]
	}
}

func (sg *SubGraph) formResult(ul *pb.List) (*groupResults, error) {
	var dedupMap dedup
	res := new(groupResults)

	for _, child := range sg.Children {
		if !child.Params.IgnoreResult {
			continue
		}

		attr := child.Params.Alias
		if attr == "" {
			attr = child.Attr
		}
		if len(child.DestUIDs.GetUids()) > 0 {
			// It's a UID node.
			for i := range child.uidMatrix {
				srcUid := child.SrcUIDs.Uids[i]
				// Ignore uids which are not part of srcUid.
				if algo.IndexOf(ul, srcUid) < 0 {
					continue
				}

				ul := child.uidMatrix[i]
				for _, uid := range ul.GetUids() {
					dedupMap.addValue(attr, types.Val{Tid: types.UidID, Value: uid}, srcUid)
				}
			}
		} else {
			// It's a value node.
			for i, v := range child.valueMatrix {
				srcUid := child.SrcUIDs.Uids[i]
				if len(v.Values) == 0 || algo.IndexOf(ul, srcUid) < 0 {
					continue
				}
				val, err := convertTo(v.Values[0])
				if err != nil {
					continue
				}
				dedupMap.addValue(attr, val, srcUid)
			}
		}
	}

	// Create all the groups here.
	res.formGroups(dedupMap, &pb.List{}, []groupPair{})

	// Go over the groups and aggregate the values.
	for _, child := range sg.Children {
		if child.Params.IgnoreResult {
			continue
		}
		// This is a aggregation node.
		for _, grp := range res.group {
			err := grp.aggregateChild(child)
			if err != nil && err != ErrEmptyVal {
				return res, err
			}
		}
	}
	// Sort to order the groups for determinism.
	sort.Slice(res.group, func(i, j int) bool {
		return groupLess(res.group[i], res.group[j])
	})

	return res, nil
}

// This function is to use the fillVars. It is similar to formResult, the only difference being
// that it considers the whole uidMatrix to do the grouping before assigning the variable.
// TODO - Check if we can reduce this duplication.
func (sg *SubGraph) fillGroupedVars(doneVars map[string]varValue, path []*SubGraph) error {
	var childHasVar bool
	for _, child := range sg.Children {
		if child.Params.Var != "" {
			childHasVar = true
			break
		}
	}

	if !childHasVar {
		return nil
	}

	var pathNode *SubGraph
	var dedupMap dedup

	for _, child := range sg.Children {
		if !child.Params.IgnoreResult {
			continue
		}

		attr := child.Params.Alias
		if attr == "" {
			attr = child.Attr
		}
		if len(child.DestUIDs.GetUids()) > 0 {
			// It's a UID node.
			for i := range child.uidMatrix {
				srcUid := child.SrcUIDs.Uids[i]
				ul := child.uidMatrix[i]
				for _, uid := range ul.Uids {
					dedupMap.addValue(attr, types.Val{Tid: types.UidID, Value: uid}, srcUid)
				}
			}
			pathNode = child
		} else {
			// It's a value node.
			for i, v := range child.valueMatrix {
				srcUid := child.SrcUIDs.Uids[i]
				if len(v.Values) == 0 {
					continue
				}
				val, err := convertTo(v.Values[0])
				if err != nil {
					continue
				}
				dedupMap.addValue(attr, val, srcUid)
			}
		}
	}

	// Create all the groups here.
	res := new(groupResults)
	res.formGroups(dedupMap, &pb.List{}, []groupPair{})

	// Go over the groups and aggregate the values.
	for _, child := range sg.Children {
		if child.Params.IgnoreResult {
			continue
		}
		// This is a aggregation node.
		for _, grp := range res.group {
			err := grp.aggregateChild(child)
			if err != nil && err != ErrEmptyVal {
				return err
			}
		}
		if child.Params.Var == "" {
			continue
		}
		chVar := child.Params.Var

		tempMap := types.NewShardedMap()
		for _, grp := range res.group {
			if len(grp.keys) == 0 {
				continue
			}
			if len(grp.keys) > 1 {
				return errors.Errorf("Expected one UID for var in groupby but got: %d", len(grp.keys))
			}
			uidVal := grp.keys[0].key.Value
			uid, ok := uidVal.(uint64)
			if !ok {
				return errors.Errorf("Vars can be assigned only when grouped by UID attribute")
			}
			// grp.aggregates could be empty if schema conversion failed during aggregation
			if len(grp.aggregates) > 0 {
				tempMap.Set(uid, grp.aggregates[len(grp.aggregates)-1].key)
			}
		}
		doneVars[chVar] = varValue{
			Vals: tempMap,
			path: append(path, pathNode),
		}
	}
	return nil
}

func (sg *SubGraph) processGroupBy(doneVars map[string]varValue, path []*SubGraph) error {
	for _, ul := range sg.uidMatrix {
		// We need to process groupby for each list as grouping needs to happen for each path of the
		// tree.

		r, err := sg.formResult(ul)
		if err != nil {
			return err
		}
		sg.GroupbyRes = append(sg.GroupbyRes, r)
	}

	if err := sg.fillGroupedVars(doneVars, path); err != nil {
		return err
	}

	// All the result that we want to return is in sg.GroupbyRes
	sg.Children = sg.Children[:0]

	return nil
}

func groupLess(a, b *groupResult) bool {
	switch {
	case len(a.uids) < len(b.uids):
		return true
	case len(a.uids) != len(b.uids):
		return false
	}
	switch {
	case len(a.keys) < len(b.keys):
		return true
	case len(a.keys) != len(b.keys):
		return false
	}
	switch {
	case len(a.aggregates) < len(b.aggregates):
		return true
	case len(a.aggregates) != len(b.aggregates):
		return false
	}

	for i := range a.keys {
		l, err := types.Less(a.keys[i].key, b.keys[i].key)
		if err == nil {
			if l {
				return l
			}
			l, _ = types.Less(b.keys[i].key, a.keys[i].key)
			if l {
				return !l
			}
		}
	}

	for i := range a.aggregates {
		if l, err := types.Less(a.aggregates[i].key, b.aggregates[i].key); err == nil {
			if l {
				return l
			}
			l, _ = types.Less(b.aggregates[i].key, a.aggregates[i].key)
			if l {
				return !l
			}
		}
	}
	return false
}
