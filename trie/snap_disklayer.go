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
	"sync"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

// diskLayer is a low level persistent snapshot built on top of a key-value store.
type diskLayer struct {
	root   common.Hash // Immutable, root hash of the base snapshot
	diffid uint64      // Immutable, corresponding reverse diff id

	diskdb ethdb.Database   // Key-value store containing the base snapshot
	clean  *fastcache.Cache // Clean node cache to avoid hitting the disk for direct access
	dirty  *diskcache       // Dirty node cache to aggregate writes and temporary cache.
	stale  bool             // Signals that the layer became stale (state progressed)
	lock   sync.RWMutex     // Lock used to protect stale flag
}

// newDiskLayer creates a new disk layer based on the passing arguments.
func newDiskLayer(root common.Hash, diffid uint64, clean *fastcache.Cache, dirty *diskcache, diskdb ethdb.Database) *diskLayer {
	return &diskLayer{
		diskdb: diskdb,
		clean:  clean,
		dirty:  dirty,
		root:   root,
		diffid: diffid,
	}
}

// Root returns root hash of corresponding state.
func (dl *diskLayer) Root() common.Hash {
	return dl.root
}

// Parent always returns nil as there's no layer below the disk.
func (dl *diskLayer) Parent() snapshot {
	return nil
}

// ID returns the id of associated reverse diff.
func (dl *diskLayer) ID() uint64 {
	return dl.diffid
}

// Stale return whether this layer has become stale (was flattened across) or if
// it's still live.
func (dl *diskLayer) Stale() bool {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	return dl.stale
}

// MarkStale sets the stale flag as true.
func (dl *diskLayer) MarkStale() {
	dl.lock.Lock()
	defer dl.lock.Unlock()

	if dl.stale {
		panic("triedb disk layer is stale") // we've committed into the same base from two children, boom
	}
	dl.stale = true
}

// node retrieves the node with provided storage key and node hash. The returned
// node is in a wrapper through which callers can obtain the RLP-format or canonical
// node representation easily. No error will be returned if node is not found.
func (dl *diskLayer) node(owner common.Hash, path []byte, hash common.Hash, depth int) (*memoryNode, error) {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	if dl.stale {
		return nil, errSnapshotStale
	}
	// Try to retrieve the trie node from the dirty memory cache.
	// The map is lock free since it's impossible to mutate the
	// disk layer before tagging it as stale.
	n, err := dl.dirty.node(owner, path, hash)
	if err != nil {
		return nil, err
	}
	if n != nil {
		// Hit node in disk cache which resides in disk layer
		triedbDirtyHitMeter.Mark(1)
		triedbDirtyNodeHitDepthHist.Update(int64(depth))
		triedbDirtyReadMeter.Mark(int64(n.size))
		return n, nil
	}
	// If we're in the disk layer, all diff layers missed
	triedbDirtyMissMeter.Mark(1)

	// Try to retrieve the trie node from the clean memory cache
	if dl.clean != nil {
		if blob := dl.clean.Get(nil, hash.Bytes()); len(blob) > 0 {
			triedbCleanHitMeter.Mark(1)
			triedbCleanReadMeter.Mark(int64(len(blob)))
			return &memoryNode{node: rawNode(blob), hash: hash, size: uint16(len(blob))}, nil
		}
		triedbCleanMissMeter.Mark(1)
	}
	// Try to retrieve the trie node from the disk.
	var (
		nBlob []byte
		nHash common.Hash
	)
	if owner == (common.Hash{}) {
		nBlob, nHash = rawdb.ReadAccountTrieNode(dl.diskdb, path)
	} else {
		nBlob, nHash = rawdb.ReadStorageTrieNode(dl.diskdb, owner, path)
	}
	if nHash != hash {
		return nil, fmt.Errorf("%w %x!=%x(%x %v)", errUnexpectedNode, nHash, hash, owner, path)
	}
	if dl.clean != nil && len(nBlob) > 0 {
		dl.clean.Set(hash.Bytes(), nBlob)
		triedbCleanWriteMeter.Mark(int64(len(nBlob)))
	}
	if len(nBlob) == 0 {
		return nil, nil
	}
	return &memoryNode{node: rawNode(nBlob), hash: hash, size: uint16(len(nBlob))}, nil
}

// Node retrieves the trie node with the provided trie identifier, node path
// and the corresponding node hash. No error will be returned if the node is
// not found.
func (dl *diskLayer) Node(owner common.Hash, path []byte, hash common.Hash) (node, error) {
	n, err := dl.node(owner, path, hash, 0)
	if err != nil || n == nil {
		return nil, err
	}
	return n.obj(), nil
}

// NodeBlob retrieves the RLP-encoded trie node blob with the provided trie
// identifier, node path and the corresponding node hash. No error will be
// returned if the node is not found.
func (dl *diskLayer) NodeBlob(owner common.Hash, path []byte, hash common.Hash) ([]byte, error) {
	n, err := dl.node(owner, path, hash, 0)
	if err != nil || n == nil {
		return nil, err
	}
	return n.rlp(), nil
}

// Update returns a new diff layer on top with the given dirty node set.
func (dl *diskLayer) Update(blockHash common.Hash, id uint64, nodes map[common.Hash]map[string]*nodeWithPrev) *diffLayer {
	return newDiffLayer(dl, blockHash, id, nodes)
}

// commit merges the given bottom-most diff layer into the local cache
// and returns a newly constructed disk layer. Note the current disk
// layer must be tagged as stale first to prevent re-access.
func (dl *diskLayer) commit(freezer *rawdb.Freezer, stateHistory *rawdb.Freezer, statelimit uint64, bottom *diffLayer, force bool) (*diskLayer, error) {
	dl.lock.Lock()
	defer dl.lock.Unlock()

	// Mark the diskLayer as stale before applying any mutations on top.
	dl.stale = true

	// Construct and store the reverse diff firstly. If crash happens
	// after storing the reverse diff but without flushing the corresponding
	// states(journal), the stored reverse diff will be truncated in
	// the next restart.
	if freezer != nil {
		err := storeReverseDiff(freezer, bottom, statelimit)
		if err != nil {
			return nil, err
		}
	}
	if stateHistory != nil {
		err := storeStateHistory(stateHistory, bottom)
		if err != nil {
			return nil, err
		}
	}
	// Drop the previous value to reduce memory usage.
	slim := make(map[common.Hash]map[string]*memoryNode)
	for owner, nodes := range bottom.nodes {
		subset := make(map[string]*memoryNode)
		for path, n := range nodes {
			subset[path] = n.unwrap()
		}
		slim[owner] = subset
	}
	ndl := newDiskLayer(bottom.root, bottom.diffid, dl.clean, dl.dirty.commit(slim), dl.diskdb)

	// Persist the content in disk layer if there are too many nodes cached.
	if err := ndl.dirty.mayFlush(ndl.diskdb, ndl.clean, ndl.diffid, force); err != nil {
		return nil, err
	}
	return ndl, nil
}

// revert applies the given reverse diff by reverting the disk layer
// and return a newly constructed disk layer.
func (dl *diskLayer) revert(diff *reverseDiff, diffid uint64) (*diskLayer, error) {
	root := dl.Root()
	if diff.Root != root {
		return nil, errUnmatchedReverseDiff
	}
	if diffid != dl.diffid {
		return nil, errUnmatchedReverseDiff
	}
	if dl.diffid == 0 {
		return nil, fmt.Errorf("%w: zero reverse diff id", errStateUnrecoverable)
	}
	// Mark the diskLayer as stale before applying any mutations on top.
	dl.lock.Lock()
	defer dl.lock.Unlock()

	dl.stale = true

	switch {
	case dl.dirty.empty():
		// The disk cache is empty, applies the state reverting
		// on disk directly.
		batch := dl.diskdb.NewBatch()
		diff.apply(batch)
		rawdb.WriteReverseDiffHead(batch, diffid-1)

		if err := batch.Write(); err != nil {
			log.Crit("Failed to write reverse diff", "err", err)
		}
		batch.Reset()

	default:
		// Revert embedded state in the disk set.
		if err := dl.dirty.revert(diff); err != nil {
			return nil, err
		}
	}
	return newDiskLayer(diff.Parent, dl.diffid-1, dl.clean, dl.dirty, dl.diskdb), nil
}

// setCacheSize sets the dirty cache size to the provided value.
func (dl *diskLayer) setCacheSize(size int) error {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	if dl.stale {
		return errSnapshotStale
	}
	return dl.dirty.setSize(size, dl.diskdb, dl.clean, dl.diffid)
}

// size returns the approximate size of cached nodes in the disk layer.
func (dl *diskLayer) size() common.StorageSize {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	if dl.stale {
		return 0
	}
	return common.StorageSize(dl.dirty.size)
}
