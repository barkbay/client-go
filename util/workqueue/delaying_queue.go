/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package workqueue

import (
	"bytes"
	"container/heap"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/clock"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

// DelayingInterface is an Interface that can Add an item at a later time. This makes it easier to
// requeue items after failures without ending up in a hot-loop.
type DelayingInterface interface {
	Interface
	// AddAfter adds an item to the workqueue after the indicated duration has passed
	AddAfter(item interface{}, duration time.Duration)
}

// NewDelayingQueue constructs a new workqueue with delayed queuing ability
func NewDelayingQueue() DelayingInterface {
	return newDelayingQueue(clock.RealClock{}, "")
}

func NewNamedDelayingQueue(name string) DelayingInterface {
	return newDelayingQueue(clock.RealClock{}, name)
}

func newDelayingQueue(clock clock.Clock, name string) DelayingInterface {
	ret := &delayingType{
		Interface:         NewNamed(name),
		clock:             clock,
		stopCh:            make(chan struct{}),
		waitingForAddCh:   make(chan *waitFor, 1000),
		metrics:           newRetryMetrics(name),
		deprecatedMetrics: newDeprecatedRetryMetrics(name),
	}

	go ret.waitingLoop()

	return ret
}

// delayingType wraps an Interface and provides delayed re-enquing
type delayingType struct {
	Interface

	// clock tracks time for delayed firing
	clock clock.Clock

	// stopCh lets us signal a shutdown to the waiting loop
	stopCh chan struct{}

	// waitingForAddCh is a buffered channel that feeds waitingForAdd
	waitingForAddCh chan *waitFor

	// metrics counts the number of retries
	metrics           retryMetrics
	deprecatedMetrics retryMetrics
}

// waitFor holds the data to add and the time it should be added
type waitFor struct {
	data    t
	readyAt time.Time
	// index in the priority queue (heap)
	index int
}

// waitForPriorityQueue implements a priority queue for waitFor items.
//
// waitForPriorityQueue implements heap.Interface. The item occurring next in
// time (i.e., the item with the smallest readyAt) is at the root (index 0).
// Peek returns this minimum item at index 0. Pop returns the minimum item after
// it has been removed from the queue and placed at index Len()-1 by
// container/heap. Push adds an item at index Len(), and container/heap
// percolates it into the correct location.
type waitForPriorityQueue []*waitFor

func (pq waitForPriorityQueue) String() string {
	var sb strings.Builder
	for _, str := range pq {
		sb.WriteString(fmt.Sprintf("%v,",str.data))
	}
	return sb.String()
}

func (pq waitForPriorityQueue) Len() int {
	return len(pq)
}
func (pq waitForPriorityQueue) Less(i, j int) bool {
	return pq[i].readyAt.Before(pq[j].readyAt)
}
func (pq waitForPriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

// Push adds an item to the queue. Push should not be called directly; instead,
// use `heap.Push`.
func (pq *waitForPriorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*waitFor)
	item.index = n
	*pq = append(*pq, item)
}

// Pop removes an item from the queue. Pop should not be called directly;
// instead, use `heap.Pop`.
func (pq *waitForPriorityQueue) Pop() interface{} {
	n := len(*pq)
	item := (*pq)[n-1]
	item.index = -1
	*pq = (*pq)[0:(n - 1)]
	return item
}

// Peek returns the item at the beginning of the queue, without removing the
// item or otherwise mutating the queue. It is safe to call directly.
func (pq waitForPriorityQueue) Peek() interface{} {
	return pq[0]
}

// ShutDown gives a way to shut off this queue
func (q *delayingType) ShutDown() {
	q.Interface.ShutDown()
	close(q.stopCh)
}

// AddAfter adds the given item to the work queue after the given delay
func (q *delayingType) AddAfter(item interface{}, duration time.Duration) {
	// don't add if we're already shutting down
	if q.ShuttingDown() {
		return
	}

	q.metrics.retry()
	q.deprecatedMetrics.retry()

	// immediately add things with no delay
	if duration <= 0 {
		q.Add(item)
		return
	}

	select {
	case <-q.stopCh:
		// unblock if ShutDown() is called
	case q.waitingForAddCh <- &waitFor{data: item, readyAt: q.clock.Now().Add(duration)}:
	}
}

func getGID() uint64 {
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	b = b[:bytes.IndexByte(b, ' ')]
	n, _ := strconv.ParseUint(string(b), 10, 64)
	return n
}

// maxWait keeps a max bound on the wait time. It's just insurance against weird things happening.
// Checking the queue every 10 seconds isn't expensive and we know that we'll never end up with an
// expired item sitting for more than 10 seconds.
const maxWait = 10 * time.Second

// waitingLoop runs until the workqueue is shutdown and keeps a check on the list of items to be added.
func (q *delayingType) waitingLoop() {
	defer utilruntime.HandleCrash()

	waitingForQueue := &waitForPriorityQueue{}
	heap.Init(waitingForQueue)

	waitingEntryByData := map[t]*waitFor{}

	var nextReadyAt *time.Timer

	for {
		if q.Interface.ShuttingDown() {
			return
		}

		now := q.clock.Now()

		// Add ready entries
		for waitingForQueue.Len() > 0 {
			entry := waitingForQueue.Peek().(*waitFor)
			if entry.readyAt.After(now) {
				break
			}

			entry = heap.Pop(waitingForQueue).(*waitFor)
			q.Add(entry.data)
			delete(waitingEntryByData, entry.data)
		}

		// Stop any previous timer
		if nextReadyAt != nil {
			fmt.Printf("%d - stop nextReady\n",getGID())
			nextReadyAt.Stop()
		}

		// Even if queue is empty enforce a heartbeat
		if waitingForQueue.Len() == 0 {
			nextReadyAt = time.NewTimer(maxWait)
		} else if waitingForQueue.Len() > 0 {
			entry := waitingForQueue.Peek().(*waitFor)
			afterValue := entry.readyAt.Sub(now)
			fmt.Printf("%d - queue: %v - %v - afterValue: %v\n", getGID(), *waitingForQueue, entry.data, afterValue)
			nextReadyAt = time.NewTimer(afterValue)
		}

		select {
		case <-q.stopCh:
			if nextReadyAt != nil {
				nextReadyAt.Stop()
			}
			return

		case <-nextReadyAt.C:
			// continue the loop, which will add ready items

		case waitEntry := <-q.waitingForAddCh:
			if waitEntry.readyAt.After(q.clock.Now()) {
				insert(waitingForQueue, waitingEntryByData, waitEntry)
			} else {
				q.Add(waitEntry.data)
			}

			drained := false
			for !drained {
				select {
				case waitEntry := <-q.waitingForAddCh:
					if waitEntry.readyAt.After(q.clock.Now()) {
						insert(waitingForQueue, waitingEntryByData, waitEntry)
					} else {
						q.Add(waitEntry.data)
					}
				default:
					drained = true
				}
			}
		}
	}
}

// insert adds the entry to the priority queue, or updates the readyAt if it already exists in the queue
func insert(q *waitForPriorityQueue, knownEntries map[t]*waitFor, entry *waitFor) {
	// if the entry already exists, update the time only if it would cause the item to be queued sooner
	existing, exists := knownEntries[entry.data]
	if exists {
		if existing.readyAt.After(entry.readyAt) {
			existing.readyAt = entry.readyAt
			heap.Fix(q, existing.index)
		}

		return
	}

	heap.Push(q, entry)
	knownEntries[entry.data] = entry
}
