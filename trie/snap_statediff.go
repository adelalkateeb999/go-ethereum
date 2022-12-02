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

// stateHistory represents a state change(account, storage slot). The prev
// refers to the content before the change is applied.
type stateHistory struct {
	Key  []byte // Hex format element key(e.g. account hash or slot location)
	Prev []byte // RLP-encoded value, nil means the state is previously non-existent
}

// accountMetadata describes the metadata belongs to an account history.
type accountMetadata struct {
	Hash       common.Hash
	Offset     uint32
	Length     uint32
	SlotOffset uint32
	SlotNumber uint32
}

type accountIndexes []*accountMetadata

func (is accountIndexes) encode() []byte {
	var (
		tmp [16]byte
		buf = new(bytes.Buffer)
	)
	for _, index := range is {
		buf.Write(index.Hash.Bytes())
		binary.BigEndian.PutUint32(tmp[:4], index.Offset)
		binary.BigEndian.PutUint32(tmp[4:8], index.Length)
		binary.BigEndian.PutUint32(tmp[8:12], index.SlotOffset)
		binary.BigEndian.PutUint32(tmp[12:], index.SlotNumber)
		buf.Write(tmp[:])
	}
	return buf.Bytes()
}

type storageMetadata struct {
	Hash   common.Hash
	Offset uint32
	Length uint32
}
type slotIndexes []*storageMetadata

func (is slotIndexes) encode() []byte {
	var (
		tmp [8]byte
		buf = new(bytes.Buffer)
	)
	for _, index := range is {
		buf.Write(index.Hash.Bytes())
		binary.BigEndian.PutUint32(tmp[:4], index.Offset)
		binary.BigEndian.PutUint32(tmp[4:], index.Length)
		buf.Write(tmp[:])
	}
	return buf.Bytes()
}

func newStorageMetadata(offset uint32, storage []stateHistory) ([]*storageMetadata, []byte, uint32) {
	var (
		data  []byte
		metas []*storageMetadata
	)
	for _, slot := range storage {
		meta := &storageMetadata{
			Hash:   common.BytesToHash(slot.Key),
			Offset: offset,
			Length: uint32(len(slot.Prev)),
		}
		metas = append(metas, meta)
		data = append(data, slot.Prev...)
		offset += uint32(len(slot.Prev))
	}
	return metas, data, offset
}

func newAccountIndex(element stateHistory, accountOffset uint32, slotIndexes []*storageMetadata, slotOffset uint32) *accountMetadata {
	return &accountMetadata{
		Hash:       common.BytesToHash(element.Key),
		Offset:     accountOffset,
		Length:     uint32(len(element.Prev)),
		SlotOffset: slotOffset,
		SlotNumber: uint32(len(slotIndexes)),
	}
}

func newStateHistory(nodes map[common.Hash]map[string]*nodeWithPrev) ([]*accountMetadata, []*storageMetadata, []byte, []byte) {
	var (
		leaves = make(map[common.Hash][]stateHistory)

		accountData    []byte
		slotData       []byte
		accountOffset  uint32
		accountSlotOff uint32
		slotOffset     uint32

		accountMetas []*accountMetadata
		storageMetas []*storageMetadata
	)
	for owner, subset := range nodes {
		var elements []stateHistory
		resolvePrevLeaves(subset, func(path []byte, blob []byte) {
			elements = append(elements, stateHistory{
				Key:  common.CopyBytes(path),
				Prev: common.CopyBytes(blob),
			})
		})
		leaves[owner] = elements
	}
	for _, element := range leaves[common.Hash{}] {
		accountHash := common.BytesToHash(element.Key)
		slots := leaves[accountHash]
		sMeta, sdata, soffset := newStorageMetadata(slotOffset, slots)

		aMeta := newAccountIndex(element, accountOffset, sMeta, accountSlotOff)
		accountMetas = append(accountMetas, aMeta)
		accountOffset += uint32(len(element.Prev))
		accountSlotOff += uint32(len(sMeta))
		accountData = append(accountData, element.Prev...)

		storageMetas = append(storageMetas, sMeta...)
		slotData = append(slotData, sdata...)
		slotOffset = soffset
	}
	return accountMetas, storageMetas, accountData, slotData
}

// storeReverseDiff constructs the reverse state diff for the passed bottom-most
// diff layer. After storing the corresponding reverse diff, it will also prune
// the stale reverse diffs from the disk with the given threshold.
// This function will panic if it's called for non-bottom-most diff layer.
func storeStateHistory(freezer *rawdb.Freezer, dl *diffLayer) error {
	aMeta, sMeta, aData, sData := newStateHistory(dl.nodes)
	aIndexEnc := accountIndexes(aMeta).encode()
	sIndexEnc := slotIndexes(sMeta).encode()

	// The reverse diff object and the lookup are stored in two different
	// places, so there is no atomicity guarantee. It's possible that reverse
	// diff object is written but lookup is not, vice versa. So double-check
	// the presence when using the reverse diff.
	rawdb.WriteStateHistory(freezer, dl.diffid, aIndexEnc, sIndexEnc, aData, sData)
	return nil
}
