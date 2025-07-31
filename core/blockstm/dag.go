package blockstm

import (
	"fmt"
	"strings"
	"time"

	"github.com/ava-labs/libevm/log"
	"github.com/heimdalr/dag"
)

// DAG is used for tracking the dependencies between transactions in a block
// each node represents a single transaction in the block
// each edge represents dependency between two transactions
// whole DAG represents a single block (block-stm)
type DAG struct {
	*dag.DAG
}

// ensureVertex checks that the vertex is added to the DAG and returns the id of the vertex
func (d *DAG) ensureVertex(ids map[int]string, index int) string {
	if _, ok := ids[index]; !ok {
		id, _ := d.AddVertex(index)
		ids[index] = id
	}
	return ids[index]
}

type TransactionDependency struct {
	Index         int                // transaction index in the block
	ReadList      []ReadOperation    // all the read operations for the transaction
	FullWriteList [][]WriteOperation // contains the write sets of all previous transactions in the block (allows us to check for read dependencies much faster)
	// FullWriteList - [priorTxIndex] - []WriteOperation by that transaction
}

// HasReadDependency checks that if txTo reads any key from txFrom
func HasReadDependency(txFrom TransactionOutput, txTo TransactionInput) bool {
	reads := make(map[STMKey]bool)
	// map all the read operations in the txTo
	for _, read := range txTo {
		reads[read.Path] = true
	}

	// check if any read operation in the txFrom is in the txTo
	for _, rd := range txFrom {
		// if the read operation is in the txTo, return true
		if _, ok := reads[rd.Path]; ok {
			return true
		}
	}

	return false
}

// BuildDAG builds the DAG for a given block
func BuildDAG(dependencies TransactionInputOutput) DAG {
	d := DAG{dag.NewDAG()}
	ids := make(map[int]string)

	for i := len(dependencies.inputs) - 1; i > 0; i-- {
		txTo := dependencies.inputs[i]

		// make sure every transaction has a vertex in the DAG
		txToId := d.ensureVertex(ids, i)

		// iterate over all the previous transactions from this transaction
		for j := i - 1; j >= 0; j-- {
			txFrom := dependencies.allOutputs[j] // txFrom means that previous transaction write operations
			// with this, we can get the dependency between txTo and txFrom

			// if there is a dependency
			if HasReadDependency(txFrom, txTo) {
				// make sure the vertex exists in the dag for the previous transaction
				txFromId := d.ensureVertex(ids, j)
				err := d.AddEdge(txFromId, txToId)
				if err != nil {
					log.Warn("Error adding edge to DAG from tx", "txFrom", txFromId, "txTo", txToId, "error", err)
				}
			}

		}
	}

	return d
}

// depsHelper is a helper function that checks if there is a dependency between two transactions
// if such a dependency exists, it updates the dependency map
func depsHelper(dependencies map[int]map[int]bool, txFrom TransactionOutput, txTo TransactionInput, i, j int) map[int]map[int]bool {
	// check if there is a dependency, if not return the deps as it is
	if HasReadDependency(txFrom, txTo) {
		// add the dependency
		dependencies[i][j] = true

		// remove redundant dependencies
		for k := range dependencies[i] {
			_, found := dependencies[k][j]

			if found {
				delete(dependencies[i], k)
			}
		}
	}

	return dependencies
}

// UpdateDependencies updates the dependencies for a given transaction
func UpdateDependencies(dependencies map[int]map[int]bool, txDep TransactionDependency) map[int]map[int]bool {
	txTo := txDep.ReadList
	dependencies[txDep.Index] = make(map[int]bool)

	for j := 0; j < txDep.Index; j++ {
		txFrom := txDep.FullWriteList[j]
		dependencies = depsHelper(dependencies, txFrom, txTo, txDep.Index, j)
	}

	return dependencies
}

// GetBlockDependencies returns the dependencies for a given block
func GetBlockDependencies(dependencies TransactionInputOutput) map[int]map[int]bool {
	deps := map[int]map[int]bool{}

	for i := 1; i < len(dependencies.inputs); i++ {
		txTo := dependencies.inputs[i]
		deps[i] = map[int]bool{}

		// iterate over all the previous transactions from this transaction
		for j := i - 1; j >= 0; j-- {
			txFrom := dependencies.allOutputs[j]
			deps = depsHelper(deps, txFrom, txTo, i, j)
		}
	}

	return deps
}

// LongestPath returns the longest path in the DAG and the weight of the path
// the path is a list of transaction indices in the ascending order
// the weight is the sum of the execution times of the transactions in the path
func (d DAG) LongestPath(stats map[int]ExecutionStat) ([]int, uint64) {
	prev := make(map[int]int, len(d.GetVertices()))

	for i := 0; i < len(d.GetVertices()); i++ {
		prev[i] = -1
	}

	pathWeights := make(map[int]uint64, len(d.GetVertices()))

	maxPath := 0
	maxPathWeight := uint64(0)

	idxToId := make(map[int]string, len(d.GetVertices()))

	for k, i := range d.GetVertices() {
		idxToId[i.(int)] = k
	}

	for i := 0; i < len(idxToId); i++ {
		parents, _ := d.GetParents(idxToId[i])

		if len(parents) > 0 {
			for _, p := range parents {
				weight := pathWeights[p.(int)] + stats[i].End - stats[i].Start
				if weight > pathWeights[i] {
					pathWeights[i] = weight
					prev[i] = p.(int)
				}
			}
		} else {
			pathWeights[i] = stats[i].End - stats[i].Start
		}

		if pathWeights[i] > maxPathWeight {
			maxPath = i
			maxPathWeight = pathWeights[i]
		}
	}

	path := make([]int, 0)
	for i := maxPath; i != -1; i = prev[i] {
		path = append(path, i)
	}

	// Reverse the path so the transactions are in the ascending order
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}

	return path, maxPathWeight
}

func (d DAG) Report(stats map[int]ExecutionStat, out func(string)) {
	longestPath, weight := d.LongestPath(stats)

	serialWeight := uint64(0)

	for i := 0; i < len(d.GetVertices()); i++ {
		serialWeight += stats[i].End - stats[i].Start
	}

	makeStrs := func(ints []int) (ret []string) {
		for _, v := range ints {
			ret = append(ret, fmt.Sprint(v))
		}

		return
	}

	out("Longest execution path:")
	out(fmt.Sprintf("(%v) %v", len(longestPath), strings.Join(makeStrs(longestPath), "->")))

	out(fmt.Sprintf("Longest path ideal execution time: %v of %v (serial total), %v%%", time.Duration(weight),
		time.Duration(serialWeight), fmt.Sprintf("%.1f", float64(weight)*100.0/float64(serialWeight))))
}
