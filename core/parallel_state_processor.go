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
// Copyright 2015 The go-ethereum Authors
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

package core

import (
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/log"
	"github.com/ava-labs/libevm/metrics"
	ethparams "github.com/ava-labs/libevm/params"
	"github.com/ava-labs/subnet-evm/consensus"
	"github.com/ava-labs/subnet-evm/core/blockstm"
	"github.com/ava-labs/subnet-evm/core/state"
	"github.com/ava-labs/subnet-evm/params"
	"github.com/ava-labs/subnet-evm/plugin/evm/customtypes"
)

// TODO: implement configuration for parallel execution
type ParallelEVMConfig struct {
	Enable   bool // enable parallel execution
	NumProcs int  // number of processors to use
	Enforce  bool // enforce parallel execution
}

// ParallelStateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// ParallelStateProcessor implements Processor.
type ParallelStateProcessor struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for block rewards
}

// NewParallelStateProcessor initializes a new ParallelStateProcessor.
func NewParallelStateProcessor(config *params.ChainConfig, bc *BlockChain, engine consensus.Engine) *ParallelStateProcessor {
	return &ParallelStateProcessor{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

// ExecutionTask is a transaction that needs to be executed with parallel executor.
type ExecutionTask struct {
	msg    Message
	config *params.ChainConfig

	gasLimit                   uint64
	blockNumber                *big.Int
	blockHash                  common.Hash
	tx                         *types.Transaction
	index                      int
	statedb                    *state.StateDB // state database that stores the modified values after tx execution
	cleanStatedb               *state.StateDB // a clean copy of the initial statedb (should not be modified)
	finalStatedb               *state.StateDB // the final state database after tx execution
	header                     *types.Header
	blockChain                 *BlockChain
	evmConfig                  vm.Config
	result                     *ExecutionResult
	shouldRerunWithoutFeeDelay bool // ??
	sender                     common.Address
	totalUsedGas               *uint64
	receipts                   *types.Receipts
	allLogs                    *[]*types.Log

	// length of dependencies          -> 2 + k (k = a whole number)
	// first 2 element in dependencies -> transaction index, and flag representing if delay is allowed or not
	//                                       (0 -> delay is not allowed, 1 -> delay is allowed)
	// next k elements in dependencies -> transaction indexes on which transaction i is dependent on
	dependencies []int
	coinbase     common.Address
	blockContext vm.BlockContext
}

// Execute is the main function that executes the transaction but not settle the tx
// it is called by the parallel executor
func (task *ExecutionTask) Execute(mvh *blockstm.MVHashMap, incarnation int) (err error) {
	now := time.Now()
	log.Info("[PARALLEL PROCESSOR] Executing transaction", "tx", task.tx.Hash().Hex())

	// copy the clean statedb to the statedb
	task.statedb = task.cleanStatedb.Copy()
	task.statedb.SetTxContext(task.tx.Hash(), task.index)
	task.statedb.SetMVHashMap(mvh)
	task.statedb.SetIncarnation(incarnation)

	// create new context to be used in the EVM environment
	txContext := NewEVMTxContext(&task.msg)

	// create new evm instance
	evm := vm.NewEVM(task.blockContext, txContext, task.statedb, task.config, task.evmConfig)

	defer func() {
		if r := recover(); r != nil {
			// recover the panic and retry the execution
			log.Error("panic in parallel execution", "err", r)

			err = blockstm.ExecutionAbortError{Dependency: task.statedb.DepTxIndex()}
			return
		}
	}()

	// execute the transaction
	task.result, err = ApplyMessage(evm, &task.msg, new(GasPool).AddGas(task.gasLimit))
	if task.statedb.HadInvalidRead() || err != nil {
		err = blockstm.ExecutionAbortError{Dependency: task.statedb.DepTxIndex(), OriginError: err}
		return
	}

	// finalize the state in backup state db
	// will commit all changes to the statedb at the end of the block execution
	task.statedb.Finalise(task.config.IsEIP158(task.blockNumber))
	log.Info("[PARALLEL PROCESSOR] Transaction executed", "tx", task.tx.Hash().Hex(), "duration", time.Since(now))

	return
}

func (task *ExecutionTask) MVReadList() []blockstm.ReadOperation {
	return task.statedb.MVReadList()
}

func (task *ExecutionTask) MVWriteList() []blockstm.WriteOperation {
	return task.statedb.MVWriteList()
}

func (task *ExecutionTask) MVFullWriteList() []blockstm.WriteOperation {
	return task.statedb.MVFullWriteList()
}

func (task *ExecutionTask) Sender() common.Address {
	return task.sender
}

func (task *ExecutionTask) Hash() common.Hash {
	return task.tx.Hash()
}

func (task *ExecutionTask) Dependencies() []int {
	return task.dependencies
}

// Settle is the function that settles the transaction
func (task *ExecutionTask) Settle() {
	now := time.Now()
	log.Info("[PARALLEL PROCESSOR] Settling transaction", "tx", task.tx.Hash().Hex())

	// set the tx context to the final statedb
	task.finalStatedb.SetTxContext(task.tx.Hash(), task.index)

	// apply the write set to the final statedb
	task.finalStatedb.ApplyMVWriteSet(task.statedb.MVFullWriteList())

	// add logs
	for _, l := range task.statedb.GetLogs(task.tx.Hash(), task.blockNumber.Uint64(), task.blockHash) {
		task.finalStatedb.AddLog(l)
	}

	// if preimage recording is enabled, add the preimages to the final statedb
	if task.evmConfig.EnablePreimageRecording {
		// add preimages
		for k, v := range task.statedb.Preimages() {
			task.finalStatedb.AddPreimage(k, v)
		}
	}

	// update the state with pending changes
	// var root []byte
	// if task.config.IsByzantium(task.blockNumber) {
	// 	task.finalStatedb.Finalise(true)
	// } else {
	// 	root = task.finalStatedb.IntermediateRoot(task.config.IsEIP158(task.blockNumber)).Bytes()
	// }
	root := task.finalStatedb.IntermediateRoot(task.config.IsEIP158(task.blockNumber)).Bytes()

	*task.totalUsedGas += task.result.UsedGas

	// Create a new receipt for the transaction, storing the intermediate root and gas used
	// by the tx.
	receipt := &types.Receipt{Type: task.tx.Type(), PostState: root, CumulativeGasUsed: *task.totalUsedGas}
	if task.result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	receipt.TxHash = task.tx.Hash()
	receipt.GasUsed = task.result.UsedGas

	if task.tx.Type() == types.BlobTxType {
		receipt.BlobGasUsed = uint64(len(task.tx.BlobHashes()) * ethparams.BlobTxBlobGasPerBlob)
		receipt.BlobGasPrice = task.blockContext.BlobBaseFee
	}

	// If the transaction created a contract, store the creation address in the receipt.
	if task.msg.To == nil {
		receipt.ContractAddress = crypto.CreateAddress(task.msg.From, task.tx.Nonce())
	}

	// Set the receipt logs and create the bloom filter.
	receipt.Logs = task.finalStatedb.GetLogs(task.tx.Hash(), task.blockNumber.Uint64(), task.blockHash)
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
	receipt.BlockHash = task.blockHash
	receipt.BlockNumber = task.blockNumber
	receipt.TransactionIndex = uint(task.finalStatedb.TxIndex())

	*task.receipts = append(*task.receipts, receipt)
	*task.allLogs = append(*task.allLogs, task.finalStatedb.Logs()...)

	log.Info("[PARALLEL PROCESSOR] Transaction settled", "tx", task.tx.Hash().Hex(), "duration", time.Since(now))
}

var parallelizabilityTimer = metrics.NewRegisteredTimer("block/parallelizability", nil)

func (p *ParallelStateProcessor) Process(block *types.Block, parent *types.Header, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error) {
	now := time.Now()
	log.Info("[PARALLEL PROCESSOR] Processing block", "block", block.Hash().Hex())

	var (
		receipts types.Receipts
		usedGas  = new(uint64)
		header   = block.Header()
		allLogs  []*types.Log
	)

	// get tx dependencies from header
	txDependencies := customtypes.TxDependency(block)
	deps := getDeps(txDependencies)

	// verify tx dependencies
	if !verifyDeps(deps) {
		log.Info("Invalid tx dependencies", "deps", deps)
		return nil, nil, 0, fmt.Errorf("invalid tx dependencies")
	}

	// Configure any upgrades that should go into effect during this block.
	blockContext := NewBlockContext(block.Number(), block.Time())
	err := ApplyUpgrades(p.config, &parent.Time, blockContext, statedb)
	if err != nil {
		log.Error("failed to configure precompiles processing block", "hash", block.Hash(), "number", block.NumberU64(), "timestamp", block.Time(), "err", err)
		return nil, nil, 0, err
	}

	log.Info("Transaction dependencies fetched successfully", "deps", deps)

	var (
		context = NewEVMBlockContext(header, p.bc, nil)
		vmenv   = vm.NewEVM(context, vm.TxContext{}, statedb, p.config, cfg)
		signer  = types.MakeSigner(p.config, header.Number, header.Time)
	)
	if beaconRoot := block.BeaconRoot(); beaconRoot != nil {
		ProcessBeaconBlockRoot(*beaconRoot, vmenv, statedb)
	}

	// Create execution tasks for all transactions
	tasks := make([]blockstm.ExecutionTask, 0, len(block.Transactions()))

	// Iterate over and create execution tasks for individual transactions
	for i, tx := range block.Transactions() {
		msg, err := TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}

		// also skip the account checks
		msg.SkipAccountChecks = true

		// get clean statedb
		cleanState := statedb.Copy()

		// Get dependencies for this transaction
		txDeps := []int{}
		if depsForTx, exists := deps[i]; exists {
			txDeps = depsForTx
		}

		// create new execution task
		task := &ExecutionTask{
			msg:          *msg,
			config:       p.config,
			gasLimit:     block.GasLimit(),
			blockNumber:  block.Number(),
			blockHash:    block.Hash(),
			tx:           tx,
			index:        i,
			cleanStatedb: cleanState,
			finalStatedb: statedb,
			header:       header,
			blockChain:   p.bc,
			evmConfig:    cfg,
			sender:       msg.From,
			totalUsedGas: usedGas,
			receipts:     &receipts,
			allLogs:      &allLogs,
			dependencies: txDeps,
			coinbase:     header.Coinbase,
			blockContext: context,
		}

		tasks = append(tasks, task)
	}

	log.Info("Tasks created successfully", "tasks", len(tasks))

	start := time.Now()

	// TODO: configure the number of processors
	_, err = blockstm.ExecuteParallel(tasks, 5, nil)
	if err != nil {
		return nil, nil, 0, err
	}

	// Sort receipts by transaction index
	sort.Slice(receipts, func(i, j int) bool {
		return receipts[i].TransactionIndex < receipts[j].TransactionIndex
	})

	log.Info("Parallel execution", "tasks", len(tasks))

	// _, weight := result.Deps.LongestPath(*result.Stats)
	// serialWeight := uint64(0)
	// for i := 0; i < len(result.Deps.GetVertices()); i++ {
	// 	serialWeight += (*result.Stats)[i].End - (*result.Stats)[i].Start
	// }
	// log.Info("Parallel execution weight", "weight", weight, "serialWeight", serialWeight)
	// parallelizability := time.Duration(serialWeight * 100 / weight)
	// log.Info("Parallel execution parallelizability", "parallelizability", parallelizability)
	// parallelizabilityTimer.Update(parallelizability)

	duration := time.Since(start)
	log.Info("Parallel execution completed",
		"duration", duration,
		"task_count", len(tasks),
		"avg_time_per_tx", duration/time.Duration(len(tasks)),
		"receipts", len(receipts))

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	if err = p.engine.Finalize(p.bc, block, parent, statedb, receipts); err != nil {
		return nil, nil, 0, fmt.Errorf("engine finalization check failed: %w", err)
	}

	log.Info("[PARALLEL PROCESSOR] Block processed", "block", block.Hash().Hex(), "duration", time.Since(now))

	return receipts, allLogs, *usedGas, nil
}

func getDeps(txDependency [][]uint64) map[int][]int {
	deps := make(map[int][]int)

	for i := 0; i <= len(txDependency)-1; i++ {
		deps[i] = []int{}

		for j := 0; j <= len(txDependency[i])-1; j++ {
			deps[i] = append(deps[i], int(txDependency[i][j]))
		}
	}

	return deps
}

// returns true if dependencies are correct
func verifyDeps(deps map[int][]int) bool {
	// number of transactions in the block
	n := len(deps)

	// Handle out-of-range and circular dependency problem
	for i := 0; i <= n-1; i++ {
		val := deps[i]
		for _, depTx := range val {
			if depTx >= n || depTx >= i {
				return false
			}
		}
	}

	return true
}

// NewExecutionTask creates a new ExecutionTask with all fields properly initialized
func NewExecutionTask(
	msg Message,
	config *params.ChainConfig,
	gasLimit uint64,
	blockNumber *big.Int,
	blockHash common.Hash,
	tx *types.Transaction,
	index int,
	cleanStatedb *state.StateDB,
	finalStatedb *state.StateDB,
	header *types.Header,
	blockChain *BlockChain,
	evmConfig vm.Config,
	sender common.Address,
	totalUsedGas *uint64,
	receipts *types.Receipts,
	allLogs *[]*types.Log,
	dependencies []int,
	coinbase common.Address,
	blockContext vm.BlockContext,
) ExecutionTask {
	return ExecutionTask{
		msg:                        msg,
		config:                     config,
		gasLimit:                   gasLimit,
		blockNumber:                blockNumber,
		blockHash:                  blockHash,
		tx:                         tx,
		index:                      index,
		cleanStatedb:               cleanStatedb,
		finalStatedb:               finalStatedb,
		header:                     header,
		blockChain:                 blockChain,
		evmConfig:                  evmConfig,
		sender:                     sender,
		totalUsedGas:               totalUsedGas,
		receipts:                   receipts,
		allLogs:                    allLogs,
		dependencies:               dependencies,
		coinbase:                   coinbase,
		blockContext:               blockContext,
		shouldRerunWithoutFeeDelay: false, // Default to false
	}
}
