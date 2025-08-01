package blockstm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/log"
)

type ExecutionStat struct {
	TransactionIndex int // transaction index in the block
	Incarnation      int
	Start            uint64 // start time of the transaction
	End              uint64 // end time of the transaction
	Worker           int    // worker id
}

type ExecutionResult struct {
	err      error
	version  Version
	txIn     TransactionInput
	txOut    TransactionOutput
	txAllOut TransactionOutput
}

type ExecutionTask interface {
	Execute(mv *MVHashMap, incarnation int) error
	MVReadList() []ReadOperation
	MVWriteList() []WriteOperation
	MVFullWriteList() []WriteOperation
	Hash() common.Hash
	Sender() common.Address
	Settle()
	Dependencies() []int
}

type ExecutionVersionView struct {
	version Version
	task    ExecutionTask
	mv      *MVHashMap
	sender  common.Address
}

type ParallelExecutionResult struct {
	TxIO    *TransactionInputOutput
	Stats   *map[int]ExecutionStat
	Deps    *DAG
	AllDeps map[int]map[int]bool
}

func (ev *ExecutionVersionView) Execute() (res ExecutionResult) {
	res.version = ev.version
	if res.err = ev.task.Execute(ev.mv, ev.version.Incarnation); res.err != nil {
		return
	}

	res.txIn = ev.task.MVReadList()
	res.txOut = ev.task.MVWriteList()
	res.txAllOut = ev.task.MVFullWriteList()

	return
}

type ExecutionAbortError struct {
	Dependency  int
	OriginError error
}

func (e ExecutionAbortError) Error() string {
	if e.Dependency >= 0 {
		return fmt.Sprintf("Execution aborted due to dependency %d", e.Dependency)
	} else {
		return "Execution aborted"
	}
}

type ParallelExecutionFailedError struct {
	Msg string
}

func (e ParallelExecutionFailedError) Error() string {
	return e.Msg
}

type ParallelExecutor struct {
	// tasks to execute parallel
	tasks []ExecutionTask

	// stores the execution stats for the last incarnation of each task
	stats map[int]ExecutionStat

	statusMu sync.Mutex

	// number of workers that execute transactions speculatively
	numSpeculativeProcs int

	// channel for tasks that should be prioritized
	chTasks chan ExecutionVersionView

	// channel for speculative tasks
	chSpeculativeTasks chan struct{}

	// channel to signal that the result o a transaction could be written to storage
	specTasksQueue SafeQueue

	// a priority queue that stores speculative tasks
	chSettle chan int

	// channel to signal that a transaction has finished executing
	chResults chan struct{}

	// a priority queue that stores the transaction index of results, so we can validate the results in order
	resultsQueue SafeQueue

	// wait group to wait for all settling tasks to finish
	settleWg sync.WaitGroup

	// transaction index of the last settled transaction
	lastSettled int

	// For a task that runs only after all of its preceding tasks have finished and passed validation,
	// its result will be absolutely valid and therefore its validation could be skipped.
	// This map stores the boolean value indicating whether a task satisfy this condition (absolutely valid).
	skipCheck map[int]bool

	// Execution tasks stores the state of each execution task
	execTasks taskStatusManager

	// Validate tasks stores the state of each validation task
	validateTasks taskStatusManager

	// Stats for debugging purposes
	executionCount, successCount, abortCount, totalValidations, validationFailCount int

	diagExecSuccess, diagExecAbort []int

	// Multi-version hash map
	mvh *MVHashMap

	// Stores the inputs and outputs of the last incardanotion of all transactions
	lastTxIO *TransactionInputOutput

	// Tracks the incarnation number of each transaction
	txIncarnations []int

	// A map that stores the estimated dependency of a transaction if it is aborted without any known dependency
	estimateDeps map[int][]int

	// A map that records whether a transaction result has been speculatively validated
	preValidated map[int]bool

	// Time records when the parallel execution starts
	begin time.Time

	// Worker wait group
	workerWg sync.WaitGroup
}

// NewParallelExecutor creates a new parallel executor
func NewParallelExecutor(tasks []ExecutionTask, numProcs int) *ParallelExecutor {
	numOfTasks := len(tasks)

	resultQueue := NewSafePriorityQueue(numOfTasks)
	specTasksQueue := NewSafePriorityQueue(numOfTasks)

	pe := &ParallelExecutor{
		tasks:               tasks,
		numSpeculativeProcs: numProcs,
		stats:               make(map[int]ExecutionStat, numOfTasks),
		chTasks:             make(chan ExecutionVersionView, numOfTasks),
		chSpeculativeTasks:  make(chan struct{}, numOfTasks),
		chSettle:            make(chan int, numOfTasks),
		chResults:           make(chan struct{}, numOfTasks),
		specTasksQueue:      specTasksQueue,
		resultsQueue:        resultQueue,
		lastSettled:         -1,
		skipCheck:           make(map[int]bool),
		execTasks:           newTaskStatusManager(numOfTasks),
		validateTasks:       newTaskStatusManager(0),
		diagExecSuccess:     make([]int, numOfTasks),
		diagExecAbort:       make([]int, numOfTasks),
		mvh:                 NewMVHashMap(),
		lastTxIO:            NewTransactionInputOutput(numOfTasks),
		txIncarnations:      make([]int, numOfTasks),
		estimateDeps:        make(map[int][]int),
		preValidated:        make(map[int]bool),
		begin:               time.Now(),
	}

	return pe
}

func (pe *ParallelExecutor) Prepare() error {
	// tracks the most recent transaction index for each sender
	senderTransactions := make(map[common.Address]int)

	for idx, task := range pe.tasks {
		hadAnyDependencies := false

		// set false by default for all tasks
		pe.skipCheck[idx] = false
		pe.estimateDeps[idx] = make([]int, 0)

		// if task has any dependencies, it cannot be skipped
		if len(task.Dependencies()) > 0 {
			// for each dependency, add it to the dependency list
			for _, dep := range task.Dependencies() {
				hadAnyDependencies = true // mark that the task has dependencies
				pe.execTasks.addDependency(dep, idx)
			}

			// if there is at least one dependency, need to remove the task from the pending list
			// since tx is not executable until all dependencies are resolved
			if hadAnyDependencies {
				pe.execTasks.clearPending(idx)
			}

			continue
		}

		// if there is no dependencies
		// need to check that there is no other transaction with the same sender (because of nonce)
		if tx, ok := senderTransactions[task.Sender()]; ok {
			pe.execTasks.addDependency(tx, idx) // add dependency
			pe.execTasks.clearPending(idx)      // removes tx from pending list
		}

		// mark that the task has been added to the senderTransactions map
		senderTransactions[task.Sender()] = idx
	}

	// we are adding +1 for coordinator and numSpeculativeProcs for workers
	pe.workerWg.Add(pe.numSpeculativeProcs + 1)

	// launch workers that executes tasks in parallel
	for i := 0; i < pe.numSpeculativeProcs+1; i++ {
		go func(workerID int) {
			defer pe.workerWg.Done()

			// function that executes a task
			doWork := func(task ExecutionVersionView) {
				startTime := time.Since(pe.begin)

				// execute the task
				result := task.Execute()
				// if task is successfully executed, flush the results into mvh
				if result.err == nil {
					pe.mvh.FlushMVWriteSet(result.txAllOut)
				}

				// push to the result queue
				pe.resultsQueue.Push(result.version.TransactionIndex, result)
				pe.chResults <- struct{}{}

				endTime := time.Since(pe.begin)

				pe.statusMu.Lock()
				pe.stats[result.version.TransactionIndex] = ExecutionStat{
					TransactionIndex: result.version.TransactionIndex,
					Incarnation:      result.version.Incarnation,
					Start:            uint64(startTime),
					End:              uint64(endTime),
					Worker:           workerID,
				}
				pe.statusMu.Unlock()
			}

			// means that worker is a speculative worker
			if workerID < pe.numSpeculativeProcs {
				// execute speculative tasks in parallel
				for range pe.chSpeculativeTasks {
					doWork(pe.specTasksQueue.Pop().(ExecutionVersionView))
				}
			} else {
				// execute non-speculative tasks in sequential
				for task := range pe.chTasks {
					doWork(task)
				}
			}

		}(i)
	}

	pe.settleWg.Add(1)

	// wait for all tasks to settle
	go func() {
		for t := range pe.chSettle {
			pe.tasks[t].Settle()
		}

		pe.settleWg.Done()
	}()

	// take the next pending transaction
	nextPendingTx := pe.execTasks.takeNextPending()
	if nextPendingTx == -1 {
		return ParallelExecutionFailedError{"no executable transactions due to bad dependency"}
	}

	pe.executionCount++
	// starts the whole execution process by sending the first transaction to be executed
	pe.chTasks <- ExecutionVersionView{
		version: Version{nextPendingTx, 0},
		task:    pe.tasks[nextPendingTx],
		mv:      pe.mvh,
		sender:  pe.tasks[nextPendingTx].Sender(),
	}

	return nil
}

func (pe *ParallelExecutor) Step(res *ExecutionResult) (result ParallelExecutionResult, err error) {
	txIndex := res.version.TransactionIndex

	if abortError, ok := res.err.(ExecutionAbortError); ok && abortError.OriginError != nil && pe.skipCheck[txIndex] {
		// If the transaction failed when we know it should not fail, this means the transaction itself is
		// bad (e.g. wrong nonce), and we should exit the execution immediately
		err = fmt.Errorf("could not apply tx %d [%v]: %w", txIndex, pe.tasks[txIndex].Hash(), abortError.OriginError)
		pe.Close(true)
		return
	}

	// if there is a execution error
	if executionError, ok := res.err.(ExecutionAbortError); ok {
		hasNewDependency := false

		// if there is a real dependency that we know about
		if executionError.Dependency >= 0 {
			// remove the estimate dependencies that are greater than the new dependency
			l := len(pe.estimateDeps[txIndex])
			for l > 0 && pe.estimateDeps[txIndex][l-1] > executionError.Dependency {
				// remove the dependency list
				pe.execTasks.removeDependency(pe.estimateDeps[txIndex][l-1])
				// update the estimated dependency list
				pe.estimateDeps[txIndex] = pe.estimateDeps[txIndex][:l-1]
				l--
			}

			// add the new dependency to the dependency list
			hasNewDependency = pe.execTasks.addDependency(executionError.Dependency, txIndex)
		} else {
			estimate := 0

			// if there is an estimated dependency, use the last one
			if len(pe.estimateDeps[txIndex]) > 0 {
				estimate = pe.estimateDeps[txIndex][len(pe.estimateDeps[txIndex])-1]
			}

			// add this estimated dependency
			hasNewDependency = pe.execTasks.addDependency(estimate, txIndex)
			newEstimate := estimate + (estimate+txIndex)/2
			if newEstimate >= txIndex {
				newEstimate = txIndex - 1
			}

			// append new estimated dependency
			pe.estimateDeps[txIndex] = append(pe.estimateDeps[txIndex], newEstimate)
		}

		pe.execTasks.clearInProgress(txIndex)

		// if there is no new dependency, mark as pending (ready to execute)
		if !hasNewDependency {
			pe.execTasks.pushToPending(txIndex)
		}

		pe.txIncarnations[txIndex]++
		pe.diagExecAbort[txIndex]++
		pe.abortCount++
	} else {
		// if successful execution, record the read sets of tx
		pe.lastTxIO.recordRead(txIndex, res.txIn)

		// if incarnation is 0, means that this is the first successful execution of the transaction
		if res.version.Incarnation == 0 {
			pe.lastTxIO.recordWrite(txIndex, res.txOut)
			pe.lastTxIO.recordAllWrite(txIndex, res.txAllOut)
		} else {
			// means that we tried at least once to execute the tx
			// if there are new writes we need to revalidate the tx
			if res.txAllOut.hasNewWrite(pe.lastTxIO.AllWriteSet(txIndex)) {
				// create a validation tasks for all transaction > tx
				pe.validateTasks.pushToPendingSet(pe.execTasks.getRevalidationRange(txIndex + 1))
			}
			// find previous writes of the tx, that no longer written by this incarnation
			// delete it from the mvh
			previousWrites := pe.lastTxIO.AllWriteSet(txIndex)

			compareMap := make(map[STMKey]bool)
			for _, w := range res.txAllOut {
				compareMap[w.Path] = true
			}

			for _, w := range previousWrites {
				if _, ok := compareMap[w.Path]; !ok {
					pe.mvh.Delete(w.Path, txIndex)
				}
			}

			// record the new writes with the successful execution incarnation
			pe.lastTxIO.recordWrite(txIndex, res.txOut)
			pe.lastTxIO.recordAllWrite(txIndex, res.txAllOut)
		}

		// after successful execution, we need to validate the tx
		pe.validateTasks.pushToPending(txIndex)
		pe.execTasks.markAsCompleted(txIndex)

		// record the success count
		pe.diagExecSuccess[txIndex]++
		pe.successCount++

		// remove the dependency from the dependency list (unblocks other txs that depend on this tx)
		pe.execTasks.removeDependency(txIndex)
	}

	// do validations
	maxCompleted := pe.execTasks.lastCompletedTransaction()
	toValidate := make([]int, 0, 2)

	// only validate txs that are pending validation
	for pe.validateTasks.getMinimumPending() <= maxCompleted && pe.validateTasks.getMinimumPending() >= 0 {
		toValidate = append(toValidate, pe.validateTasks.takeNextPending())
	}

	for i := 0; i < len(toValidate); i++ {
		pe.totalValidations++

		tx := toValidate[i]

		// if the transaction is skipped or the version is valid, mark as completed
		if pe.skipCheck[tx] || ValidateVersion(tx, pe.lastTxIO, pe.mvh) {
			pe.validateTasks.markAsCompleted(tx)
			continue
		}

		// if not validation is invalid
		pe.validationFailCount++
		pe.diagExecAbort[tx]++

		for _, v := range pe.lastTxIO.AllWriteSet(tx) {
			// since tx is not validated yet, we need to mark it as estimate for all writes
			pe.mvh.MarkEstimate(v.Path, tx)
		}

		// create a validation tasks for all transaction > tx
		pe.validateTasks.pushToPendingSet(pe.execTasks.getRevalidationRange(tx + 1))
		pe.validateTasks.clearInProgress(tx)

		pe.execTasks.clearCompleted(tx)
		pe.execTasks.pushToPending(tx)

		pe.preValidated[tx] = false
		pe.txIncarnations[tx]++
	}

	maxValidated := pe.validateTasks.lastCompletedTransaction()

	// for settling the transactions
	// we need to check that the transaction is not in progress, pending or blocked
	for pe.lastSettled < maxValidated {
		pe.lastSettled++
		if pe.execTasks.checkInProgress(pe.lastSettled) ||
			pe.execTasks.checkPending(pe.lastSettled) ||
			pe.execTasks.isBlocked(pe.lastSettled) {
			pe.lastSettled--
			break
		}

		// send the transaction to be settled
		pe.chSettle <- pe.lastSettled
	}

	// if all txs validated and executed successfully, means that the execution is complete
	// we can close the parallel executor and return the result
	if pe.validateTasks.completedCount() == len(pe.tasks) && pe.execTasks.completedCount() == len(pe.tasks) {
		log.Debug("blockstm exec summary", "execs", pe.executionCount, "success", pe.successCount, "aborts", pe.abortCount, "validations", pe.totalValidations, "failures", pe.validationFailCount, "#tasks/#execs", fmt.Sprintf("%.2f%%", float64(len(pe.tasks))/float64(pe.executionCount)*100))

		pe.Close(true)

		var allDeps map[int]map[int]bool
		var deps DAG

		allDeps = GetBlockDependencies(*pe.lastTxIO)
		deps = BuildDAG(*pe.lastTxIO)

		return ParallelExecutionResult{pe.lastTxIO, &pe.stats, &deps, allDeps}, nil
	}

	// if the next executable transaction is the next to be validated
	// we need to send it to be executed
	if pe.execTasks.getMinimumPending() != -1 && pe.execTasks.getMinimumPending() == maxValidated+1 {
		nextTx := pe.execTasks.takeNextPending()
		if nextTx != -1 {
			pe.executionCount++
			pe.skipCheck[nextTx] = true
			pe.chTasks <- ExecutionVersionView{version: Version{nextTx, pe.txIncarnations[nextTx]}, task: pe.tasks[nextTx], mv: pe.mvh, sender: pe.tasks[nextTx].Sender()}
		}
	}

	// find all the speculative tasks that are pending
	for pe.execTasks.getMinimumPending() != -1 {
		// if there is such a pending transaction, send it to be executed in parallel
		nextTx := pe.execTasks.takeNextPending()

		if nextTx != -1 {
			pe.executionCount++

			task := ExecutionVersionView{version: Version{nextTx, pe.txIncarnations[nextTx]}, task: pe.tasks[nextTx], mv: pe.mvh, sender: pe.tasks[nextTx].Sender()}
			pe.specTasksQueue.Push(nextTx, task)
			pe.chSpeculativeTasks <- struct{}{}
		}
	}

	return
}

// Close closes channels and waits for all workers to finish if wait is true
func (pe *ParallelExecutor) Close(wait bool) {
	close(pe.chTasks)
	close(pe.chSpeculativeTasks)
	close(pe.chSettle)

	if wait {
		pe.settleWg.Wait()
	}

	if wait {
		pe.workerWg.Wait()
	}
}

type PropertyCheck func(*ParallelExecutor) error

func executeParallelWithCheck(tasks []ExecutionTask, check PropertyCheck, numProcs int, interruptCtx context.Context) (result ParallelExecutionResult, err error) {
	// if there is no tasks
	if len(tasks) == 0 {
		return ParallelExecutionResult{NewTransactionInputOutput(len(tasks)), nil, nil, nil}, nil
	}

	// create a new parallel executor
	pe := NewParallelExecutor(tasks, numProcs)
	// prepare the parallel executor
	err = pe.Prepare()
	if err != nil {
		pe.Close(true)
		return
	}

	for range pe.chResults {
		if interruptCtx != nil && interruptCtx.Err() != nil {
			pe.Close(true)
			return result, interruptCtx.Err()
		}

		// pop the result from the result queue
		res := pe.resultsQueue.Pop().(ExecutionResult)

		// step the parallel executor
		result, err = pe.Step(&res)
		if err != nil {
			return result, err
		}

		if check != nil {
			err = check(pe)
		}

		if result.TxIO != nil || err != nil {
			return result, err
		}
	}

	return
}

func ExecuteParallel(tasks []ExecutionTask, numProcs int, interruptCtx context.Context) (result ParallelExecutionResult, err error) {
	return executeParallelWithCheck(tasks, nil, numProcs, interruptCtx)
}
