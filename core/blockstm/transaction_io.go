package blockstm

const (
	ReadFromMVHashMap = iota // read operation from stm multi version hash map
	ReadFromState            // read operation from latest state snapshot
)

type ReadOperation struct {
	Path    STMKey // the path to the key to read from the multi version hash map
	Kind    int    // the kind of read operation (ReadFromMVHashMap or ReadFromState)
	Version Version
}

type WriteOperation struct {
	Path    STMKey // the path to the key to write to the multi version hash map
	Version Version
	Data    interface{} // the data to write to the key
}

type TransactionInput []ReadOperation   // list of all read operations for single transaction
type TransactionOutput []WriteOperation // list of all write operations for single transaction

// hasNewWrite returns true if current set has a new write compared to the input
func (txOut TransactionOutput) hasNewWrite(compareSet []WriteOperation) bool {
	// if there is no write operation in the transaction output, return false
	if len(txOut) == 0 {
		return false
	}

	if len(compareSet) == 0 || len(txOut) > len(compareSet) {
		return true
	}

	compMap := make(map[STMKey]bool, len(compareSet))
	// add all the write operations in the compare set to the map
	for _, w := range compareSet {
		compMap[w.Path] = true
	}

	// check if any write operation in the transaction output is not in the compare set
	for _, v := range txOut {
		if !compMap[v.Path] {
			return true
		}
	}

	return false
}

// TransactionInputOutput is block-scoped structure that stores all the read and write operations for every transaction in a block
type TransactionInputOutput struct {
	inputs     []TransactionInput
	outputs    []TransactionOutput   // write sets that should be checked during validation
	outputsSet []map[STMKey]struct{} // set of all write operations in the outputs (map for fast lookup)
	allOutputs []TransactionOutput   // entire set of write operations, must be a parent set of outputs
}

// NewTransactionInputOutput creates a new TransactionInputOutput
func NewTransactionInputOutput(numOfTx int) *TransactionInputOutput {
	return &TransactionInputOutput{
		inputs:     make([]TransactionInput, numOfTx),
		outputs:    make([]TransactionOutput, numOfTx),
		outputsSet: make([]map[STMKey]struct{}, numOfTx),
		allOutputs: make([]TransactionOutput, numOfTx),
	}
}

// ReadSet returns all read operations for a given transaction
func (tio *TransactionInputOutput) ReadSet(txnIndex int) []ReadOperation {
	return tio.inputs[txnIndex]
}

// WriteSet returns all write operations for a given transaction
func (tio *TransactionInputOutput) WriteSet(txnIndex int) []WriteOperation {
	return tio.outputs[txnIndex]
}

// AllWriteSet returns all write operations (including internal) for a given transaction.
func (tio *TransactionInputOutput) AllWriteSet(txIndex int) []WriteOperation {
	return tio.allOutputs[txIndex]
}

// HasWritten return true if the key has been written to in the transaction
func (tio *TransactionInputOutput) HasWritten(txIndex int, key STMKey) bool {
	// if the txIndex is out of bounds, return false
	if txIndex < 0 || txIndex >= len(tio.outputsSet) {
		return false
	}
	// if the outputs set is not initialized, return false
	if tio.outputsSet[txIndex] == nil {
		return false
	}

	_, ok := tio.outputsSet[txIndex][key]
	return ok
}

// recordRead records the read operations for a given transaction
func (tio *TransactionInputOutput) recordRead(txIndex int, read []ReadOperation) {
	tio.inputs[txIndex] = read
}

// recordWrite records the write operations for a given transaction
func (tio *TransactionInputOutput) recordWrite(txIndex int, write []WriteOperation) {
	tio.outputs[txIndex] = write
	tio.outputsSet[txIndex] = make(map[STMKey]struct{}, len(write))

	for _, w := range write {
		tio.outputsSet[txIndex][w.Path] = struct{}{}
	}
}

// recordAllWrite records all write operations (including internal or speculative writes)”
func (tio *TransactionInputOutput) recordAllWrite(txIndex int, write []WriteOperation) {
	tio.allOutputs[txIndex] = write
}

// RecordReadAtOnce records the read operations for all transactions at once in a given block
func (tio *TransactionInputOutput) RecordReadAtOnce(inputs [][]ReadOperation) {
	for i, read := range inputs {
		tio.recordRead(i, read)
	}
}

// RecordWriteAtOnce records the write operations for all transactions at once in a given block
func (tio *TransactionInputOutput) RecordWriteAtOnce(inputs [][]WriteOperation) {
	for i, write := range inputs {
		tio.recordWrite(i, write)
	}
}
