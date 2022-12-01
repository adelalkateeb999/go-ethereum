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

import (
	"bytes"
	"encoding/binary"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
)

// stateDiff represents a reverse change of a state data. The prev refers to the
// content before the change is applied.
type elementHistory struct {
	Key  []byte // Hex format element key(e.g. account hash or slot location)
	Prev []byte // Element value, nil means the node is previously non-existent
}

type accountIndex struct {
	Hash       common.Hash
	DataOffset uint32
	DataLength uint32
	SlotOffset uint32
	SlotNumber uint32
}

type accountIndexes []*accountIndex

func (is accountIndexes) encode() []byte {
	var buf = new(bytes.Buffer)

	for _, index := range is {
		buf.Write(index.Hash.Bytes()) // 32 bytes

		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], index.DataOffset)
		buf.Write(tmp[:])
		binary.BigEndian.PutUint32(tmp[:], index.DataLength)
		buf.Write(tmp[:])
		binary.BigEndian.PutUint32(tmp[:], index.SlotOffset)
		buf.Write(tmp[:])
		binary.BigEndian.PutUint32(tmp[:], index.SlotNumber)
		buf.Write(tmp[:])
	}
	return buf.Bytes()
}

type slotIndex struct {
	Hash       common.Hash
	DataOffset uint32
	DataLength uint32
}
type slotIndexes []*slotIndex

func (is slotIndexes) encode() []byte {
	var buf = new(bytes.Buffer)
	for _, index := range is {
		buf.Write(index.Hash.Bytes()) // 32 bytes

		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], index.DataOffset)
		buf.Write(tmp[:])
		binary.BigEndian.PutUint32(tmp[:], index.DataLength)
	}
	return buf.Bytes()
}

func newSlotIndex(offset uint32, slots []elementHistory) ([]*slotIndex, []byte, uint32) {
	var (
		data    []byte
		indexes []*slotIndex
	)
	for _, slot := range slots {
		index := &slotIndex{
			Hash:       common.BytesToHash(slot.Key),
			DataOffset: offset,
			DataLength: uint32(len(slot.Prev)),
		}
		indexes = append(indexes, index)
		data = append(data, slot.Prev...)
		offset += uint32(len(slot.Prev))
	}
	return indexes, data, offset
}

func newAccountIndex(element elementHistory, accountOffset uint32, slotIndexes []*slotIndex, slotOffset uint32) (*accountIndex, uint32) {
	return &accountIndex{
		Hash:       common.BytesToHash(element.Key),
		DataOffset: accountOffset,
		DataLength: uint32(len(element.Prev)),
		SlotOffset: slotOffset,
		SlotNumber: uint32(len(slotIndexes)),
	}, accountOffset + uint32(len(element.Prev))
}

func newStateHistory(nodes map[common.Hash]map[string]*nodeWithPrev) ([]*accountIndex, []*slotIndex, []byte, []byte) {
	var (
		accounts    []common.Hash
		accountBlob [][]byte
		leaves      = make(map[common.Hash][]elementHistory)

		accountData   []byte
		slotData      []byte
		accountOffset uint32
		slotOffset    uint32

		accountIndexes []*accountIndex
		slotIndexes    []*slotIndex
	)
	for owner, subset := range nodes {
		var elements []elementHistory
		resolvePrevLeaves(subset, func(path []byte, blob []byte) {
			elements = append(elements, elementHistory{
				Key:  common.CopyBytes(path),
				Prev: common.CopyBytes(blob),
			})
		})
		leaves[owner] = elements

		if owner == (common.Hash{}) {
			for _, element := range elements {
				accounts = append(accounts, common.BytesToHash(element.Key))
				accountBlob = append(accountBlob, element.Prev)
			}
		}
	}
	for i, account := range accounts {
		slots, ok := leaves[account]
		if !ok {
			// todo
		}
		sindexes, sdata, soffset := newSlotIndex(slotOffset, slots)

		aIndex, aoffset := newAccountIndex(leaves[account][i], accountOffset, sindexes, uint32(len(sindexes)))
		accountIndexes = append(accountIndexes, aIndex)
		accountOffset = aoffset
		accountData = append(accountData, leaves[account][i].Prev...)

		slotIndexes = append(slotIndexes, sindexes...)
		slotData = append(slotData, sdata...)
		slotOffset = soffset
	}
	return accountIndexes, slotIndexes, accountData, slotData
}

// storeReverseDiff constructs the reverse state diff for the passed bottom-most
// diff layer. After storing the corresponding reverse diff, it will also prune
// the stale reverse diffs from the disk with the given threshold.
// This function will panic if it's called for non-bottom-most diff layer.
func storeStateHistory(freezer *rawdb.Freezer, dl *diffLayer) error {
	aIndexes, sIndexes, aData, sData := newStateHistory(dl.nodes)
	aIndexEnc := accountIndexes(aIndexes).encode()
	sIndexEnc := slotIndexes(sIndexes).encode()

	// The reverse diff object and the lookup are stored in two different
	// places, so there is no atomicity guarantee. It's possible that reverse
	// diff object is written but lookup is not, vice versa. So double-check
	// the presence when using the reverse diff.
	rawdb.WriteStateHistory(freezer, dl.diffid, aIndexEnc, sIndexEnc, aData, sData)
	return nil
}
