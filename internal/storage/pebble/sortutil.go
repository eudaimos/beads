package pebble

import "sort"

// stableSortBy sorts items in place with less, keeping equal items in order.
func stableSortBy[T any](items []T, less func(a, b T) bool) {
	sort.SliceStable(items, func(i, j int) bool { return less(items[i], items[j]) })
}
