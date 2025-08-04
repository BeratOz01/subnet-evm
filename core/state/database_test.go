// Copyright (C) 2019-2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.
package state

import (
	"encoding/binary"
	"math/big"
	"math/rand"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm/stateconf"
	"github.com/ava-labs/libevm/trie/trienode"
	"github.com/ava-labs/libevm/triedb"
	"github.com/ava-labs/subnet-evm/core/blockstm"
	"github.com/ava-labs/subnet-evm/triedb/firewood"
	"github.com/ava-labs/subnet-evm/triedb/hashdb"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
	"gotest.tools/assert"
)

const (
	commit byte = iota
	createAccount
	updateAccount
	deleteAccount
	addStorage
	updateStorage
	deleteStorage
	maxStep
)

var (
	stepMap = map[byte]string{
		commit:        "commit",
		createAccount: "createAccount",
		updateAccount: "updateAccount",
		deleteAccount: "deleteAccount",
		addStorage:    "addStorage",
		updateStorage: "updateStorage",
		deleteStorage: "deleteStorage",
	}
)

type fuzzState struct {
	require *require.Assertions

	// current state
	currentAddrs               []common.Address
	currentStorageInputIndices map[common.Address]uint64
	inputCounter               uint64
	blockNumber                uint64

	// pending changes to be committed
	merkleTries []*merkleTrie
}
type merkleTrie struct {
	name             string
	ethDatabase      Database
	accountTrie      Trie
	openStorageTries map[common.Address]Trie
	lastRoot         common.Hash
}

func newFuzzState(t *testing.T) *fuzzState {
	r := require.New(t)

	hashState := NewDatabaseWithConfig(
		rawdb.NewMemoryDatabase(),
		&triedb.Config{
			DBOverride: hashdb.Defaults.BackendConstructor,
		})
	ethRoot := types.EmptyRootHash
	hashTr, err := hashState.OpenTrie(ethRoot)
	r.NoError(err)
	t.Cleanup(func() {
		r.NoError(hashState.TrieDB().Close())
	})

	firewoodMemdb := rawdb.NewMemoryDatabase()
	fwCfg := firewood.Defaults
	fwCfg.FilePath = filepath.Join(t.TempDir(), "firewood") // Use a temporary directory for the Firewood
	firewoodState := NewDatabaseWithConfig(
		firewoodMemdb,
		&triedb.Config{
			DBOverride: fwCfg.BackendConstructor,
		},
	)
	fwTr, err := firewoodState.OpenTrie(ethRoot)
	r.NoError(err)
	t.Cleanup(func() {
		r.NoError(firewoodState.TrieDB().Close())
	})

	return &fuzzState{
		merkleTries: []*merkleTrie{
			&merkleTrie{
				name:             "hash",
				ethDatabase:      hashState,
				accountTrie:      hashTr,
				openStorageTries: make(map[common.Address]Trie),
				lastRoot:         ethRoot,
			},
			&merkleTrie{
				name:             "firewood",
				ethDatabase:      firewoodState,
				accountTrie:      fwTr,
				openStorageTries: make(map[common.Address]Trie),
				lastRoot:         ethRoot,
			},
		},
		currentStorageInputIndices: make(map[common.Address]uint64),
		require:                    r,
	}
}

// commit writes the pending changes to both tries and clears the pending changes
func (fs *fuzzState) commit() {
	for _, tr := range fs.merkleTries {
		mergedNodeSet := trienode.NewMergedNodeSet()
		for addr, str := range tr.openStorageTries {
			accountStateRoot, set, err := str.Commit(false)
			fs.require.NoError(err, "failed to commit storage trie for account %s in %s", addr.Hex(), tr.name)
			// A no-op change returns a nil set, which will cause merge to panic.
			if set != nil {
				fs.require.NoError(mergedNodeSet.Merge(set), "failed to merge storage trie nodeset for account %s in %s", addr.Hex(), tr.name)
			}

			acc, err := tr.accountTrie.GetAccount(addr)
			fs.require.NoError(err, "failed to get account %s in %s", addr.Hex(), tr.name)
			// If the account was deleted, we can skip updating the account's
			// state root.
			fs.require.NotNil(acc, "account %s is nil in %s", addr.Hex(), tr.name)

			acc.Root = accountStateRoot
			fs.require.NoError(tr.accountTrie.UpdateAccount(addr, acc), "failed to update account %s in %s", addr.Hex(), tr.name)
		}

		updatedRoot, set, err := tr.accountTrie.Commit(true)
		fs.require.NoError(err, "failed to commit account trie in %s", tr.name)

		// A no-op change returns a nil set, which will cause merge to panic.
		if set != nil {
			fs.require.NoError(mergedNodeSet.Merge(set), "failed to merge account trie nodeset in %s", tr.name)
		}

		// HashDB/PathDB only allows updating the triedb if there have been changes.
		if _, ok := tr.ethDatabase.TrieDB().Backend().(*firewood.Database); ok {
			triedbopt := stateconf.WithTrieDBUpdatePayload(common.Hash{byte(int64(fs.blockNumber - 1))}, common.Hash{byte(int64(fs.blockNumber))})
			fs.require.NoError(tr.ethDatabase.TrieDB().Update(updatedRoot, tr.lastRoot, fs.blockNumber, mergedNodeSet, nil, triedbopt), "failed to update triedb in %s", tr.name)
			tr.lastRoot = updatedRoot
		} else if updatedRoot != tr.lastRoot {
			fs.require.NoError(tr.ethDatabase.TrieDB().Update(updatedRoot, tr.lastRoot, fs.blockNumber, mergedNodeSet, nil), "failed to update triedb in %s", tr.name)
			tr.lastRoot = updatedRoot
		}
		tr.openStorageTries = make(map[common.Address]Trie)
		fs.require.NoError(tr.ethDatabase.TrieDB().Commit(updatedRoot, true),
			"failed to commit %s: expected hashdb root %s", tr.name, fs.merkleTries[0].lastRoot.Hex())
		tr.accountTrie, err = tr.ethDatabase.OpenTrie(tr.lastRoot)
		fs.require.NoError(err, "failed to reopen account trie for %s", tr.name)
	}
	fs.blockNumber++

	// After computing the new root for each trie, we can confirm that the hashing matches
	expectedRoot := fs.merkleTries[0].lastRoot
	for i, tr := range fs.merkleTries[1:] {
		fs.require.Equalf(expectedRoot, tr.lastRoot,
			"root mismatch for %s: expected %x, got %x (trie index %d)",
			tr.name, expectedRoot.Hex(), tr.lastRoot.Hex(), i,
		)
	}
}

// createAccount generates a new, unique account and adds it to both tries and the tracked
// current state.
func (fs *fuzzState) createAccount() {
	fs.inputCounter++
	addr := common.BytesToAddress(crypto.Keccak256Hash(binary.BigEndian.AppendUint64(nil, fs.inputCounter)).Bytes())
	acc := &types.StateAccount{
		Nonce:    1,
		Balance:  uint256.NewInt(100),
		Root:     types.EmptyRootHash,
		CodeHash: types.EmptyCodeHash[:],
	}
	fs.currentAddrs = append(fs.currentAddrs, addr)

	for _, tr := range fs.merkleTries {
		fs.require.NoError(tr.accountTrie.UpdateAccount(addr, acc), "failed to create account %s in %s", addr.Hex(), tr.name)
	}
}

// selectAccount returns a random account and account hash for the provided index
// assumes: addrIndex < len(tr.currentAddrs)
func (fs *fuzzState) selectAccount(addrIndex int) common.Address {
	return fs.currentAddrs[addrIndex]
}

// updateAccount selects a random account, increments its nonce, and adds the update
// to the pending changes for both tries.
func (fs *fuzzState) updateAccount(addrIndex int) {
	addr := fs.selectAccount(addrIndex)

	for _, tr := range fs.merkleTries {
		acc, err := tr.accountTrie.GetAccount(addr)
		fs.require.NoError(err, "failed to get account %s for update in %s", addr.Hex(), tr.name)
		fs.require.NotNil(acc, "account %s is nil for update in %s", addr.Hex(), tr.name)
		acc.Nonce++
		acc.CodeHash = crypto.Keccak256Hash(acc.CodeHash[:]).Bytes()
		acc.Balance.Add(acc.Balance, uint256.NewInt(3))
		fs.require.NoError(tr.accountTrie.UpdateAccount(addr, acc), "failed to update account %s in %s", addr.Hex(), tr.name)
	}
}

// deleteAccount selects a random account and deletes it from both tries and the tracked
// current state.
func (fs *fuzzState) deleteAccount(accountIndex int) {
	deleteAddr := fs.selectAccount(accountIndex)
	fs.currentAddrs = slices.DeleteFunc(fs.currentAddrs, func(addr common.Address) bool {
		return deleteAddr == addr
	})
	for _, tr := range fs.merkleTries {
		fs.require.NoError(tr.accountTrie.DeleteAccount(deleteAddr), "failed to delete account %s in %s", deleteAddr.Hex(), tr.name)
		delete(tr.openStorageTries, deleteAddr) // remove any open storage trie for the deleted account
	}
}

// openStorageTrie opens the storage trie for the provided account address.
// Uses an already opened trie, if there's a pending update to the ethereum nested
// storage trie.
//
// must maintain a map of currently open storage tries, so we can defer committing them
// until commit as opposed to after each storage update.
// This mimics the actual handling of state commitments in the EVM where storage tries are all committed immediately
// before updating the account trie along with the updated storage trie roots:
// https://github.com/ava-labs/libevm/blob/0bfe4a0380c86d7c9bf19fe84368b9695fcb96c7/core/state/statedb.go#L1155
//
// If we attempt to commit the storage tries after each operation, then attempting to re-open the storage trie
// with an updated storage trie root from ethDatabase will fail since the storage trie root will not have been
// persisted yet - leading to a missing trie node error.
func (fs *fuzzState) openStorageTrie(addr common.Address, tr *merkleTrie) Trie {
	storageTrie, ok := tr.openStorageTries[addr]
	if ok {
		return storageTrie
	}

	acc, err := tr.accountTrie.GetAccount(addr)
	fs.require.NoError(err, "failed to get account %s for storage trie in %s", addr.Hex(), tr.name)
	fs.require.NotNil(acc, "account %s not found in %s", addr.Hex(), tr.name)
	storageTrie, err = tr.ethDatabase.OpenStorageTrie(tr.lastRoot, addr, acc.Root, tr.accountTrie)
	fs.require.NoError(err, "failed to open storage trie for %s in %s", addr.Hex(), tr.name)
	tr.openStorageTries[addr] = storageTrie
	return storageTrie
}

// addStorage selects an account and adds a new storage key-value pair to the account.
func (fs *fuzzState) addStorage(accountIndex int) {
	addr := fs.selectAccount(accountIndex)
	// Increment storageInputIndices for the account and take the next input to generate
	// a new storage key-value pair for the account.
	fs.currentStorageInputIndices[addr]++
	storageIndex := fs.currentStorageInputIndices[addr]
	key := crypto.Keccak256Hash(binary.BigEndian.AppendUint64(nil, storageIndex))
	keyHash := crypto.Keccak256Hash(key[:])
	val := crypto.Keccak256Hash(keyHash[:])

	for _, tr := range fs.merkleTries {
		str := fs.openStorageTrie(addr, tr)
		fs.require.NoError(str.UpdateStorage(addr, key[:], val[:]), "failed to add storage for account %s in %s", addr.Hex(), tr.name)
	}

	fs.currentStorageInputIndices[addr]++
}

// updateStorage selects an account and updates an existing storage key-value pair
// note: this may "update" a key-value pair that doesn't exist if it was previously deleted.
func (fs *fuzzState) updateStorage(accountIndex int, storageIndexInput uint64) {
	addr := fs.selectAccount(accountIndex)
	storageIndex := fs.currentStorageInputIndices[addr]
	storageIndex %= storageIndexInput

	storageKey := crypto.Keccak256Hash(binary.BigEndian.AppendUint64(nil, storageIndex))
	storageKeyHash := crypto.Keccak256Hash(storageKey[:])
	fs.inputCounter++
	updatedValInput := binary.BigEndian.AppendUint64(storageKeyHash[:], fs.inputCounter)
	updatedVal := crypto.Keccak256Hash(updatedValInput[:])

	for _, tr := range fs.merkleTries {
		str := fs.openStorageTrie(addr, tr)
		fs.require.NoError(str.UpdateStorage(addr, storageKey[:], updatedVal[:]), "failed to update storage for account %s in %s", addr.Hex(), tr.name)
	}
}

// deleteStorage selects an account and deletes an existing storage key-value pair
// note: this may "delete" a key-value pair that doesn't exist if it was previously deleted.
func (fs *fuzzState) deleteStorage(accountIndex int, storageIndexInput uint64) {
	addr := fs.selectAccount(accountIndex)
	storageIndex := fs.currentStorageInputIndices[addr]
	storageIndex %= storageIndexInput
	storageKey := crypto.Keccak256Hash(binary.BigEndian.AppendUint64(nil, storageIndex))

	for _, tr := range fs.merkleTries {
		str := fs.openStorageTrie(addr, tr)
		fs.require.NoError(str.DeleteStorage(addr, storageKey[:]), "failed to delete storage for account %s in %s", addr.Hex(), tr.name)
	}
}

func FuzzTree(f *testing.F) {
	for randSeed := range int64(1000) {
		rand := rand.New(rand.NewSource(randSeed))
		steps := make([]byte, 32)
		_, err := rand.Read(steps)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(randSeed, steps)
	}
	f.Fuzz(func(t *testing.T, randSeed int64, byteSteps []byte) {
		fuzzState := newFuzzState(t)
		rand := rand.New(rand.NewSource(randSeed))

		for range 10 {
			fuzzState.createAccount()
		}
		fuzzState.commit()

		const maxSteps = 1000
		if len(byteSteps) > maxSteps {
			byteSteps = byteSteps[:maxSteps]
		}

		for _, step := range byteSteps {
			step = step % maxStep
			t.Log(stepMap[step])
			switch step {
			case commit:
				fuzzState.commit()
			case createAccount:
				fuzzState.createAccount()
			case updateAccount:
				if len(fuzzState.currentAddrs) > 0 {
					fuzzState.updateAccount(rand.Intn(len(fuzzState.currentAddrs)))
				}
			case deleteAccount:
				if len(fuzzState.currentAddrs) > 0 {
					fuzzState.deleteAccount(rand.Intn(len(fuzzState.currentAddrs)))
				}
			case addStorage:
				if len(fuzzState.currentAddrs) > 0 {
					fuzzState.addStorage(rand.Intn(len(fuzzState.currentAddrs)))
				}
			case updateStorage:
				if len(fuzzState.currentAddrs) > 0 {
					fuzzState.updateStorage(rand.Intn(len(fuzzState.currentAddrs)), rand.Uint64())
				}
			case deleteStorage:
				if len(fuzzState.currentAddrs) > 0 {
					fuzzState.deleteStorage(rand.Intn(len(fuzzState.currentAddrs)), rand.Uint64())
				}
			default:
				t.Fatalf("unknown step: %d", step)
			}
		}
	})
}

func TestMVHashMapReadWriteDelete(t *testing.T) {
	t.Parallel()

	db := NewDatabase(rawdb.NewMemoryDatabase())
	mvhm := blockstm.NewMVHashMap()
	s, _ := New(common.Hash{}, db, nil)
	s.SetMVHashMap(mvhm)

	states := []*StateDB{s}

	// Create copies of the original state for each transition
	for i := 1; i <= 4; i++ {
		sCopy := s.Copy()
		sCopy.txIndex = i
		states = append(states, sCopy)
	}

	addr := common.HexToAddress("0x01")
	key := common.HexToHash("0x01")
	val := common.HexToHash("0x01")
	balance := uint256.NewInt(100)

	// Tx0 read
	v := states[0].GetState(addr, key)

	assert.Equal(t, common.Hash{}, v)

	// Tx1 write
	states[1].getOrNewStateObject(addr)
	states[1].SetState(addr, key, val)
	states[1].SetBalance(addr, balance)
	states[1].FlushMVWriteSet()

	// Tx1 read
	v = states[1].GetState(addr, key)
	b := states[1].GetBalance(addr)

	assert.Equal(t, val, v)
	assert.Equal(t, balance, b)

	// Tx2 read
	v = states[2].GetState(addr, key)
	b = states[2].GetBalance(addr)

	assert.Equal(t, val, v)
	assert.Equal(t, balance, b)

	// Tx3 delete
	states[3].SelfDestruct(addr)

	// Within Tx 3, the state should not change before finalize
	v = states[3].GetState(addr, key)
	assert.Equal(t, val, v)

	// After finalizing Tx 3, the state will change
	states[3].Finalise(false)
	v = states[3].GetState(addr, key)
	assert.Equal(t, common.Hash{}, v)
	states[3].FlushMVWriteSet()

	// Tx4 read
	v = states[4].GetState(addr, key)
	b = states[4].GetBalance(addr)

	assert.Equal(t, common.Hash{}, v)
	assert.Equal(t, b.Cmp(uint256.NewInt(0)), 0)
}

func TestMVHashMapCreateContract(t *testing.T) {
	t.Parallel()

	db := NewDatabase(rawdb.NewMemoryDatabase())
	mvhm := blockstm.NewMVHashMap()
	s, _ := New(common.Hash{}, db, nil)
	s.SetMVHashMap(mvhm)

	states := []*StateDB{s}

	// Create copies of the original state for each transition
	for i := 1; i <= 4; i++ {
		sCopy := s.Copy()
		sCopy.txIndex = i
		states = append(states, sCopy)
	}

	addr := common.HexToAddress("0x01")
	states[0].SetBalance(addr, uint256.NewInt(100))
	states[0].FlushMVWriteSet()

	states[1].CreateAccount(addr)
	states[1].FlushMVWriteSet()

	b := states[1].GetBalance(addr)
	assert.Equal(t, b.Cmp(uint256.NewInt(100)), 0)
}

func TestMVHashMapRevert(t *testing.T) {
	t.Parallel()

	db := NewDatabase(rawdb.NewMemoryDatabase())
	mvhm := blockstm.NewMVHashMap()
	s, _ := New(common.Hash{}, db, nil)
	s.SetMVHashMap(mvhm)

	states := []*StateDB{s}

	// Create copies of the original state for each transition
	for i := 1; i <= 4; i++ {
		sCopy := s.Copy()
		sCopy.txIndex = i
		states = append(states, sCopy)
	}

	addr := common.HexToAddress("0x01")
	key := common.HexToHash("0x01")
	val := common.HexToHash("0x01")
	balance := uint256.NewInt(100)

	// Tx0 write
	states[0].getOrNewStateObject(addr)
	states[0].SetState(addr, key, val)
	states[0].SetBalance(addr, balance)
	states[0].FlushMVWriteSet()

	// Tx1 perform some ops and then revert
	snapshot := states[1].Snapshot()
	states[1].AddBalance(addr, uint256.NewInt(100))
	states[1].SetState(addr, key, common.HexToHash("0x02"))
	v := states[1].GetState(addr, key)
	b := states[1].GetBalance(addr)
	assert.Equal(t, b.Cmp(uint256.NewInt(200)), 0)
	assert.Equal(t, common.HexToHash("0x02"), v)

	states[1].SelfDestruct(addr)

	states[1].RevertToSnapshot(snapshot)

	v = states[1].GetState(addr, key)
	b = states[1].GetBalance(addr)

	assert.Equal(t, val, v)
	assert.Equal(t, b.Cmp(balance), 0)
	states[1].Finalise(false)
	states[1].FlushMVWriteSet()

	// Tx2 check the state and balance
	v = states[2].GetState(addr, key)
	b = states[2].GetBalance(addr)

	assert.Equal(t, val, v)
	assert.Equal(t, b.Cmp(balance), 0)
}

func TestMVHashMapMarkEstimate(t *testing.T) {
	t.Parallel()

	db := NewDatabase(rawdb.NewMemoryDatabase())
	mvhm := blockstm.NewMVHashMap()
	s, _ := New(common.Hash{}, db, nil)
	s.SetMVHashMap(mvhm)

	states := []*StateDB{s}

	// Create copies of the original state for each transition
	for i := 1; i <= 4; i++ {
		sCopy := s.Copy()
		sCopy.txIndex = i
		states = append(states, sCopy)
	}

	addr := common.HexToAddress("0x01")
	key := common.HexToHash("0x01")
	val := common.HexToHash("0x01")
	balance := uint256.NewInt(100)

	// Tx0 read
	v := states[0].GetState(addr, key)
	assert.Equal(t, common.Hash{}, v)

	// Tx0 write
	states[0].SetState(addr, key, val)
	v = states[0].GetState(addr, key)
	assert.Equal(t, val, v)
	states[0].FlushMVWriteSet()

	// Tx1 write
	states[1].getOrNewStateObject(addr)
	states[1].SetState(addr, key, val)
	states[1].SetBalance(addr, balance)
	states[1].FlushMVWriteSet()

	// Tx2 read
	v = states[2].GetState(addr, key)
	b := states[2].GetBalance(addr)

	assert.Equal(t, val, v)
	assert.Equal(t, balance, b)

	// Tx1 mark estimate
	for _, v := range states[1].MVWriteList() {
		mvhm.MarkEstimate(v.Path, 1)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Errorf("The code did not panic")
		} else {
			t.Log("Recovered in f", r)
		}
	}()

	// Tx2 read again should get default (empty) vals because its dependency Tx1 is marked as estimate
	states[2].GetState(addr, key)
	states[2].GetBalance(addr)

	// Tx1 read again should get Tx0 vals
	v = states[1].GetState(addr, key)
	assert.Equal(t, val, v)
}

func TestMVHashMapWriteNoConflict(t *testing.T) {
	t.Parallel()

	db := NewDatabase(rawdb.NewMemoryDatabase())
	mvhm := blockstm.NewMVHashMap()
	s, _ := New(common.Hash{}, db, nil)
	s.SetMVHashMap(mvhm)

	states := []*StateDB{s}

	// Create copies of the original state for each transition
	for i := 1; i <= 6; i++ {
		sCopy := s.Copy()
		sCopy.txIndex = i
		states = append(states, sCopy)
	}

	addr := common.HexToAddress("0x01")
	key1 := common.HexToHash("0x01")
	key2 := common.HexToHash("0x02")
	val1 := common.HexToHash("0x01")
	balance1 := uint256.NewInt(100)
	val2 := common.HexToHash("0x02")

	// Tx0 write
	states[0].getOrNewStateObject(addr)
	states[0].FlushMVWriteSet()

	// Tx2 write
	states[2].SetState(addr, key2, val2)
	states[2].FlushMVWriteSet()

	// Tx1 write
	tx1Snapshot := states[1].Snapshot()
	states[1].SetState(addr, key1, val1)
	states[1].SetBalance(addr, balance1)
	states[1].FlushMVWriteSet()

	// Tx1 read
	assert.Equal(t, val1, states[1].GetState(addr, key1))
	assert.Equal(t, balance1, states[1].GetBalance(addr))
	// Tx1 should see empty value in key2
	assert.Equal(t, common.Hash{}, states[1].GetState(addr, key2))

	// Tx2 read
	assert.Equal(t, val2, states[2].GetState(addr, key2))
	// Tx2 should see values written by Tx1
	assert.Equal(t, val1, states[2].GetState(addr, key1))
	assert.Equal(t, balance1, states[2].GetBalance(addr))

	// Tx3 read
	assert.Equal(t, val1, states[3].GetState(addr, key1))
	assert.Equal(t, val2, states[3].GetState(addr, key2))
	assert.Equal(t, balance1, states[3].GetBalance(addr))

	// Tx2 delete
	for _, v := range states[2].writeMap {
		mvhm.Delete(v.Path, 2)

		states[2].writeMap = nil
	}

	assert.Equal(t, val1, states[4].GetState(addr, key1))
	assert.Equal(t, balance1, states[4].GetBalance(addr))
	assert.Equal(t, common.Hash{}, states[4].GetState(addr, key2))

	// Tx1 revert
	states[1].RevertToSnapshot(tx1Snapshot)
	states[1].FlushMVWriteSet()

	assert.Equal(t, common.Hash{}, states[5].GetState(addr, key1))
	assert.Equal(t, common.Hash{}, states[5].GetState(addr, key2))
	assert.Equal(t, states[5].GetBalance(addr).Cmp(uint256.NewInt(0)), 0)

	// Tx1 delete
	for _, v := range states[1].writeMap {
		mvhm.Delete(v.Path, 1)

		states[1].writeMap = nil
	}

	assert.Equal(t, common.Hash{}, states[6].GetState(addr, key1))
	assert.Equal(t, common.Hash{}, states[6].GetState(addr, key2))
	assert.Equal(t, states[6].GetBalance(addr).Cmp(uint256.NewInt(0)), 0)
}

func TestApplyMVWriteSet(t *testing.T) {
	t.Parallel()

	db := NewDatabase(rawdb.NewMemoryDatabase())
	mvhm := blockstm.NewMVHashMap()
	s, _ := New(common.Hash{}, db, nil)
	s.SetMVHashMap(mvhm)

	sClean := s.Copy()
	sClean.mvHashmap = nil

	sSingleProcess := sClean.Copy()

	states := []*StateDB{s}

	// Create copies of the original state for each transition
	for i := 1; i <= 4; i++ {
		sCopy := s.Copy()
		sCopy.txIndex = i
		states = append(states, sCopy)
	}

	addr1 := common.HexToAddress("0x01")
	addr2 := common.HexToAddress("0x02")
	addr3 := common.HexToAddress("0x03")
	key1 := common.HexToHash("0x01")
	key2 := common.HexToHash("0x02")
	val1 := common.HexToHash("0x01")
	balance1 := uint256.NewInt(100)
	val2 := common.HexToHash("0x02")
	balance2 := uint256.NewInt(200)
	code := []byte{1, 2, 3}

	// Tx0 write
	states[0].getOrNewStateObject(addr1)
	states[0].SetState(addr1, key1, val1)
	states[0].SetBalance(addr1, balance1)
	states[0].SetState(addr2, key2, val2)
	states[0].getOrNewStateObject(addr3)
	states[0].Finalise(true)
	states[0].FlushMVWriteSet()

	sSingleProcess.getOrNewStateObject(addr1)
	sSingleProcess.SetState(addr1, key1, val1)
	sSingleProcess.SetBalance(addr1, balance1)
	sSingleProcess.SetState(addr2, key2, val2)
	sSingleProcess.getOrNewStateObject(addr3)

	sClean.ApplyMVWriteSet(states[0].MVWriteList())
	assert.Equal(t, sSingleProcess.IntermediateRoot(true), sClean.IntermediateRoot(true))

	// Tx1 write
	states[1].SetState(addr1, key2, val2)
	states[1].SetBalance(addr1, balance2)
	states[1].SetNonce(addr1, 1)
	states[1].Finalise(true)
	states[1].FlushMVWriteSet()

	sSingleProcess.SetState(addr1, key2, val2)
	sSingleProcess.SetBalance(addr1, balance2)
	sSingleProcess.SetNonce(addr1, 1)

	sClean.ApplyMVWriteSet(states[1].MVWriteList())
	assert.Equal(t, sSingleProcess.IntermediateRoot(true), sClean.IntermediateRoot(true))

	// Tx2 write
	states[2].SetState(addr1, key1, val2)
	states[2].SetBalance(addr1, balance2)
	states[2].SetNonce(addr1, 2)
	states[2].Finalise(true)
	states[2].FlushMVWriteSet()

	sSingleProcess.SetState(addr1, key1, val2)
	sSingleProcess.SetBalance(addr1, balance2)
	sSingleProcess.SetNonce(addr1, 2)

	sClean.ApplyMVWriteSet(states[2].MVWriteList())
	assert.Equal(t, sSingleProcess.IntermediateRoot(true), sClean.IntermediateRoot(true))

	// Tx3 write
	states[3].SelfDestruct(addr2)
	states[3].SetCode(addr1, code)
	states[3].Finalise(true)
	states[3].FlushMVWriteSet()

	sSingleProcess.SelfDestruct(addr2)
	sSingleProcess.SetCode(addr1, code)

	sClean.ApplyMVWriteSet(states[3].MVWriteList())
	assert.Equal(t, sSingleProcess.IntermediateRoot(true), sClean.IntermediateRoot(true))
}

func TestMVHashMapRevertConcurrent(t *testing.T) {
	t.Parallel()

	db := NewDatabase(rawdb.NewMemoryDatabase())
	mvhm := blockstm.NewMVHashMap()
	s, _ := New(common.Hash{}, db, nil)
	s.SetMVHashMap(mvhm)

	states := []*StateDB{s}

	// Create copies of the original state for each transition
	for i := 1; i <= 2; i++ {
		sCopy := s.Copy()
		sCopy.txIndex = i
		states = append(states, sCopy)
	}

	addr := common.HexToAddress("0x01")
	balance := new(big.Int).SetUint64(uint64(100))

	// Tx0 touches the account. Amount doesn't matter.
	// This is to make sure that Tx1 and Tx2 will use the same state object from Tx0.
	states[0].AddBalance(addr, uint256.MustFromBig(common.Big0))
	states[0].Finalise(false)
	states[0].FlushMVWriteSet()

	// Tx1 creates the account and add balance
	snapshot1 := states[1].Snapshot()
	states[1].CreateAccount(addr)
	states[1].AddBalance(addr, uint256.MustFromBig(balance))

	// Tx2 creates the account, reverts.
	snapshot2 := states[2].Snapshot()
	states[2].CreateAccount(addr)
	states[2].RevertToSnapshot(snapshot2)
	states[2].Finalise(false)

	// Tx2 adds balance
	states[2].AddBalance(addr, uint256.MustFromBig(balance))

	// Tx1 now reverts
	states[1].RevertToSnapshot(snapshot1)
	states[1].Finalise(false)

	// Balance after executing Tx0 should be 0 because it shouldn't be affected by Tx1 or Tx2
	b := states[0].GetBalance(addr)
	assert.Equal(t, b.Cmp(uint256.NewInt(0)), 0)

	// Balance after executing Tx1 should be 0 because Tx1 got reverted
	b = states[1].GetBalance(addr)
	assert.Equal(t, b.Cmp(uint256.NewInt(0)), 0)

	// Balance after executing Tx2 should be 100 because its snapshot is taken before Tx1 got reverted
	b = states[2].GetBalance(addr)
	assert.Equal(t, b.Cmp(uint256.NewInt(100)), 0)
}

func TestMVHashMapOverwrite(t *testing.T) {
	t.Parallel()

	db := NewDatabase(rawdb.NewMemoryDatabase())
	mvhm := blockstm.NewMVHashMap()
	s, _ := New(common.Hash{}, db, nil)
	s.SetMVHashMap(mvhm)

	states := []*StateDB{s}

	// Create copies of the original state for each transition
	for i := 1; i <= 5; i++ {
		sCopy := s.Copy()
		sCopy.txIndex = i
		states = append(states, sCopy)
	}

	addr := common.HexToAddress("0x01")
	key := common.HexToHash("0x01")
	val1 := common.HexToHash("0x01")
	balance1 := uint256.NewInt(100)
	val2 := common.HexToHash("0x02")
	balance2 := uint256.NewInt(200)

	// Tx0 write
	states[0].getOrNewStateObject(addr)
	states[0].SetState(addr, key, val1)
	states[0].SetBalance(addr, balance1)
	states[0].FlushMVWriteSet()

	// Tx1 write
	states[1].SetState(addr, key, val2)
	states[1].SetBalance(addr, balance2)
	v := states[1].GetState(addr, key)
	b := states[1].GetBalance(addr)
	states[1].FlushMVWriteSet()

	assert.Equal(t, val2, v)
	assert.Equal(t, balance2, b)

	// Tx2 read should get Tx1's value
	v = states[2].GetState(addr, key)
	b = states[2].GetBalance(addr)

	assert.Equal(t, val2, v)
	assert.Equal(t, balance2, b)

	// Tx1 delete
	for _, v := range states[1].writeMap {
		mvhm.Delete(v.Path, 1)

		states[1].writeMap = nil
	}

	// Tx3 read should get Tx0's value
	v = states[3].GetState(addr, key)
	b = states[3].GetBalance(addr)

	assert.Equal(t, val1, v)
	assert.Equal(t, balance1, b)

	// Tx1 read should get Tx0's value
	v = states[1].GetState(addr, key)
	b = states[1].GetBalance(addr)

	assert.Equal(t, val1, v)
	assert.Equal(t, balance1, b)

	// Tx0 delete
	for _, v := range states[0].writeMap {
		mvhm.Delete(v.Path, 0)

		states[0].writeMap = nil
	}

	// Tx4 read again should get default vals
	v = states[4].GetState(addr, key)
	b = states[4].GetBalance(addr)

	assert.Equal(t, common.Hash{}, v)
	assert.Equal(t, b.Cmp(uint256.NewInt(0)), 0)
}
