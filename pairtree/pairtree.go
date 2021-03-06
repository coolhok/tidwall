// Copyright 2014 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package btree implements in-memory B-Trees of arbitrary degree.
//
// btree implements an in-memory B-Tree for use as an ordered data structure.
// It is not meant for persistent storage solutions.
//
// It has a flatter structure than an equivalent red-black or other binary tree,
// which in some cases yields better memory usage and/or performance.
// See some discussion on the matter here:
//   http://google-opensource.blogspot.com/2013/01/c-containers-that-save-memory-and-time.html
// Note, though, that this project is in no way related to the C++ B-Tree
// implementation written about there.
//
// Within this tree, each node contains a slice of items and a (possibly nil)
// slice of children.  For basic numeric values or raw structs, this can cause
// efficiency differences when compared to equivalent C++ template code that
// stores values in arrays within the node:
//   * Due to the overhead of storing values as interfaces (each
//     value needs to be stored as the value itself, then 2 words for the
//     interface pointing to that value and its type), resulting in higher
//     memory use.
//   * Since interfaces can point to values anywhere in memory, values are
//     most likely not stored in contiguous blocks, resulting in a higher
//     number of cache misses.
// These issues don't tend to matter, though, when working with strings or other
// heap-allocated structures, since C++-equivalent structures also must store
// pointers and also distribute their values across the heap.
//
// This implementation is designed to be a drop-in replacement to gollrb.LLRB
// trees, (http://github.com/petar/gollrb), an excellent and probably the most
// widely used ordered tree implementation in the Go ecosystem currently.
// Its functions, therefore, exactly mirror those of
// llrb.LLRB where possible.  Unlike gollrb, though, we currently don't
// support storing multiple equivalent values.
package pairtree

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/tidwall/pair"
)

const (
	defaultFreeListSize = 32
	defaultDegrees      = 9
)

var (
	nilPairs    = make(items, 16)
	nilChildren = make(children, 16)
)

// freeList represents a free list of btree nodes. By default each
// BTree has its own freeList, but multiple BTrees can share the same
// freeList.
// Two Btrees using the same freelist are safe for concurrent write access.
type freeList struct {
	mu       sync.Mutex
	freelist []*node
}

// newFreeList creates a new free list.
// size is the maximum size of the returned free list.
func newFreeList(size int) *freeList {
	return &freeList{freelist: make([]*node, 0, size)}
}

func (f *freeList) newNode() (n *node) {
	f.mu.Lock()
	index := len(f.freelist) - 1
	if index < 0 {
		f.mu.Unlock()
		return new(node)
	}
	n = f.freelist[index]
	f.freelist[index] = nil
	f.freelist = f.freelist[:index]
	f.mu.Unlock()
	return
}

func (f *freeList) freeNode(n *node) {
	f.mu.Lock()
	if len(f.freelist) < cap(f.freelist) {
		f.freelist = append(f.freelist, n)
	}
	f.mu.Unlock()
}

// New creates a new B-Tree with the given degree.
//
// New(2), for example, will create a 2-3-4 tree (each node contains 1-3 items
// and 2-4 children).
func New(less func(a, b pair.Pair) bool) *PairTree {
	return newWithFreeList(newFreeList(defaultFreeListSize), less)
}

// newWithFreeList creates a new B-Tree that uses the given node free list.
func newWithFreeList(f *freeList, less func(a, b pair.Pair) bool) *PairTree {
	if less == nil {
		less = func(a, b pair.Pair) bool {
			return bytes.Compare(a.Key(), b.Key()) == -1
		}
	}
	return &PairTree{
		degree: defaultDegrees,
		cow:    &copyOnWriteContext{freelist: f},
		less:   less,
	}
}

var nilPair = pair.Pair{}

// items stores items in a node.
type items []pair.Pair

// insertAt inserts a value into the given index, pushing all subsequent values
// forward.
func (s *items) insertAt(index int, item pair.Pair) {
	*s = append(*s, nilPair)
	if index < len(*s) {
		copy((*s)[index+1:], (*s)[index:])
	}
	(*s)[index] = item
}

// removeAt removes a value at a given index, pulling all subsequent values
// back.
func (s *items) removeAt(index int) pair.Pair {
	item := (*s)[index]
	copy((*s)[index:], (*s)[index+1:])
	(*s)[len(*s)-1] = nilPair
	*s = (*s)[:len(*s)-1]
	return item
}

// pop removes and returns the last element in the list.
func (s *items) pop() (out pair.Pair) {
	index := len(*s) - 1
	out = (*s)[index]
	(*s)[index] = nilPair
	*s = (*s)[:index]
	return
}

// truncate truncates this instance at index so that it contains only the
// first index items. index must be less than or equal to length.
func (s *items) truncate(index int) {
	var toClear items
	*s, toClear = (*s)[:index], (*s)[index:]
	for len(toClear) > 0 {
		toClear = toClear[copy(toClear, nilPairs):]
	}
}

// find returns the index where the given item should be inserted into this
// list.  'found' is true if the item already exists in the list at the given
// index.
func (s items) find(item pair.Pair, less func(a, b pair.Pair) bool) (index int, found bool) {
	i, j := 0, len(s)
	for i < j {
		h := i + (j-i)/2
		if !less(item, s[h]) {
			i = h + 1
		} else {
			j = h
		}
	}
	if i > 0 && !less(s[i-1], item) {
		return i - 1, true
	}
	return i, false
}

// children stores child nodes in a node.
type children []*node

// insertAt inserts a value into the given index, pushing all subsequent values
// forward.
func (s *children) insertAt(index int, n *node) {
	*s = append(*s, nil)
	if index < len(*s) {
		copy((*s)[index+1:], (*s)[index:])
	}
	(*s)[index] = n
}

// removeAt removes a value at a given index, pulling all subsequent values
// back.
func (s *children) removeAt(index int) *node {
	n := (*s)[index]
	copy((*s)[index:], (*s)[index+1:])
	(*s)[len(*s)-1] = nil
	*s = (*s)[:len(*s)-1]
	return n
}

// pop removes and returns the last element in the list.
func (s *children) pop() (out *node) {
	index := len(*s) - 1
	out = (*s)[index]
	(*s)[index] = nil
	*s = (*s)[:index]
	return
}

// truncate truncates this instance at index so that it contains only the
// first index children. index must be less than or equal to length.
func (s *children) truncate(index int) {
	var toClear children
	*s, toClear = (*s)[:index], (*s)[index:]
	for len(toClear) > 0 {
		toClear = toClear[copy(toClear, nilChildren):]
	}
}

// node is an internal node in a tree.
//
// It must at all times maintain the invariant that either
//   * len(children) == 0, len(items) unconstrained
//   * len(children) == len(items) + 1
type node struct {
	items    items
	children children
	cow      *copyOnWriteContext
}

func (n *node) mutableFor(cow *copyOnWriteContext) *node {
	if n.cow == cow {
		return n
	}
	out := cow.newNode()
	if cap(out.items) >= len(n.items) {
		out.items = out.items[:len(n.items)]
	} else {
		out.items = make(items, len(n.items), cap(n.items))
	}
	copy(out.items, n.items)
	// Copy children
	if cap(out.children) >= len(n.children) {
		out.children = out.children[:len(n.children)]
	} else {
		out.children = make(children, len(n.children), cap(n.children))
	}
	copy(out.children, n.children)
	return out
}

func (n *node) mutableChild(i int) *node {
	c := n.children[i].mutableFor(n.cow)
	n.children[i] = c
	return c
}

// split splits the given node at the given index.  The current node shrinks,
// and this function returns the item that existed at that index and a new node
// containing all items/children after it.
func (n *node) split(i int) (pair.Pair, *node) {
	item := n.items[i]
	next := n.cow.newNode()
	next.items = append(next.items, n.items[i+1:]...)
	n.items.truncate(i)
	if len(n.children) > 0 {
		next.children = append(next.children, n.children[i+1:]...)
		n.children.truncate(i + 1)
	}
	return item, next
}

// maybeSplitChild checks if a child should be split, and if so splits it.
// Returns whether or not a split occurred.
func (n *node) maybeSplitChild(i, maxPairs int) bool {
	if len(n.children[i].items) < maxPairs {
		return false
	}
	first := n.mutableChild(i)
	item, second := first.split(maxPairs / 2)
	n.items.insertAt(i, item)
	n.children.insertAt(i+1, second)
	return true
}

// insert inserts an item into the subtree rooted at this node, making sure
// no nodes in the subtree exceed maxPairs items.  Should an equivalent item be
// be found/replaced by insert, it will be returned.
func (n *node) insert(item pair.Pair, maxPairs int, less func(a, b pair.Pair) bool) pair.Pair {
	i, found := n.items.find(item, less)
	if found {
		out := n.items[i]
		n.items[i] = item
		return out
	}
	if len(n.children) == 0 {
		n.items.insertAt(i, item)
		return nilPair
	}
	if n.maybeSplitChild(i, maxPairs) {
		inTree := n.items[i]
		switch {
		case less(item, inTree):
			// no change, we want first split node
		case less(inTree, item):
			i++ // we want second split node
		default:
			out := n.items[i]
			n.items[i] = item
			return out
		}
	}
	return n.mutableChild(i).insert(item, maxPairs, less)
}

// get finds the given key in the subtree and returns it.
func (n *node) get(key pair.Pair, less func(a, b pair.Pair) bool) pair.Pair {
	i, found := n.items.find(key, less)
	if found {
		return n.items[i]
	} else if len(n.children) > 0 {
		return n.children[i].get(key, less)
	}
	return nilPair
}

// min returns the first item in the subtree.
func min(n *node) pair.Pair {
	if n == nil {
		return nilPair
	}
	for len(n.children) > 0 {
		n = n.children[0]
	}
	if len(n.items) == 0 {
		return nilPair
	}
	return n.items[0]
}

// max returns the last item in the subtree.
func max(n *node) pair.Pair {
	if n == nil {
		return nilPair
	}
	for len(n.children) > 0 {
		n = n.children[len(n.children)-1]
	}
	if len(n.items) == 0 {
		return nilPair
	}
	return n.items[len(n.items)-1]
}

// toRemove details what item to remove in a node.remove call.
type toRemove int

const (
	removePair toRemove = iota // removes the given item
	removeMin                  // removes smallest item in the subtree
	removeMax                  // removes largest item in the subtree
)

// remove removes an item from the subtree rooted at this node.
func (n *node) remove(item pair.Pair, minPairs int, typ toRemove, less func(a, b pair.Pair) bool) pair.Pair {
	var i int
	var found bool
	switch typ {
	case removeMax:
		if len(n.children) == 0 {
			return n.items.pop()
		}
		i = len(n.items)
	case removeMin:
		if len(n.children) == 0 {
			return n.items.removeAt(0)
		}
		i = 0
	case removePair:
		i, found = n.items.find(item, less)
		if len(n.children) == 0 {
			if found {
				return n.items.removeAt(i)
			}
			return nilPair
		}
	default:
		panic("invalid type")
	}
	// If we get to here, we have children.
	if len(n.children[i].items) <= minPairs {
		return n.growChildAndRemove(i, item, minPairs, typ, less)
	}
	child := n.mutableChild(i)
	// Either we had enough items to begin with, or we've done some
	// merging/stealing, because we've got enough now and we're ready to return
	// stuff.
	if found {
		// The item exists at index 'i', and the child we've selected can give us a
		// predecessor, since if we've gotten here it's got > minPairs items in it.
		out := n.items[i]
		// We use our special-case 'remove' call with typ=maxPair to pull the
		// predecessor of item i (the rightmost leaf of our immediate left child)
		// and set it into where we pulled the item from.
		n.items[i] = child.remove(nilPair, minPairs, removeMax, less)
		return out
	}
	// Final recursive call.  Once we're here, we know that the item isn't in this
	// node and that the child is big enough to remove from.
	return child.remove(item, minPairs, typ, less)
}

// growChildAndRemove grows child 'i' to make sure it's possible to remove an
// item from it while keeping it at minPairs, then calls remove to actually
// remove it.
//
// Most documentation says we have to do two sets of special casing:
//   1) item is in this node
//   2) item is in child
// In both cases, we need to handle the two subcases:
//   A) node has enough values that it can spare one
//   B) node doesn't have enough values
// For the latter, we have to check:
//   a) left sibling has node to spare
//   b) right sibling has node to spare
//   c) we must merge
// To simplify our code here, we handle cases #1 and #2 the same:
// If a node doesn't have enough items, we make sure it does (using a,b,c).
// We then simply redo our remove call, and the second time (regardless of
// whether we're in case 1 or 2), we'll have enough items and can guarantee
// that we hit case A.
func (n *node) growChildAndRemove(i int, item pair.Pair, minPairs int, typ toRemove, less func(a, b pair.Pair) bool) pair.Pair {
	if i > 0 && len(n.children[i-1].items) > minPairs {
		// Steal from left child
		child := n.mutableChild(i)
		stealFrom := n.mutableChild(i - 1)
		stolenPair := stealFrom.items.pop()
		child.items.insertAt(0, n.items[i-1])
		n.items[i-1] = stolenPair
		if len(stealFrom.children) > 0 {
			child.children.insertAt(0, stealFrom.children.pop())
		}
	} else if i < len(n.items) && len(n.children[i+1].items) > minPairs {
		// steal from right child
		child := n.mutableChild(i)
		stealFrom := n.mutableChild(i + 1)
		stolenPair := stealFrom.items.removeAt(0)
		child.items = append(child.items, n.items[i])
		n.items[i] = stolenPair
		if len(stealFrom.children) > 0 {
			child.children = append(child.children, stealFrom.children.removeAt(0))
		}
	} else {
		if i >= len(n.items) {
			i--
		}
		child := n.mutableChild(i)
		// merge with right child
		mergePair := n.items.removeAt(i)
		mergeChild := n.children.removeAt(i + 1)
		child.items = append(child.items, mergePair)
		child.items = append(child.items, mergeChild.items...)
		child.children = append(child.children, mergeChild.children...)
		n.cow.freeNode(mergeChild)
	}
	return n.remove(item, minPairs, typ, less)
}

type direction int

const (
	descend = direction(-1)
	ascend  = direction(+1)
)

// iterate provides a simple method for iterating over elements in the tree.
//
// When ascending, the 'start' should be less than 'stop' and when descending,
// the 'start' should be greater than 'stop'. Setting 'includeStart' to true
// will force the iterator to include the first item when it equals 'start',
// thus creating a "greaterOrEqual" or "lessThanEqual" rather than just a
// "greaterThan" or "lessThan" queries.
func (n *node) iterate(dir direction, start, stop pair.Pair, includeStart bool, hit bool, iter func(item pair.Pair) bool, less func(a, b pair.Pair) bool) (bool, bool) {
	var ok bool
	switch dir {
	case ascend:
		for i := 0; i < len(n.items); i++ {
			if start != nilPair && less(n.items[i], start) {
				continue
			}
			if len(n.children) > 0 {
				if hit, ok = n.children[i].iterate(dir, start, stop, includeStart, hit, iter, less); !ok {
					return hit, false
				}
			}
			if !includeStart && !hit && start != nilPair && !less(start, n.items[i]) {
				hit = true
				continue
			}
			hit = true
			if stop != nilPair && !less(n.items[i], stop) {
				return hit, false
			}
			if !iter(n.items[i]) {
				return hit, false
			}
		}
		if len(n.children) > 0 {
			if hit, ok = n.children[len(n.children)-1].iterate(dir, start, stop, includeStart, hit, iter, less); !ok {
				return hit, false
			}
		}
	case descend:
		for i := len(n.items) - 1; i >= 0; i-- {
			if start != nilPair && !less(n.items[i], start) {
				if !includeStart || hit || less(start, n.items[i]) {
					continue
				}
			}
			if len(n.children) > 0 {
				if hit, ok = n.children[i+1].iterate(dir, start, stop, includeStart, hit, iter, less); !ok {
					return hit, false
				}
			}
			if stop != nilPair && !less(stop, n.items[i]) {
				return hit, false //	continue
			}
			hit = true
			if !iter(n.items[i]) {
				return hit, false
			}
		}
		if len(n.children) > 0 {
			if hit, ok = n.children[0].iterate(dir, start, stop, includeStart, hit, iter, less); !ok {
				return hit, false
			}
		}
	}
	return hit, true
}

// Used for testing/debugging purposes.
func (n *node) print(w io.Writer, level int) {
	fmt.Fprintf(w, "%sNODE:%v\n", strings.Repeat("  ", level), n.items)
	for _, c := range n.children {
		c.print(w, level+1)
	}
}

// PairTree is an implementation of a B-Tree.
//
// PairTree stores Pair instances in an ordered structure, allowing easy insertion,
// removal, and iteration.
//
// Write operations are not safe for concurrent mutation by multiple
// goroutines, but Read operations are.
type PairTree struct {
	degree int
	length int
	root   *node
	less   func(a, b pair.Pair) bool
	cow    *copyOnWriteContext
}

// copyOnWriteContext pointers determine node ownership... a tree with a write
// context equivalent to a node's write context is allowed to modify that node.
// A tree whose write context does not match a node's is not allowed to modify
// it, and must create a new, writable copy (IE: it's a Clone).
//
// When doing any write operation, we maintain the invariant that the current
// node's context is equal to the context of the tree that requested the write.
// We do this by, before we descend into any node, creating a copy with the
// correct context if the contexts don't match.
//
// Since the node we're currently visiting on any write has the requesting
// tree's context, that node is modifiable in place.  Children of that node may
// not share context, but before we descend into them, we'll make a mutable
// copy.
type copyOnWriteContext struct {
	freelist *freeList
}

// Clone clones the btree, lazily.  Clone should not be called concurrently,
// but the original tree (t) and the new tree (t2) can be used concurrently
// once the Clone call completes.
//
// The internal tree structure of b is marked read-only and shared between t and
// t2.  Writes to both t and t2 use copy-on-write logic, creating new nodes
// whenever one of b's original nodes would have been modified.  Read operations
// should have no performance degredation.  Write operations for both t and t2
// will initially experience minor slow-downs caused by additional allocs and
// copies due to the aforementioned copy-on-write logic, but should converge to
// the original performance characteristics of the original tree.
func (t *PairTree) Clone() (t2 *PairTree) {
	// Create two entirely new copy-on-write contexts.
	// This operation effectively creates three trees:
	//   the original, shared nodes (old b.cow)
	//   the new b.cow nodes
	//   the new out.cow nodes
	cow1, cow2 := *t.cow, *t.cow
	out := *t
	t.cow = &cow1
	out.cow = &cow2
	return &out
}

// maxPairs returns the max number of items to allow per node.
func (t *PairTree) maxPairs() int {
	return t.degree*2 - 1
}

// minPairs returns the min number of items to allow per node (ignored for the
// root node).
func (t *PairTree) minPairs() int {
	return t.degree - 1
}

func (c *copyOnWriteContext) newNode() (n *node) {
	n = c.freelist.newNode()
	n.cow = c
	return
}

func (c *copyOnWriteContext) freeNode(n *node) {
	if n.cow == c {
		// clear to allow GC
		n.items.truncate(0)
		n.children.truncate(0)
		n.cow = nil
		c.freelist.freeNode(n)
	}
}

// ReplaceOrInsert adds the given item to the tree.  If an item in the tree
// already equals the given one, it is removed from the tree and returned.
// Otherwise, nil is returned.
//
// nil cannot be added to the tree (will panic).
func (t *PairTree) ReplaceOrInsert(item pair.Pair) pair.Pair {
	if item == nilPair {
		panic("nil item being added to BTree")
	}
	if t.root == nil {
		t.root = t.cow.newNode()
		t.root.items = append(t.root.items, item)
		t.length++
		return nilPair
	} else {
		t.root = t.root.mutableFor(t.cow)
		if len(t.root.items) >= t.maxPairs() {
			item2, second := t.root.split(t.maxPairs() / 2)
			oldroot := t.root
			t.root = t.cow.newNode()
			t.root.items = append(t.root.items, item2)
			t.root.children = append(t.root.children, oldroot, second)
		}
	}
	out := t.root.insert(item, t.maxPairs(), t.less)
	if out == nilPair {
		t.length++
	}
	return out
}

// Delete removes an item equal to the passed in item from the tree, returning
// it.  If no such item exists, returns nil.
func (t *PairTree) Delete(item pair.Pair) pair.Pair {
	return t.deletePair(item, removePair, t.less)
}

// DeleteMin removes the smallest item in the tree and returns it.
// If no such item exists, returns nil.
func (t *PairTree) DeleteMin() pair.Pair {
	return t.deletePair(nilPair, removeMin, t.less)
}

// DeleteMax removes the largest item in the tree and returns it.
// If no such item exists, returns nil.
func (t *PairTree) DeleteMax() pair.Pair {
	return t.deletePair(nilPair, removeMax, t.less)
}

func (t *PairTree) deletePair(item pair.Pair, typ toRemove, less func(a, b pair.Pair) bool) pair.Pair {
	if t.root == nil || len(t.root.items) == 0 {
		return nilPair
	}
	t.root = t.root.mutableFor(t.cow)
	out := t.root.remove(item, t.minPairs(), typ, less)
	if len(t.root.items) == 0 && len(t.root.children) > 0 {
		oldroot := t.root
		t.root = t.root.children[0]
		t.cow.freeNode(oldroot)
	}
	if out != nilPair {
		t.length--
	}
	return out
}

// AscendRange calls the iterator for every value in the tree within the range
// [greaterOrEqual, lessThan), until iterator returns false.
func (t *PairTree) AscendRange(greaterOrEqual, lessThan pair.Pair, iterator func(item pair.Pair) bool) {
	if t.root == nil {
		return
	}
	t.root.iterate(ascend, greaterOrEqual, lessThan, true, false, iterator, t.less)
}

// AscendLessThan calls the iterator for every value in the tree within the range
// [first, pivot), until iterator returns false.
func (t *PairTree) AscendLessThan(pivot pair.Pair, iterator func(item pair.Pair) bool) {
	if t.root == nil {
		return
	}
	t.root.iterate(ascend, nilPair, pivot, false, false, iterator, t.less)
}

// AscendGreaterOrEqual calls the iterator for every value in the tree within
// the range [pivot, last], until iterator returns false.
func (t *PairTree) AscendGreaterOrEqual(pivot pair.Pair, iterator func(item pair.Pair) bool) {
	if t.root == nil {
		return
	}
	t.root.iterate(ascend, pivot, nilPair, true, false, iterator, t.less)
}

// Ascend calls the iterator for every value in the tree within the range
// [first, last], until iterator returns false.
func (t *PairTree) Ascend(iterator func(item pair.Pair) bool) {
	if t.root == nil {
		return
	}
	t.root.iterate(ascend, nilPair, nilPair, false, false, iterator, t.less)
}

// DescendRange calls the iterator for every value in the tree within the range
// [lessOrEqual, greaterThan), until iterator returns false.
func (t *PairTree) DescendRange(lessOrEqual, greaterThan pair.Pair, iterator func(item pair.Pair) bool) {
	if t.root == nil {
		return
	}
	t.root.iterate(descend, lessOrEqual, greaterThan, true, false, iterator, t.less)
}

// DescendLessOrEqual calls the iterator for every value in the tree within the range
// [pivot, first], until iterator returns false.
func (t *PairTree) DescendLessOrEqual(pivot pair.Pair, iterator func(item pair.Pair) bool) {
	if t.root == nil {
		return
	}
	t.root.iterate(descend, pivot, nilPair, true, false, iterator, t.less)
}

// DescendGreaterThan calls the iterator for every value in the tree within
// the range (pivot, last], until iterator returns false.
func (t *PairTree) DescendGreaterThan(pivot pair.Pair, iterator func(item pair.Pair) bool) {
	if t.root == nil {
		return
	}
	t.root.iterate(descend, nilPair, pivot, false, false, iterator, t.less)
}

// Descend calls the iterator for every value in the tree within the range
// [last, first], until iterator returns false.
func (t *PairTree) Descend(iterator func(item pair.Pair) bool) {
	if t.root == nil {
		return
	}
	t.root.iterate(descend, nilPair, nilPair, false, false, iterator, t.less)
}

// Get looks for the key item in the tree, returning it.  It returns nil if
// unable to find that item.
func (t *PairTree) Get(key pair.Pair) pair.Pair {
	if t.root == nil {
		return nilPair
	}
	return t.root.get(key, t.less)
}

// Min returns the smallest item in the tree, or nil if the tree is empty.
func (t *PairTree) Min() pair.Pair {
	return min(t.root)
}

// Max returns the largest item in the tree, or nil if the tree is empty.
func (t *PairTree) Max() pair.Pair {
	return max(t.root)
}

// Has returns true if the given key is in the tree.
func (t *PairTree) Has(key pair.Pair) bool {
	return t.Get(key) != nilPair
}

// Len returns the number of items currently in the tree.
func (t *PairTree) Len() int {
	return t.length
}

type stackPair struct {
	n *node // current node
	i int   // index of the next child/item.
}

// Cursor represents an iterator that can traverse over all items in the tree
// in sorted order.
//
// Changing data while traversing a cursor may result in unexpected items to
// be returned. You must reposition your cursor after mutating data.
type Cursor struct {
	t     *PairTree
	stack []stackPair
}

// Cursor returns a new cursor used to traverse over items in the tree.
func (t *PairTree) Cursor() *Cursor {
	return &Cursor{t: t}
}

// First moves the cursor to the first item in the tree and returns that item.
func (c *Cursor) First() pair.Pair {
	c.stack = c.stack[:0]
	n := c.t.root
	if n == nil {
		return nilPair
	}
	c.stack = append(c.stack, stackPair{n: n})
	for len(n.children) > 0 {
		n = n.children[0]
		c.stack = append(c.stack, stackPair{n: n})
	}
	if len(n.items) == 0 {
		return nilPair
	}
	return n.items[0]
}

// Next moves the cursor to the next item and returns that item.
func (c *Cursor) Next() pair.Pair {
	if len(c.stack) == 0 {
		return nilPair
	}
	si := len(c.stack) - 1
	c.stack[si].i++
	n := c.stack[si].n
	i := c.stack[si].i
	if i == len(n.children)+len(n.items) {
		c.stack = c.stack[:len(c.stack)-1]
		return c.Next()
	}
	if len(n.children) == 0 {
		if i >= len(n.items) {
			c.stack = c.stack[:len(c.stack)-1]
			return c.Next()
		}
		return n.items[i]
	} else if i%2 == 1 {
		return n.items[i/2]
	}
	c.stack = append(c.stack, stackPair{n: n.children[i/2], i: -1})
	return c.Next()

}

// Last moves the cursor to the last item in the tree and returns that item.
func (c *Cursor) Last() pair.Pair {
	c.stack = c.stack[:0]
	n := c.t.root
	if n == nil {
		return nilPair
	}
	c.stack = append(c.stack, stackPair{n: n, i: len(n.children) + len(n.items) - 1})
	for len(n.children) > 0 {
		n = n.children[len(n.children)-1]
		c.stack = append(c.stack, stackPair{n: n, i: len(n.children) + len(n.items) - 1})
	}
	if len(n.items) == 0 {
		return nilPair
	}
	return n.items[len(n.items)-1]
}

// Prev moves the cursor to the previous item and returns that item.
func (c *Cursor) Prev() pair.Pair {
	if len(c.stack) == 0 {
		return nilPair
	}
	si := len(c.stack) - 1
	c.stack[si].i--
	n := c.stack[si].n
	i := c.stack[si].i
	if i == -1 {
		c.stack = c.stack[:len(c.stack)-1]
		return c.Prev()
	}
	if len(n.children) == 0 {
		return n.items[i]
	} else if i%2 == 1 {
		return n.items[i/2]
	}
	child := n.children[i/2]
	c.stack = append(c.stack, stackPair{n: child,
		i: len(child.children) + len(child.items)})
	return c.Prev()
}

// Seek moves the cursor to provided item and returns that item.
// If the item does not exist then the next item is returned.
func (c *Cursor) Seek(pivot pair.Pair) pair.Pair {
	c.stack = c.stack[:0]
	n := c.t.root
	for n != nil {
		i, found := n.items.find(pivot, c.t.less)
		c.stack = append(c.stack, stackPair{n: n})
		if found {
			if len(n.children) == 0 {
				c.stack[len(c.stack)-1].i = i
			} else {
				c.stack[len(c.stack)-1].i = i*2 + 1
			}
			return n.items[i]
		}
		if len(n.children) == 0 {
			if i == len(n.items) {
				c.stack[len(c.stack)-1].i = i + 1
				return c.Next()
			}
			c.stack[len(c.stack)-1].i = i
			return n.items[i]
		}
		c.stack[len(c.stack)-1].i = i * 2
		n = n.children[i]
	}
	return nilPair
}
