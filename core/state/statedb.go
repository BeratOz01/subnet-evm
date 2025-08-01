// Copyright (C) 2019-2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.
//
// This file is a derived work, based on the go-ethereum library whose original
// notices appear below.
//
// It is distributed under a license compatible with the licensing terms of the
// original code from which it is derived.
//
// Much love to the original authors for their work.
// **********
// Copyright 2014 The go-ethereum Authors
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

// Package state provides a caching layer atop the Ethereum state trie.
package state

import (
	"fmt"

	"github.com/ava-labs/libevm/common"
	ethstate "github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm/stateconf"
	"github.com/ava-labs/subnet-evm/constants"
	"github.com/ava-labs/subnet-evm/core/blockstm"
	"github.com/ava-labs/subnet-evm/utils"
	"github.com/holiman/uint256"
)

// StateDB structs within the ethereum protocol are used to store anything
// within the merkle trie. StateDBs take care of caching and storing
// nested states. It's the general query interface to retrieve:
//
// * Contracts
// * Accounts
//
// Once the state is committed, tries cached in stateDB (including account
// trie, storage tries) will no longer be functional. A new state instance
// must be created with new root and updated database for accessing post-
// commit states.
type StateDB struct {
	*ethstate.StateDB

	// The tx context
	thash   common.Hash
	txIndex int

	// Some fields remembered as they are used in tests
	db    Database
	snaps ethstate.SnapshotTree

	// Block-stm related fields
	mvHashmap    *blockstm.MVHashMap
	incarnation  int
	readMap      map[blockstm.STMKey]blockstm.ReadOperation
	writeMap     map[blockstm.STMKey]blockstm.WriteOperation
	revertedKeys map[blockstm.STMKey]struct{}
	dep          int
}

// New creates a new state from a given trie.
func New(root common.Hash, db Database, snaps ethstate.SnapshotTree) (*StateDB, error) {
	stateDB, err := ethstate.New(root, db, snaps)
	if err != nil {
		return nil, err
	}
	return &StateDB{
		StateDB: stateDB,
		db:      db,
		snaps:   snaps,
	}, nil
}

type workerPool struct {
	*utils.BoundedWorkers
}

func (wp *workerPool) Done() {
	// Done is guaranteed to only be called after all work is already complete,
	// so we call Wait for goroutines to finish before returning.
	wp.BoundedWorkers.Wait()
}

func WithConcurrentWorkers(prefetchers int) ethstate.PrefetcherOption {
	pool := &workerPool{
		BoundedWorkers: utils.NewBoundedWorkers(prefetchers),
	}
	return ethstate.WithWorkerPools(func() ethstate.WorkerPool { return pool })
}

// SetState sets the state of the address
func (s *StateDB) SetState(addr common.Address, key, value common.Hash, opts ...stateconf.StateDBStateOption) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject = s.mvRecordWritten(stateObject)
		MVWrite(s, blockstm.NewStateKey(addr, key))
		stateObject.SetState(key, value)
	}
}

// SetTxContext sets the current transaction hash and index which are
// used when the EVM emits new state logs. It should be invoked before
// transaction execution.
func (s *StateDB) SetTxContext(thash common.Hash, ti int) {
	s.thash = thash
	s.txIndex = ti
	s.StateDB.SetTxContext(thash, ti)
}

// GetTxHash returns the current tx hash on the StateDB set by SetTxContext.
func (s *StateDB) GetTxHash() common.Hash {
	return s.thash
}

// Copy copies the StateDB
func (s *StateDB) Copy() *StateDB {
	return &StateDB{
		StateDB:      s.StateDB.Copy(),
		db:           s.db,
		snaps:        s.snaps,
		thash:        s.thash,
		txIndex:      s.txIndex,
		mvHashmap:    s.mvHashmap,
		incarnation:  s.incarnation,
		readMap:      s.readMap,
		writeMap:     s.writeMap,
		revertedKeys: s.revertedKeys,
		dep:          s.dep,
	}
}

// SetMVHashMap sets the MVHashMap and dependency index to -1
func (s *StateDB) SetMVHashMap(mvhm *blockstm.MVHashMap) {
	s.mvHashmap = mvhm
	s.dep = -1
}

// GetMVHashmap returns the MVHashMap of the StateDB
func (s *StateDB) GetMVHashmap() *blockstm.MVHashMap {
	return s.mvHashmap
}

// MVWriteList returns the write list of the StateDB
func (s *StateDB) MVWriteList() []blockstm.WriteOperation {
	writes := make([]blockstm.WriteOperation, 0, len(s.writeMap))

	for _, v := range s.writeMap {
		// if the key is in the revertedKeys, it means the write was reverted
		if _, ok := s.revertedKeys[v.Path]; !ok {
			writes = append(writes, v)
		}
	}

	return writes
}

// MVFullWriteList returns the full write list of the StateDB
func (s *StateDB) MVFullWriteList() []blockstm.WriteOperation {
	writes := make([]blockstm.WriteOperation, 0, len(s.writeMap))

	for _, v := range s.writeMap {
		writes = append(writes, v)
	}

	return writes
}

// MVReadMap returns the read map of the StateDB
func (s *StateDB) MVReadMap() map[blockstm.STMKey]blockstm.ReadOperation {
	return s.readMap
}

// MVReadList returns the read list of the StateDB
func (s *StateDB) MVReadList() []blockstm.ReadOperation {
	reads := make([]blockstm.ReadOperation, 0, len(s.readMap))

	for _, v := range s.MVReadMap() {
		reads = append(reads, v)
	}

	return reads
}

// ensureReadMap ensures that the read map is initialized before use
func (s *StateDB) ensureReadMap() {
	if s.readMap == nil {
		s.readMap = make(map[blockstm.STMKey]blockstm.ReadOperation)
	}
}

// ensureWriteMap ensures that the write map is initialized before use
func (s *StateDB) ensureWriteMap() {
	if s.writeMap == nil {
		s.writeMap = make(map[blockstm.STMKey]blockstm.WriteOperation)
	}
}

// ClearReadMap clears the read map
func (s *StateDB) ClearReadMap() {
	s.readMap = make(map[blockstm.STMKey]blockstm.ReadOperation)
}

// ClearWriteMap clears the write map
func (s *StateDB) ClearWriteMap() {
	s.writeMap = make(map[blockstm.STMKey]blockstm.WriteOperation)
}

// HadInvalidRead returns true if the StateDB has had an invalid read (means dependency was found)
func (s *StateDB) HadInvalidRead() bool {
	return s.dep >= 0
}

// DepTxIndex returns the dependency transaction index
func (s *StateDB) DepTxIndex() int {
	return s.dep
}

// SetIncarnation sets the incarnation of the StateDB
func (s *StateDB) SetIncarnation(inc int) {
	s.incarnation = inc
}

type StorageVal[T any] struct {
	Value *T
}

func shouldSkipAddress(addr common.Address) bool {
	// if address is blackhole address or system address, skip
	if addr == constants.BlackholeAddr {
		return true
	}

	// if address is 0 address
	if addr == (common.Address{}) {
		return true
	}

	// skip precompile address ranges: 0x01, 0x02, 0x03
	firstByte := addr.Bytes()[0]
	return firstByte == 0x01 || firstByte == 0x02 || firstByte == 0x03
}

// MVRead reads a value from the StateDB using the MVHashMap
func MVRead[T any](s *StateDB, k blockstm.STMKey, defaultV T, readStorage func(s *StateDB) T) (v T) {
	if s.mvHashmap == nil {
		return readStorage(s)
	}

	// need to skip system operations if the address is the blackhole address or system address (like precompile addresses)
	if shouldSkipAddress(k.GetAddress()) {
		return readStorage(s)
	}

	s.ensureReadMap()

	if s.writeMap != nil {
		if _, ok := s.writeMap[k]; ok {
			return readStorage(s)
		}
	}

	if !k.IsAddress() {
		// If we are reading subpath from a deleted account, return default value instead of reading from MVHashmap
		addr := k.GetAddress()
		if !s.StateDB.Exist(addr) {
			return defaultV
		}
	}

	res := s.mvHashmap.Read(k, s.txIndex)

	var rd blockstm.ReadOperation

	rd.Version = blockstm.Version{
		TransactionIndex: res.DependencyIndex(),
		Incarnation:      res.Incarnation(),
	}

	rd.Path = k

	switch res.Status() {
	case blockstm.MVReadDone:
		{
			v = readStorage(res.Data().(*StateDB))
			rd.Kind = blockstm.ReadFromMVHashMap
		}
	case blockstm.MVReadDependency:
		{
			s.dep = res.DependencyIndex()

			panic("Found dependency")
		}
	case blockstm.MVReadNone:
		{
			v = readStorage(s)
			rd.Kind = blockstm.ReadFromState
		}
	default:
		return defaultV
	}

	if prevRd, ok := s.readMap[k]; !ok {
		s.readMap[k] = rd
	} else {
		if prevRd.Kind != rd.Kind || prevRd.Version.TransactionIndex != rd.Version.TransactionIndex || prevRd.Version.Incarnation != rd.Version.Incarnation {
			s.dep = rd.Version.TransactionIndex
			panic("Read conflict detected")
		}
	}

	return
}

// MVWrite writes a value to the StateDB using the MVHashMap
func MVWrite(s *StateDB, k blockstm.STMKey) {
	// whenever the key type is address, skip if the address is blackhole address or system address
	if shouldSkipAddress(k.GetAddress()) {
		return
	}

	if s.mvHashmap != nil {
		s.ensureWriteMap()
		s.writeMap[k] = blockstm.WriteOperation{
			Path: k,
			Data: s,
			Version: blockstm.Version{
				TransactionIndex: s.txIndex,
				Incarnation:      s.incarnation,
			},
		}
	}
}

// RevertWrite reverts a write to the StateDB
func RevertWrite(s *StateDB, k blockstm.STMKey) {
	s.revertedKeys[k] = struct{}{}
}

// MVWritten returns true if the key has been written to the StateDB
func MVWritten(s *StateDB, k blockstm.STMKey) bool {
	if s.mvHashmap == nil || s.writeMap == nil {
		return false
	}

	_, ok := s.writeMap[k]
	return ok
}

// FlushMVWriteSet flushes the write set to the MVHashMap
func (s *StateDB) FlushMVWriteSet() {
	if s.mvHashmap != nil && s.writeMap != nil {
		s.mvHashmap.FlushMVWriteSet(s.MVFullWriteList())
	}
}

const BalancePath = 1
const NoncePath = 2
const CodePath = 3
const SuicidePath = 4

// ApplyMVWriteSet applies entries in a given write set to StateDB. Note that this function does not change MVHashMap nor write set
// of the current StateDB.
func (s *StateDB) ApplyMVWriteSet(writes []blockstm.WriteOperation) {
	for i := range writes {
		path := writes[i].Path
		sr := writes[i].Data.(*StateDB)

		if path.IsState() {
			addr := path.GetAddress()
			stateKey := path.GetStateKey()
			state := sr.GetState(addr, stateKey)
			s.SetState(addr, stateKey, state)
		} else if path.IsAddress() {
			continue
		} else {
			addr := path.GetAddress()

			switch path.GetSubpath() {
			case BalancePath:
				s.SetBalance(addr, sr.GetBalance(addr))
			case NoncePath:
				s.SetNonce(addr, sr.GetNonce(addr))
			case CodePath:
				s.SetCode(addr, sr.GetCode(addr))
			case SuicidePath:
				if sr.Exist(addr) {
					s.SelfDestruct(addr)
				}
			default:
				panic(fmt.Errorf("unknown key type: %d", path.GetSubpath()))
			}
		}
	}
}

// AddEmptyMVHashMap adds empty MVHashMap to StateDB
func (s *StateDB) AddEmptyMVHashMap() {
	mvh := blockstm.NewMVHashMap()
	s.mvHashmap = mvh
}

// GetBalance returns the balance of the address with MVHashMap support
func (s *StateDB) GetBalance(addr common.Address) *uint256.Int {
	return MVRead(s, blockstm.NewSubpathKey(addr, BalancePath), uint256.NewInt(0), func(s *StateDB) *uint256.Int {
		return s.StateDB.GetBalance(addr)
	})
}

// GetNonce returns the nonce of the address with MVHashMap support
func (s *StateDB) GetNonce(addr common.Address) uint64 {
	return MVRead(s, blockstm.NewSubpathKey(addr, NoncePath), uint64(0), func(s *StateDB) uint64 {
		return s.StateDB.GetNonce(addr)
	})
}

// GetCode returns the code of the address with MVHashMap support
func (s *StateDB) GetCode(addr common.Address) []byte {
	return MVRead(s, blockstm.NewSubpathKey(addr, CodePath), []byte{}, func(s *StateDB) []byte {
		return s.StateDB.GetCode(addr)
	})
}

// GetCodeSize returns the code size of the address with MVHashMap support
func (s *StateDB) GetCodeSize(addr common.Address) int {
	return MVRead(s, blockstm.NewSubpathKey(addr, CodePath), 0, func(s *StateDB) int {
		return s.StateDB.GetCodeSize(addr)
	})
}

// GetCodeHash returns the code hash of the address with MVHashMap support
func (s *StateDB) GetCodeHash(addr common.Address) common.Hash {
	return MVRead(s, blockstm.NewSubpathKey(addr, CodePath), common.Hash{}, func(s *StateDB) common.Hash {
		return s.StateDB.GetCodeHash(addr)
	})
}

// GetState retrieves a value from the given account's storage trie.
func (s *StateDB) GetState(addr common.Address, hash common.Hash, opts ...stateconf.StateDBStateOption) common.Hash {
	return MVRead(s, blockstm.NewStateKey(addr, hash), common.Hash{}, func(s *StateDB) common.Hash {
		return s.StateDB.GetState(addr, hash)
	})
}

// GetCommittedState retrieves a value from the given account's storage trie.
func (s *StateDB) GetCommittedState(addr common.Address, hash common.Hash, opts ...stateconf.StateDBStateOption) common.Hash {
	return MVRead(s, blockstm.NewStateKey(addr, hash), common.Hash{}, func(s *StateDB) common.Hash {
		return s.StateDB.GetCommittedState(addr, hash)
	})
}

// HasSelfDestructed returns true if the address has self destructed
func (s *StateDB) HasSelfDestructed(addr common.Address) bool {
	return MVRead(s, blockstm.NewSubpathKey(addr, SuicidePath), false, func(s *StateDB) bool {
		return s.StateDB.HasSelfDestructed(addr)
	})
}

// AddBalance adds the amount to the balance of the address
func (s *StateDB) AddBalance(addr common.Address, amount *uint256.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject == nil {
		return
	}

	if s.mvHashmap != nil {
		// ensure a read balance operation is recorded in mvHashmap
		s.GetBalance(addr)
	}

	stateObject = s.mvRecordWritten(stateObject)
	MVWrite(s, blockstm.NewSubpathKey(addr, BalancePath))
	stateObject.AddBalance(amount)
}

// SubBalance subtracts the amount from the balance of the address
func (s *StateDB) SubBalance(addr common.Address, amount *uint256.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject == nil {
		return
	}

	if s.mvHashmap != nil {
		// ensure a read balance operation is recorded in mvHashmap
		s.GetBalance(addr)
	}

	stateObject = s.mvRecordWritten(stateObject)
	MVWrite(s, blockstm.NewSubpathKey(addr, BalancePath))

	if amount.IsZero() {
		return
	}

	stateObject.SetBalance(new(uint256.Int).Sub(stateObject.Balance(), amount))
}

// SetBalance sets the balance of the address
func (s *StateDB) SetBalance(addr common.Address, amount *uint256.Int) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject = s.mvRecordWritten(stateObject)
		stateObject.SetBalance(amount)
		MVWrite(s, blockstm.NewSubpathKey(addr, BalancePath))
	}
}

// SetNonce sets the nonce of the address
func (s *StateDB) SetNonce(addr common.Address, nonce uint64) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject = s.mvRecordWritten(stateObject)
		stateObject.SetNonce(nonce)
		MVWrite(s, blockstm.NewSubpathKey(addr, NoncePath))
	}
}

// SetCode sets the code of the address
func (s *StateDB) SetCode(addr common.Address, code []byte) {
	stateObject := s.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject = s.mvRecordWritten(stateObject)
		stateObject.SetCode(crypto.Keccak256Hash(code), code)
		MVWrite(s, blockstm.NewSubpathKey(addr, CodePath))
	}
}

// SelfDestruct marks the address as self destructed
func (s *StateDB) SelfDestruct(addr common.Address) {
	// make sure the state object is not nil
	stateObject := s.GetStateObject(addr)
	if stateObject == nil {
		return
	}

	// do the self destruct operation
	s.StateDB.SelfDestruct(addr)
	// save for suicide path and balance path (new balance will be 0)
	MVWrite(s, blockstm.NewSubpathKey(addr, SuicidePath))
	MVWrite(s, blockstm.NewSubpathKey(addr, BalancePath))
}

// mvRecordWritten checks whether a state object is already present in the current MV writeMap.
// If yes, it returns the object directly.
// If not, it clones the object and inserts it into the writeMap before returning it.
func (s *StateDB) mvRecordWritten(object *ethstate.StateObject) *ethstate.StateObject {
	if s.mvHashmap == nil {
		return object
	}

	addrKey := blockstm.NewAddressKey(object.Address())

	if MVWritten(s, addrKey) {
		return object
	}

	// Deepcopy is needed to ensure that objects are not written by multiple transactions at the same time, because
	// the input state object can come from a different transaction.
	s.SetStateObject(object.DeepCopy(s.StateDB))
	MVWrite(s, addrKey)

	return s.StateObjectFromMap(object.Address())
}

// createObject creates a new state object
// overrides the original createObject function to record a write operation for the address key
func (s *StateDB) createObject(addr common.Address) (newobj, prev *ethstate.StateObject) {
	newobj, prev = s.StateDB.CreateObject(addr)
	MVWrite(s, blockstm.NewAddressKey(addr))
	return
}

// CreateAccount creates a new account
func (s *StateDB) CreateAccount(addr common.Address) {
	newObj, prev := s.createObject(addr)
	if prev != nil {
		newObj.SetBalance(prev.Balance())
		// need to set the blockstm key for balance as well
		MVWrite(s, blockstm.NewSubpathKey(addr, BalancePath))
	}
}

// getStateObject returns the state object for the given address
// overrides the original getStateObject function to record a read operation for the address key
func (s *StateDB) getStateObject(addr common.Address) *ethstate.StateObject {
	return MVRead(s, blockstm.NewAddressKey(addr), nil, func(s *StateDB) *ethstate.StateObject {
		return s.StateDB.GetStateObject(addr)
	})
}
