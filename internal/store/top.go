package store

import (
	"container/heap"
	"sort"
)

// Top returns the first limit matching records in before order, keeping at most
// limit records in memory. Like Find, returned nested data is read-only.
// Predicates must not call collection methods while the DB read lock is held.
func (c *Coll[T]) Top(limit int, pred func(*T) bool, before func(*T, *T) bool) []T {
	if limit <= 0 {
		return nil
	}
	h := topHeap[T]{before: before}
	func() {
		c.db.mu.RLock()
		defer c.db.mu.RUnlock()
		for _, v := range c.m {
			if pred != nil && !pred(&v) {
				continue
			}
			if len(h.values) < limit {
				heap.Push(&h, v)
			} else if before(&v, &h.values[0]) {
				h.values[0] = v
				heap.Fix(&h, 0)
			}
		}
	}()
	sort.Slice(h.values, func(i, j int) bool { return before(&h.values[i], &h.values[j]) })
	return h.values
}

type topHeap[T any] struct {
	values []T
	before func(*T, *T) bool
}

func (h topHeap[T]) Len() int           { return len(h.values) }
func (h topHeap[T]) Less(i, j int) bool { return h.before(&h.values[j], &h.values[i]) }
func (h topHeap[T]) Swap(i, j int)      { h.values[i], h.values[j] = h.values[j], h.values[i] }
func (h *topHeap[T]) Push(v any)        { h.values = append(h.values, v.(T)) }
func (h *topHeap[T]) Pop() any {
	n := len(h.values)
	v := h.values[n-1]
	h.values = h.values[:n-1]
	return v
}
