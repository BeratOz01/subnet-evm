package blockstm

import (
	"fmt"
	"sort"
)

// Utilities for sorted slice operations

// insertToSortedList inserts a value into a sorted list
// if value already exist, returns the list unchanged
// if not, it inserts the value in the correct position
func insertToSortedList(list []int, value int) []int {
	// if list is empty or the last element is less than the value, append the value to the list
	if len(list) == 0 || list[len(list)-1] < value {
		return append(list, value)
	}

	// find the index of the value in the list
	idx := sort.SearchInts(list, value)
	if idx < len(list) && list[idx] == value {
		// already exist in the list
		return list
	}

	list = append(list, 0)
	copy(list[idx+1:], list[idx:])
	list[idx] = value

	return list
}

// hasNoGap checks if there is no gap in the list
func hasNoGap(list []int) bool {
	if len(list) == 0 {
		return true
	}

	return list[0]+len(list) == list[len(list)-1]+1
}

// removeFromList removes a value from a sorted list
func removeFromList(list []int, value int, expected bool) []int {
	idx := sort.SearchInts(list, value)
	if idx == len(list) || list[idx] != value {
		if expected {
			msg := fmt.Sprintf("should not happen - element expected in the list: %d", value)
			panic(msg)
		}

		// return the list
		return list
	}

	switch idx {
	case 0:
		return list[1:]
	case len(list) - 1:
		return list[:len(list)-1]
	default:
		return append(list[:idx], list[idx+1:]...)
	}
}
