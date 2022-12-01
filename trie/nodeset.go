// Copyright 2022 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package trie

import (
	"fmt"
	"github.com/ethereum/go-ethereum/crypto"
	"reflect"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// memoryNode is all the information we know about a single cached trie node
// in the memory.
type memoryNode struct {
	hash common.Hash // Node hash, computed by hashing rlp value, empty for deleted nodes
	size uint16      // Byte size of the useful cached data, 0 for deleted nodes
	node node        // Cached collapsed trie node, or raw rlp data, nil for deleted nodes
}

// memoryNodeSize is the raw size of a memoryNode data structure without any
// node data included. It's an approximate size, but should be a lot better
// than not counting them.
var memoryNodeSize = int(reflect.TypeOf(memoryNode{}).Size())

// memorySize returns the total memory size used by this node.
func (n *memoryNode) memorySize(pathlen int) int {
	return int(n.size) + memoryNodeSize + pathlen
}

// rlp returns the raw rlp encoded blob of the cached trie node, either directly
// from the cache, or by regenerating it from the collapsed node.
func (n *memoryNode) rlp() []byte {
	if node, ok := n.node.(rawNode); ok {
		return node
	}
	return nodeToBytes(n.node)
}

// obj returns the decoded and expanded trie node, either directly from the cache,
// or by regenerating it from the rlp encoded blob.
func (n *memoryNode) obj() node {
	if node, ok := n.node.(rawNode); ok {
		return mustDecodeNode(n.hash[:], node)
	}
	return expandNode(n.hash[:], n.node)
}

// isDeleted returns the indicator if the node is marked as deleted.
func (n *memoryNode) isDeleted() bool {
	return n.hash == (common.Hash{})
}

// nodeWithPrev wraps the memoryNode with the previous node value.
type nodeWithPrev struct {
	*memoryNode
	prev []byte // RLP-encoded previous value, nil means it's non-existent
}

// unwrap returns the internal memoryNode object.
func (n *nodeWithPrev) unwrap() *memoryNode {
	return n.memoryNode
}

// memorySize returns the total memory size used by this node. It overloads
// the function in memoryNode by counting the size of previous value as well.
func (n *nodeWithPrev) memorySize(pathlen int) int {
	return n.memoryNode.memorySize(pathlen) + len(n.prev)
}

// NodeSet contains all dirty nodes collected during the commit operation.
// Each node is keyed by path. It's not thread-safe to use.
type NodeSet struct {
	owner  common.Hash              // the identifier of the trie
	nodes  map[string]*nodeWithPrev // the set of dirty nodes(inserted, updated, deleted)
	leaves []*leaf                  // the list of dirty leaves
}

// NewNodeSet initializes an empty node set to be used for tracking dirty nodes
// for a specific account or storage trie. The owner is zero for the account trie
// and the owning account address hash for storage tries.
func NewNodeSet(owner common.Hash) *NodeSet {
	return &NodeSet{
		owner: owner,
		nodes: make(map[string]*nodeWithPrev),
	}
}

/*
// NewNodeSetWithDeletion initializes the nodeset with provided deletion set.
func NewNodeSetWithDeletion(owner common.Hash, paths [][]byte, prev [][]byte) *NodeSet {
	set := NewNodeSet(owner)
	for i, path := range paths {
		set.markDeleted(path, prev[i])
	}
	return set
}
*/

// forEachWithOrder iterates the dirty nodes with the specified order.
// If topToBottom is true:
//
//	then the order of iteration is top to bottom, left to right.
//
// If topToBottom is false:
//
//	then the order of iteration is bottom to top, right to left.
func (set *NodeSet) forEachWithOrder(topToBottom bool, callback func(path string, n *nodeWithPrev)) {
	var paths sort.StringSlice
	for path := range set.nodes {
		paths = append(paths, path)
	}
	if topToBottom {
		paths.Sort()
	} else {
		sort.Sort(sort.Reverse(paths))
	}
	for _, path := range paths {
		callback(path, set.nodes[path])
	}
}

// markUpdated marks the node as dirty(newly-inserted or updated) with provided
// node path, node object along with its previous value.
func (set *NodeSet) markUpdated(path []byte, node *memoryNode, prev []byte) {
	set.nodes[string(path)] = &nodeWithPrev{
		memoryNode: node,
		prev:       prev,
	}
}

// markDeleted marks the node as deleted with provided path and previous value.
func (set *NodeSet) markDeleted(path []byte, prev []byte) {
	set.nodes[string(path)] = &nodeWithPrev{
		memoryNode: &memoryNode{},
		prev:       prev,
	}
}

// addLeaf collects the provided leaf node into set.
func (set *NodeSet) addLeaf(node *leaf) {
	set.leaves = append(set.leaves, node)
}

// Size returns the number of dirty nodes contained in the set.
func (set *NodeSet) Size() int {
	return len(set.nodes)
}

// Hashes returns the hashes of all updated nodes. TODO(rjl493456442) how can
// we get rid of it?
func (set *NodeSet) Hashes() []common.Hash {
	var ret []common.Hash
	for _, node := range set.nodes {
		ret = append(ret, node.hash)
	}
	return ret
}

// Summary returns a string-representation of the NodeSet.
func (set *NodeSet) Summary() string {
	var out = new(strings.Builder)
	fmt.Fprintf(out, "nodeset owner: %v\n", set.owner)
	if set.nodes != nil {
		for path, n := range set.nodes {
			if n.isDeleted() { // deletion
				fmt.Fprintf(out, "  [-]: %x -> %x\n", path, n.prev)
				continue
			}
			if n.prev != nil { // update
				fmt.Fprintf(out, "  [*]: %x -> %v prev: %x\n", path, n.hash, n.prev)
			} else { // insertion
				fmt.Fprintf(out, "  [+]: %x -> %v\n", path, n.hash)
			}
		}
	}
	for _, n := range set.leaves {
		fmt.Fprintf(out, "[leaf]: %v\n", n)
	}
	return out.String()
}

// forEachTipNode iterates the outermost nodes with the order from left to right.
func forEachTipNode(nodes map[string]*nodeWithPrev, callback func(path string, n *nodeWithPrev) error) error {
	// Sort node paths according to lexicographical order,
	// from top to bottom, from left to right.
	var paths sort.StringSlice
	for path := range nodes {
		paths = append(paths, path)
	}
	paths.Sort()

	// Find out the tips nodes according to the path.
	var (
		stack []string
		tips  []string
	)
	for _, path := range paths {
		stack = append(stack, path)
		if len(stack) == 1 {
			continue
		}
		prev, cur := stack[0], stack[1]
		if !strings.HasPrefix(cur, prev) {
			tips = append(tips, prev)
		}
		stack = stack[1:]
	}
	if len(stack) == 1 {
		tips = append(tips, stack[0])
	}
	for _, path := range tips {
		err := callback(path, nodes[path])
		if err != nil {
			return err
		}
	}
	return nil
}

func resolve(prefix []byte, n node, callback func(path []byte, blob []byte)) error {
	switch nn := n.(type) {
	case *shortNode:
		if !hasTerm(nn.Key) {
			return fmt.Errorf("invalid tipnode %v", nn.Key)
		}
		path := append(prefix, nn.Key...)
		callback(hexToKeybytes(path), nn.Val.(valueNode))
	case *fullNode:
		for i, cn := range nn.Children[:16] {
			if cn == nil {
				continue
			}
			resolve(append(prefix, byte(i)), cn, callback)
		}
	default:
		// hashNode might be possible to occur in the tip node.
		// The scenario is: fullnode has hashnode children and
		// a few embedded children, only the embedded children
		// is our target.
	}
	return nil
}

func resolvePrevLeaves(nodes map[string]*nodeWithPrev, callback func(path []byte, blob []byte)) error {
	return forEachTipNode(nodes, func(prefix string, tip *nodeWithPrev) error {
		if len(tip.prev) == 0 {
			return nil
		}
		n := mustDecodeNodeUnsafe(crypto.Keccak256(tip.prev), tip.prev)
		return resolve([]byte(prefix), n, callback)
	})
}

// MergedNodeSet represents a merged dirty node set for a group of tries.
type MergedNodeSet struct {
	sets map[common.Hash]*NodeSet
}

// NewMergedNodeSet initializes an empty merged set.
func NewMergedNodeSet() *MergedNodeSet {
	return &MergedNodeSet{sets: make(map[common.Hash]*NodeSet)}
}

// NewWithNodeSet constructs a merged nodeset with the provided single set.
func NewWithNodeSet(set *NodeSet) *MergedNodeSet {
	merged := NewMergedNodeSet()
	merged.Merge(set)
	return merged
}

// Merge merges the provided dirty nodes of a trie into the set. The assumption
// is held that no duplicated set belonging to the same trie will be merged twice.
func (set *MergedNodeSet) Merge(other *NodeSet) error {
	_, present := set.sets[other.owner]
	if present {
		return fmt.Errorf("duplicate trie for owner %#x", other.owner)
	}
	set.sets[other.owner] = other
	return nil
}

// simplify converts the set to a two-dimensional map in which nodes are mapped
// by owner and path.
func (set *MergedNodeSet) simplify() map[common.Hash]map[string]*nodeWithPrev {
	nodes := make(map[common.Hash]map[string]*nodeWithPrev)
	for owner, subset := range set.sets {
		nodes[owner] = subset.nodes
	}
	return nodes
}
