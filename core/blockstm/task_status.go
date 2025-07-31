package blockstm

import "sort"

type taskStatusManager struct {
	pending    []int                // pending transactions indexes
	inProgress []int                // in progress transactions indexes
	completed  []int                // completed transactions indexes
	dependency map[int]map[int]bool // dependencies between transactions
	blocker    map[int]map[int]bool // blockers between transactions
}

// newTaskStatusManager initializes the task status manager
func newTaskStatusManager(numOfTasks int) (t taskStatusManager) {
	// initialize the pending tasks
	t.pending = make([]int, numOfTasks)
	t.dependency = make(map[int]map[int]bool, numOfTasks)
	t.blocker = make(map[int]map[int]bool, numOfTasks)

	for i := range numOfTasks {
		t.blocker[i] = make(map[int]bool)
		t.pending[i] = i
	}

	return t
}

// takeNextPending takes the next pending task and moves it to in progress
func (sm *taskStatusManager) takeNextPending() int {
	if len(sm.pending) == 0 {
		return -1
	}

	x := sm.pending[0]
	sm.pending = sm.pending[1:]
	sm.inProgress = insertToSortedList(sm.inProgress, x)

	return x
}

// pushToPending pushes a task to the pending list
func (sm *taskStatusManager) pushToPending(idx int) {
	sm.pending = insertToSortedList(sm.pending, idx)
}

func (sm *taskStatusManager) pushToPendingSet(set []int) {
	for _, idx := range set {
		if sm.checkComplete(idx) {
			sm.clearCompleted(idx)
		}

		sm.pushToPending(idx)
	}
}

// lastCompletedTransaction returns the last completed transaction
// which means is that returns the largest index i such that all transactions from 0 to i are completed
// without any gaps
func (sm *taskStatusManager) lastCompletedTransaction() int {
	// if there is no completed txs or the first completed tx is not 0
	if len(sm.completed) == 0 || sm.completed[0] != 0 {
		return -1
	}

	if sm.completed[len(sm.completed)-1] == len(sm.completed)-1 {
		return sm.completed[len(sm.completed)-1]
	}

	for i := len(sm.completed) - 2; i >= 0; i-- {
		if hasNoGap(sm.completed[:i+1]) {
			return sm.completed[i]
		}
	}

	return -1
}

// markAsCompleted removes a transaction from in progress and adds it to the completed list
func (sm *taskStatusManager) markAsCompleted(txIdx int) {
	sm.inProgress = removeFromList(sm.inProgress, txIdx, true)
	sm.completed = insertToSortedList(sm.completed, txIdx)
}

// getMinimumPending returns the minimum pending transaction index
// if there is no pending transaction, returns -1
func (sm *taskStatusManager) getMinimumPending() int {
	if len(sm.pending) == 0 {
		return -1
	}

	return sm.pending[0]
}

// completedCount returns the number of completed transactions
func (sm *taskStatusManager) completedCount() int {
	return len(sm.completed)
}

func (sm *taskStatusManager) addDependency(blocker int, dependent int) bool {
	// dependent must be greater than blocker
	if blocker < 0 || blocker >= dependent {
		return false
	}

	bblockers := sm.blocker[dependent]

	if sm.checkComplete(blocker) {
		// blocker has already completed
		// remove the dependency
		delete(bblockers, blocker)
		// if there are still blockers, return false
		return len(bblockers) > 0
	}

	if _, ok := sm.dependency[dependent]; !ok {
		sm.dependency[blocker] = make(map[int]bool)
	}

	sm.dependency[blocker][dependent] = true
	bblockers[blocker] = true

	return true
}

// checkComplete checks if a transaction is completed
// returns true if the transaction is in the completed list
func (sm *taskStatusManager) checkComplete(txIdx int) bool {
	idx := sort.SearchInts(sm.completed, txIdx)
	if idx < len(sm.completed) && sm.completed[idx] == txIdx {
		return true
	}

	return false
}

// checkInProgress checks if a transaction is in progress
// returns true if the transaction is in the in progress list
func (sm *taskStatusManager) checkInProgress(txIdx int) bool {
	idx := sort.SearchInts(sm.inProgress, txIdx)
	if idx < len(sm.inProgress) && sm.inProgress[idx] == txIdx {
		return true
	}
	return false
}

// checkPending checks if a transaction is pending
// returns true if the transaction is in the pending list
func (sm *taskStatusManager) checkPending(txIdx int) bool {
	idx := sort.SearchInts(sm.pending, txIdx)
	if idx < len(sm.pending) && sm.pending[idx] == txIdx {
		return true
	}
	return false
}

// getRevalidationRange: this range will be all tasks from tx (inclusive) that are not currently in progress up to the
//
//	'all complete' limit
func (sm *taskStatusManager) getRevalidationRange(txFrom int) (ret []int) {
	max := sm.lastCompletedTransaction()
	for x := txFrom; x <= max; x++ {
		if !sm.checkInProgress(x) {
			ret = append(ret, x)
		}
	}

	return
}

// clearPending removes a transaction from the pending list
func (sm *taskStatusManager) clearPending(txIdx int) {
	sm.pending = removeFromList(sm.pending, txIdx, false)
}

// clearCompleted removes a transaction from the completed list
func (sm *taskStatusManager) clearCompleted(txIdx int) {
	sm.completed = removeFromList(sm.completed, txIdx, false)
}

// clearInProgress removes a transaction from the in progress list
func (sm *taskStatusManager) clearInProgress(txIdx int) {
	sm.inProgress = removeFromList(sm.inProgress, txIdx, true)
}

// isBlocked checks if a transaction is blocked
func (sm *taskStatusManager) isBlocked(txIdx int) bool {
	return len(sm.blocker[txIdx]) > 0
}

// removeDependency removes a dependency from a transaction
// if the transaction has no more dependencies, it will be added to the pending list
func (sm *taskStatusManager) removeDependency(txIdx int) {
	if deps, ok := sm.dependency[txIdx]; ok && len(deps) > 0 {
		for k := range deps {
			delete(sm.blocker[k], txIdx)

			if len(sm.blocker[k]) == 0 {
				if !sm.checkComplete(k) && !sm.checkPending(k) && !sm.checkInProgress(k) {
					sm.pushToPending(k)
				}
			}
		}

		delete(sm.dependency, txIdx)
	}
}
