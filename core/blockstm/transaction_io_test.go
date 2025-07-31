package blockstm

import (
	"testing"
)

func TestTransactionInputOutput_Basic(t *testing.T) {
	numTx := 3
	tio := NewTransactionInputOutput(numTx)

	keyA := STMKey{1}
	keyB := STMKey{2}
	keyC := STMKey{3}
	keyD := STMKey{4}

	read0 := []ReadOperation{{Path: keyA, Kind: ReadFromMVHashMap}}
	read1 := []ReadOperation{{Path: keyB, Kind: ReadFromState}}
	write0 := []WriteOperation{{Path: keyC, Version: Version{0, 0}, Data: "valC"}}
	write1 := []WriteOperation{{Path: keyD, Version: Version{1, 0}, Data: "valD"}}

	tio.recordRead(0, read0)
	tio.recordRead(1, read1)
	tio.recordWrite(0, write0)
	tio.recordWrite(1, write1)
	tio.recordAllWrite(0, write0)
	tio.recordAllWrite(1, write1)

	if got := tio.ReadSet(0); len(got) != 1 || got[0].Path != keyA {
		t.Errorf("ReadSet(0) failed: got %+v", got)
	}
	if got := tio.WriteSet(1); len(got) != 1 || got[0].Path != keyD {
		t.Errorf("WriteSet(1) failed: got %+v", got)
	}
	if got := tio.AllWriteSet(1); len(got) != 1 || got[0].Path != keyD {
		t.Errorf("AllWriteSet(1) failed: got %+v", got)
	}

	if !tio.HasWritten(1, keyD) {
		t.Errorf("HasWritten(1, keyD) should be true")
	}
	if tio.HasWritten(1, keyC) {
		t.Errorf("HasWritten(1, keyC) should be false")
	}

	// should return false for out of range txIndex
	if tio.HasWritten(99, keyA) {
		t.Errorf("HasWritten(99, keyA) should be false for out of range")
	}

	// test hasNewWrite
	txOut := TransactionOutput{WriteOperation{Path: keyA}, WriteOperation{Path: keyB}}
	cmpSet := []WriteOperation{{Path: keyA}}
	if !txOut.hasNewWrite(cmpSet) {
		t.Errorf("hasNewWrite should return true when there's a new write")
	}

	cmpSet2 := []WriteOperation{{Path: keyA}, {Path: keyB}}
	if txOut.hasNewWrite(cmpSet2) {
		t.Errorf("hasNewWrite should return false when all writes are present")
	}
}

func TestTransactionInputOutput_RecordAtOnce(t *testing.T) {
	numTx := 2
	tio := NewTransactionInputOutput(numTx)

	keyA := STMKey{10}
	keyB := STMKey{20}

	allReads := [][]ReadOperation{
		{{Path: keyA, Kind: ReadFromMVHashMap}},
		{{Path: keyB, Kind: ReadFromState}},
	}
	allWrites := [][]WriteOperation{
		{{Path: keyB, Version: Version{0, 1}, Data: "foo"}},
		{{Path: keyA, Version: Version{1, 1}, Data: "bar"}},
	}
	tio.RecordReadAtOnce(allReads)
	tio.RecordWriteAtOnce(allWrites)

	if got := tio.ReadSet(1); len(got) != 1 || got[0].Path != keyB {
		t.Errorf("RecordReadAtOnce failed for tx 1: got %+v", got)
	}
	if got := tio.WriteSet(0); len(got) != 1 || got[0].Path != keyB {
		t.Errorf("RecordWriteAtOnce failed for tx 0: got %+v", got)
	}
}
