package blockstm

import (
	"fmt"
	"math/big"
	"math/rand"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/stretchr/testify/require"
)

func TestMVHashMap_BasicWriteRead(t *testing.T) {
	t.Parallel()

	mv := NewMVHashMap()
	addr := common.BytesToAddress([]byte("addr"))
	slot := common.BytesToHash([]byte("slot_1"))
	key := NewStateKey(addr, slot)

	// Write initial value with txIdx=1, incarnation=0
	mv.Write(key, Version{TransactionIndex: 1, Incarnation: 0}, 100)
	// Read from txIdx=2 (should see value from txIdx=1)
	res := mv.Read(key, 2)

	if res.Status() != MVReadDone {
		t.Fatalf("expected status=MVReadDone, got %v", res.Status())
	}
	if res.data != 100 {
		t.Fatalf("expected value=100, got %v", res.data)
	}
	if res.dependencyIdx != 1 || res.incarnation != 0 {
		t.Fatalf("expected dependencyIdx=1, incarnation=0, got %d, %d", res.dependencyIdx, res.incarnation)
	}
}
func TestMVHashMap_MultiWriteAndIncarnation(t *testing.T) {
	t.Parallel()

	mv := NewMVHashMap()
	addr := common.BytesToAddress([]byte("addr"))
	slot := common.BytesToHash([]byte("slot_x"))
	key := NewStateKey(addr, slot)

	// First: txIdx=1, incarnation=0
	mv.Write(key, Version{TransactionIndex: 1, Incarnation: 0}, 50)
	// Second: txIdx=2, incarnation=0
	mv.Write(key, Version{TransactionIndex: 2, Incarnation: 0}, 75)
	// Third: txIdx=2, incarnation=1 (retry)
	mv.Write(key, Version{TransactionIndex: 2, Incarnation: 1}, 80)
	// Read: txIdx=3 (should see latest from txIdx=2, incarnation=1)
	res := mv.Read(key, 3)
	if res.Status() != MVReadDone {
		t.Fatalf("expected status=MVReadDone, got %v", res.Status())
	}
	if res.data != 80 {
		t.Fatalf("expected value=80, got %v", res.data)
	}
	if res.dependencyIdx != 2 || res.incarnation != 1 {
		t.Fatalf("expected dependencyIdx=2, incarnation=1, got %d, %d", res.dependencyIdx, res.incarnation)
	}
}
func TestMVHashMap_ReadNoPriorWrite(t *testing.T) {
	mv := NewMVHashMap()
	addr := common.BytesToAddress([]byte("no_prior_addr"))
	slot := common.BytesToHash([]byte("slot_np"))
	key := NewStateKey(addr, slot)
	// Read without any prior write (should be None, fallback to cold state)
	res := mv.Read(key, 1)
	if res.Status() != MVReadNone {
		t.Fatalf("expected status=MVReadNone for missing key, got %v", res.Status())
	}
	if res.dependencyIdx != -1 || res.incarnation != -1 {
		t.Fatalf("expected dependencyIdx and incarnation to be -1 for missing key, got %d, %d", res.dependencyIdx, res.incarnation)
	}
}

func testData(txIdx int, incarnation int) []byte {
	return []byte(fmt.Sprintf("%d-%d", txIdx, incarnation))
}

func TestHelperFunctions(t *testing.T) {
	t.Parallel()

	ap1 := NewAddressKey(common.BytesToAddress([]byte("addr1")))
	ap2 := NewAddressKey(common.BytesToAddress([]byte("addr2")))

	mvh := NewMVHashMap()

	mvh.Write(ap1, Version{0, 1}, testData(0, 1))
	mvh.Write(ap1, Version{0, 2}, testData(0, 2))
	res := mvh.Read(ap1, 0)
	require.Equal(t, -1, res.DependencyIndex())
	require.Equal(t, -1, res.Incarnation())
	require.Equal(t, 2, res.Status())

	mvh.Write(ap2, Version{1, 1}, testData(1, 1))
	mvh.Write(ap2, Version{1, 2}, testData(1, 2))
	res = mvh.Read(ap2, 1)
	require.Equal(t, -1, res.DependencyIndex())
	require.Equal(t, -1, res.Incarnation())
	require.Equal(t, 2, res.Status())

	mvh.Write(ap1, Version{2, 1}, testData(2, 1))
	mvh.Write(ap1, Version{2, 2}, testData(2, 2))
	res = mvh.Read(ap1, 2)
	require.Equal(t, 0, res.DependencyIndex())
	require.Equal(t, 2, res.Incarnation())
	require.Equal(t, testData(0, 2), res.Data())
	require.Equal(t, 0, res.Status())
}

func TestMVHashMap_MarkEstimate(t *testing.T) {
	t.Parallel()

	mv := NewMVHashMap()
	addr := common.BytesToAddress([]byte("addr"))
	slot := common.BytesToHash([]byte("slot_x"))
	key := NewStateKey(addr, slot)

	mv.Write(key, Version{0, 0}, testData(0, 1))
	result := mv.Read(key, 0)
	require.Equal(t, -1, result.DependencyIndex())
	require.Equal(t, -1, result.Incarnation())
	require.Equal(t, MVReadNone, result.Status())

	mv.MarkEstimate(key, 0)
	result = mv.Read(key, 0)
	require.Equal(t, -1, result.DependencyIndex())
	require.Equal(t, -1, result.Incarnation())
}

func TestMvHashMap_LowerIncarnation(t *testing.T) {
	t.Parallel()

	mv := NewMVHashMap()
	addr := common.BytesToAddress([]byte("addr"))
	slot := common.BytesToHash([]byte("slot_x"))
	key := NewStateKey(addr, slot)

	mv.Write(key, Version{0, 0}, testData(0, 1))

	// Test that writing with lower incarnation panics
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("Expected panic for lower incarnation, but no panic occurred")
		} else {
			expectedMsg := "existing incarnation > new incarnation"
			if r != expectedMsg {
				t.Errorf("Expected panic message '%s', got '%v'", expectedMsg, r)
			}
		}
	}()

	mv.Write(key, Version{0, -1}, testData(0, 2))
}

var randomness = rand.Intn(10) + 10

// create test data for a given txIdx and incarnation
func valueFor(txIdx, inc int) []byte {
	return []byte(fmt.Sprintf("%ver:%ver:%ver", txIdx*5, txIdx+inc, inc*5))
}

func getCommonAddress(i int) common.Address {
	return common.BigToAddress(big.NewInt(int64(i % randomness)))
}

func TestFlushMVWrite(t *testing.T) {
	t.Parallel()

	ap1 := NewAddressKey(getCommonAddress(1))
	ap2 := NewAddressKey(getCommonAddress(2))

	mvh := NewMVHashMap()

	var res MVReadResult

	wd := []WriteOperation{}

	wd = append(wd, WriteOperation{
		Path:    ap1,
		Version: Version{0, 1},
		Data:    valueFor(0, 1),
	})
	wd = append(wd, WriteOperation{
		Path:    ap1,
		Version: Version{0, 2},
		Data:    valueFor(0, 2),
	})
	wd = append(wd, WriteOperation{
		Path:    ap2,
		Version: Version{1, 1},
		Data:    valueFor(1, 1),
	})
	wd = append(wd, WriteOperation{
		Path:    ap2,
		Version: Version{1, 2},
		Data:    valueFor(1, 2),
	})
	wd = append(wd, WriteOperation{
		Path:    ap1,
		Version: Version{2, 1},
		Data:    valueFor(2, 1),
	})
	wd = append(wd, WriteOperation{
		Path:    ap1,
		Version: Version{2, 2},
		Data:    valueFor(2, 2),
	})

	mvh.FlushMVWriteSet(wd)

	res = mvh.Read(ap1, 0)
	require.Equal(t, -1, res.DependencyIndex())
	require.Equal(t, -1, res.Incarnation())
	require.Equal(t, 2, res.Status())

	res = mvh.Read(ap2, 1)
	require.Equal(t, -1, res.DependencyIndex())
	require.Equal(t, -1, res.Incarnation())
	require.Equal(t, 2, res.Status())

	res = mvh.Read(ap1, 2)
	require.Equal(t, 0, res.DependencyIndex())
	require.Equal(t, 2, res.Incarnation())
	require.Equal(t, valueFor(0, 2), res.Data())
	require.Equal(t, 0, res.Status())
}

// TODO - handle panic
func TestLowerIncarnation(t *testing.T) {
	t.Parallel()

	ap1 := NewAddressKey(getCommonAddress(1))

	mvh := NewMVHashMap()

	mvh.Write(ap1, Version{0, 2}, valueFor(0, 2))
	mvh.Read(ap1, 0)
	mvh.Write(ap1, Version{1, 2}, valueFor(1, 2))
	mvh.Write(ap1, Version{0, 5}, valueFor(0, 5))
	mvh.Write(ap1, Version{1, 5}, valueFor(1, 5))
}

func TestMarkEstimate(t *testing.T) {
	t.Parallel()

	ap1 := NewAddressKey(getCommonAddress(1))

	mvh := NewMVHashMap()

	mvh.Write(ap1, Version{7, 2}, valueFor(7, 2))
	mvh.MarkEstimate(ap1, 7)
	mvh.Write(ap1, Version{7, 4}, valueFor(7, 4))
}

func TestMVHashMapBasics(t *testing.T) {
	t.Parallel()

	// memory locations
	ap1 := NewAddressKey(getCommonAddress(1))
	ap2 := NewAddressKey(getCommonAddress(2))
	ap3 := NewAddressKey(getCommonAddress(3))

	mvh := NewMVHashMap()

	res := mvh.Read(ap1, 5)
	require.Equal(t, -1, res.DependencyIndex())

	mvh.Write(ap1, Version{10, 1}, valueFor(10, 1))

	res = mvh.Read(ap1, 9)
	require.Equal(t, -1, res.DependencyIndex(), "reads that should go the DB return dependency -1")
	res = mvh.Read(ap1, 10)
	require.Equal(t, -1, res.DependencyIndex(), "Read returns entries from smaller txns, not txn 10")

	// Reads for a higher txn return the entry written by txn 10.
	res = mvh.Read(ap1, 15)
	require.Equal(t, 10, res.DependencyIndex(), "reads for a higher txn return the entry written by txn 10.")
	require.Equal(t, 1, res.Incarnation())
	require.Equal(t, valueFor(10, 1), res.Data())

	// More writes.
	mvh.Write(ap1, Version{12, 0}, valueFor(12, 0))
	mvh.Write(ap1, Version{8, 3}, valueFor(8, 3))

	// Verify reads.
	res = mvh.Read(ap1, 15)
	require.Equal(t, 12, res.DependencyIndex())
	require.Equal(t, 0, res.Incarnation())
	require.Equal(t, valueFor(12, 0), res.Data())

	res = mvh.Read(ap1, 11)
	require.Equal(t, 10, res.DependencyIndex())
	require.Equal(t, 1, res.Incarnation())
	require.Equal(t, valueFor(10, 1), res.Data())

	res = mvh.Read(ap1, 10)
	require.Equal(t, 8, res.DependencyIndex())
	require.Equal(t, 3, res.Incarnation())
	require.Equal(t, valueFor(8, 3), res.Data())

	// Mark the entry written by 10 as an estimate.
	mvh.MarkEstimate(ap1, 10)

	res = mvh.Read(ap1, 11)
	require.Equal(t, 10, res.DependencyIndex())
	require.Equal(t, -1, res.Incarnation(), "dep at tx 10 is now an estimate")

	// Delete the entry written by 10, write to a different ap.
	mvh.Delete(ap1, 10)
	mvh.Write(ap2, Version{10, 2}, valueFor(10, 2))

	// Read by txn 11 no longer observes entry from txn 10.
	res = mvh.Read(ap1, 11)
	require.Equal(t, 8, res.DependencyIndex())
	require.Equal(t, 3, res.Incarnation())
	require.Equal(t, valueFor(8, 3), res.Data())

	// Reads, writes for ap2 and ap3.
	mvh.Write(ap2, Version{5, 0}, valueFor(5, 0))
	mvh.Write(ap3, Version{20, 4}, valueFor(20, 4))

	res = mvh.Read(ap2, 10)
	require.Equal(t, 5, res.DependencyIndex())
	require.Equal(t, 0, res.Incarnation())
	require.Equal(t, valueFor(5, 0), res.Data())

	res = mvh.Read(ap3, 21)
	require.Equal(t, 20, res.DependencyIndex())
	require.Equal(t, 4, res.Incarnation())
	require.Equal(t, valueFor(20, 4), res.Data())

	// Clear ap1 and ap3.
	mvh.Delete(ap1, 12)
	mvh.Delete(ap1, 8)
	mvh.Delete(ap3, 20)

	// Reads from ap1 and ap3 go to db.
	res = mvh.Read(ap1, 30)
	require.Equal(t, -1, res.DependencyIndex())

	res = mvh.Read(ap3, 30)
	require.Equal(t, -1, res.DependencyIndex())

	// No-op delete at ap2 - doesn't panic because ap2 does exist
	mvh.Delete(ap2, 11)

	// Read entry by txn 10 at ap2.
	res = mvh.Read(ap2, 15)
	require.Equal(t, 10, res.DependencyIndex())
	require.Equal(t, 2, res.Incarnation())
	require.Equal(t, valueFor(10, 2), res.Data())
}
