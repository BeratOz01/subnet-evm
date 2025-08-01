package blockstm

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStatusBasics(t *testing.T) {
	t.Parallel()

	s := newTaskStatusManager(10)

	x := s.takeNextPending()
	require.Equal(t, 0, x)
	require.True(t, s.checkInProgress(x))

	x = s.takeNextPending()
	require.Equal(t, 1, x)
	require.True(t, s.checkInProgress(x))

	x = s.takeNextPending()
	require.Equal(t, 2, x)
	require.True(t, s.checkInProgress(x))

	s.markAsCompleted(0)
	require.False(t, s.checkInProgress(0))
	s.markAsCompleted(1)
	s.markAsCompleted(2)
	require.False(t, s.checkInProgress(1))
	require.False(t, s.checkInProgress(2))
	require.Equal(t, 2, s.lastCompletedTransaction())

	x = s.takeNextPending()
	require.Equal(t, 3, x)

	x = s.takeNextPending()
	require.Equal(t, 4, x)

	s.markAsCompleted(x)
	require.False(t, s.checkInProgress(4))
	require.Equal(t, 2, s.lastCompletedTransaction(), "zero should still be min complete")

	exp := []int{1, 2}
	require.Equal(t, exp, s.getRevalidationRange(1))
}

func TestMaxComplete(t *testing.T) {
	t.Parallel()

	s := newTaskStatusManager(10)

	for {
		tx := s.takeNextPending()

		if tx == -1 {
			break
		}

		if tx != 7 {
			s.markAsCompleted(tx)
		}
	}

	require.Equal(t, 6, s.lastCompletedTransaction())

	s2 := newTaskStatusManager(10)

	for {
		tx := s2.takeNextPending()

		if tx == -1 {
			break
		}
	}
	s2.markAsCompleted(2)
	s2.markAsCompleted(4)
	require.Equal(t, -1, s2.lastCompletedTransaction())

	s2.completed = insertToSortedList(s2.completed, 4)
	require.Equal(t, 2, s2.completedCount())
}
