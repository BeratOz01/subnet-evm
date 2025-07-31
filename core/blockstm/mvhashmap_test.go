package blockstm

import (
	"fmt"
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
