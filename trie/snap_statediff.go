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
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>

package trie

import "github.com/ethereum/go-ethereum/common"

// stateDiff represents a reverse change of a state data. The prev refers to the
// content before the change is applied.
type elementHistory struct {
	Key  []byte // Hex format element key(e.g. account hash or slot location)
	Prev []byte // Element value, nil means the node is previously non-existent
}

// singleHistory represents a list of state diffs belong to a single contract
// or the main account trie.
type singleHistory struct {
	Owner  common.Hash      // Identifier of contract or empty for main account trie
	States []elementHistory // The list of state history
}

type stateHistory struct {
	Version uint64          // The version tag of stored reverse diff
	Parent  common.Hash     // The corresponding state root of parent block
	Root    common.Hash     // The corresponding state root which these diffs belong to
	States  []singleHistory // The list of state changes
}
