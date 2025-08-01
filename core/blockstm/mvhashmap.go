package blockstm

import (
	"fmt"
	"sync"

	"github.com/ava-labs/libevm/common"
	"github.com/emirpasic/gods/maps/treemap"
)

// Flags
const (
	DoneFlag = iota
	EstimateFlag
)

// Types
const (
	AddressType = 1
	StateType   = 2
	SubpathType = 3
)

// KeyLength is the length of the key in the mvhashmap
// +1 for subpath
// +1 for type
const KeyLength = common.AddressLength + common.HashLength + 2

// STMKey is the key in the mvhashmap
type STMKey [KeyLength]byte

// IsAddress returns true if the key is an address
func (k *STMKey) IsAddress() bool {
	return k[KeyLength-1] == AddressType
}

// IsState returns true if the key is a state
func (k *STMKey) IsState() bool {
	return k[KeyLength-1] == StateType
}

// IsSubpath returns true if the key is a subpath
func (k *STMKey) IsSubpath() bool {
	return k[KeyLength-1] == SubpathType
}

// GetAddress returns the address from the key
func (k *STMKey) GetAddress() common.Address {
	return common.BytesToAddress(k[:common.AddressLength])
}

// GetStateKey returns the state from the key
func (k *STMKey) GetStateKey() common.Hash {
	return common.BytesToHash(k[common.AddressLength : KeyLength-1])
}

// GetSubpath returns the subpath from the key
func (k *STMKey) GetSubpath() byte {
	return k[KeyLength-2]
}

// NewSTMKey creates a new STMKey from the given address, state, and key type
func newSTMKey(address common.Address, state common.Hash, subpath, keyType byte) STMKey {
	var key STMKey
	copy(key[:common.AddressLength], address.Bytes())
	copy(key[common.AddressLength:KeyLength-1], state.Bytes())
	key[KeyLength-2] = subpath
	key[KeyLength-1] = keyType

	return key
}

// NewAddressKey creates a new STMKey for an address
func NewAddressKey(address common.Address) STMKey {
	return newSTMKey(address, common.Hash{}, 0, AddressType)
}

// NewStateKey creates a new STMKey for a state
func NewStateKey(address common.Address, state common.Hash) STMKey {
	return newSTMKey(address, state, 0, StateType)
}

// NewSubpathKey creates a new STMKey for a subpath
func NewSubpathKey(address common.Address, subpath byte) STMKey {
	return newSTMKey(address, common.Hash{}, subpath, SubpathType)
}

type MVHashMap struct {
	versionedState sync.Map // Key -> *TransactionIndexCells (versioned state with incarnation)
	snapshotState  sync.Map // Key -> value (snapshot for current state of the chain)
}

// Create a new MVHashMap
func NewMVHashMap() *MVHashMap {
	return &MVHashMap{}
}

type WriteCell struct {
	flag        uint // flag for the write cell (DONE or ESTIMATE)
	incarnation int  // retry count
	data        interface{}
}

type TransactionIndexCell struct {
	rmu sync.RWMutex
	tm  *treemap.Map // ordered map of transaction index -> WriteCell
	// since this is a sorted map, we can use it to get the latest state of the state for a single key
}

// NewTransactionIndexCell creates a new TransactionIndexCell
func NewTransactionIndexCell() *TransactionIndexCell {
	return &TransactionIndexCell{
		rmu: sync.RWMutex{},
		tm:  treemap.NewWithIntComparator(),
	}
}

type Version struct {
	TransactionIndex int
	Incarnation      int // retry count
}

// getKeyCells gets the cells for a given key
// if the key is not found, it will return a new TransactionIndexCell with a callback
// if the key is found, it will return the existing TransactionIndexCell
// get or create the cells for a given key
func (mv *MVHashMap) getKeyCells(
	key STMKey,
	ifNoKey func(key STMKey) *TransactionIndexCell,
) *TransactionIndexCell {
	if cells, ok := mv.versionedState.Load(key); ok {
		if cell, ok := cells.(*TransactionIndexCell); ok {
			return cell
		}
	}
	return ifNoKey(key)
}

// function for writing into the mvhashmap
func (mv *MVHashMap) Write(k STMKey, v Version, data interface{}) {
	// get or create the cells for the key
	cells := mv.getKeyCells(k, func(k STMKey) *TransactionIndexCell {
		// create a new transaction index cell
		c := NewTransactionIndexCell()

		// load the transaction index cell
		val, _ := mv.versionedState.LoadOrStore(k, c)

		// return
		cells := val.(*TransactionIndexCell)
		return cells
	})

	cells.rmu.Lock()
	defer cells.rmu.Unlock()

	// add the write cell to the transaction index cell
	if storedCell, ok := cells.tm.Get(v.TransactionIndex); !ok {
		// if there is no cell with this transaction index, we can just add the write cell
		cells.tm.Put(v.TransactionIndex, &WriteCell{flag: DoneFlag, incarnation: v.Incarnation, data: data})
	} else {
		// existing incarnation > new incarnation - this should not be happening
		if storedCell.(*WriteCell).incarnation > v.Incarnation {
			// TODO: handle this in a better way or not ?
			panic("existing incarnation > new incarnation")
		}

		// if not just update the stored cell with new data
		storedCell.(*WriteCell).data = data
		storedCell.(*WriteCell).flag = DoneFlag
		storedCell.(*WriteCell).incarnation = v.Incarnation
	}
}

// ReadStorage reads the storage for a given key from the snapshot state
func (mv *MVHashMap) ReadStorage(k STMKey, fallback func() any) any {
	// reading from snapshot state
	data, ok := mv.snapshotState.Load(k)

	// if not found call the fallback function and store the value
	if !ok {
		data = fallback()
		data, _ = mv.snapshotState.LoadOrStore(k, data)
	}

	// if found return the data
	return data
}

// MarkEstimate marks the given key with the estimate flag
func (mv *MVHashMap) MarkEstimate(k STMKey, txIndex int) {
	cells := mv.getKeyCells(k, func(_ STMKey) *TransactionIndexCell {
		panic("key not found in stm state")
	})

	cells.rmu.Lock()
	defer cells.rmu.Unlock()

	if storedCell, ok := cells.tm.Get(txIndex); !ok {
		// if there is not cell, this should not be happening panic
		msg := fmt.Sprintf("key %s not found in stm state with TxIndex %d, cells key %s", string(k[:]), txIndex, cells.tm.Keys())
		panic(msg)
	} else {
		// if there is a cell with this transaction index, we can just update the flag
		storedCell.(*WriteCell).flag = EstimateFlag
	}
}

// Delete deletes the given key from the mvhashmap
func (mv *MVHashMap) Delete(k STMKey, txIndex int) {
	cells := mv.getKeyCells(k, func(_ STMKey) *TransactionIndexCell {
		// if the key is not found, this should not be happening panic
		msg := fmt.Sprintf("key %s not found in stm state with TxIndex %d", string(k[:]), txIndex)
		panic(msg)
	})

	cells.rmu.Lock()
	defer cells.rmu.Unlock()

	// remove the cell with the given transaction index
	cells.tm.Remove(txIndex)
}

// Flags for mvhashmap read result
const (
	MVReadDone = iota
	MVReadDependency
	MVReadNone
)

type MVReadResult struct {
	dependencyIdx int         // which transaction wrote this value
	incarnation   int         // retry count
	data          interface{} // actual data
}

// DependencyIndex returns the dependency index of the tx
func (mvRes *MVReadResult) DependencyIndex() int {
	return mvRes.dependencyIdx
}

// Incarnation returns the retry count
func (mvRes *MVReadResult) Incarnation() int {
	return mvRes.incarnation
}

// Data returns the data of the result
func (mvRes *MVReadResult) Data() interface{} {
	return mvRes.data
}

// Status returns the status of the read result
func (mvRes *MVReadResult) Status() int {
	// If dependency index is -1 means that the no dependency was found
	if mvRes.dependencyIdx == -1 {
		return MVReadNone
	}

	// if there is a dependency, but incarnation is -1 means that this is an ESTIMATE
	if mvRes.incarnation == -1 {
		return MVReadDependency
	}

	// if dependency and incarnation is valid this is a DONE
	return MVReadDone
}

func (mv *MVHashMap) Read(k STMKey, txIndex int) MVReadResult {
	// starting with default values
	result := MVReadResult{
		dependencyIdx: -1,
		incarnation:   -1,
	}

	cells := mv.getKeyCells(k, func(_ STMKey) *TransactionIndexCell {
		// if not found return nil
		return nil
	})

	// if cells is not found just return the default result
	if cells == nil {
		return result
	}

	cells.rmu.RLock()
	defer cells.rmu.RUnlock()

	// with using .Floor(txIndex - 1) - we can find the latest write cell for the given transaction index
	foundKey, foundValue := cells.tm.Floor(txIndex - 1)

	if foundKey == nil && foundValue == nil {
		// if not found return the default result
		return result
	}

	// if found, we need to check the cells for the given transaction index
	c := foundValue.(*WriteCell)

	// depending on the cell flag
	switch c.flag {
	case DoneFlag:
		{
			result.dependencyIdx = foundKey.(int)
			result.data = c.data
			result.incarnation = c.incarnation
		}
	case EstimateFlag:
		{
			result.dependencyIdx = foundKey.(int)
			result.incarnation = c.incarnation
			result.data = c.data
		}
	default:
		msg := fmt.Sprintf("invalid flag %d for key %s", c.flag, string(k[:]))
		panic(msg)
	}

	return result
}

// FlushMVWriteSet - flushes the WriteDescriptor into mv hashmap
func (mv *MVHashMap) FlushMVWriteSet(writes []WriteOperation) {
	for _, w := range writes {
		mv.Write(w.Path, w.Version, w.Data)
	}
}

// ValidateVersion checks that all reads for a transaction are still valid with the current state of the mvhashmap
func ValidateVersion(txIdx int, lastInputOutput *TransactionInputOutput, versionedData *MVHashMap) (isValid bool) {
	isValid = true

	// get all reads for a given transaction
	for _, rd := range lastInputOutput.ReadSet(txIdx) {
		// read the versioned data for the given key
		mvResult := versionedData.Read(rd.Path, txIdx)
		switch mvResult.Status() {
		case MVReadDone:
			// if its done needs to be read from mvhashmap and the version should match
			isValid = rd.Kind == ReadFromMVHashMap && rd.Version == Version{mvResult.dependencyIdx, mvResult.incarnation}
		case MVReadDependency:
			// if its a dependency, this should not be happening
			isValid = false
		case MVReadNone:
			isValid = rd.Kind == ReadFromState // valid only if its a read from state
		default:
			panic(fmt.Errorf("should not happen - undefined mv read status: %ver", mvResult.Status()))
		}

		if !isValid {
			break
		}
	}

	return
}
