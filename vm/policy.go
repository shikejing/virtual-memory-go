package vm

import "container/list"

// policyQueue tracks resident pages in policy order.
// For FIFO the front of the queue is the oldest resident.
// For LRU the queue is scanned for the smallest (last-access, page-number)
// pair, which breaks ties deterministically by page number.
type policyQueue struct {
	policy ReplacementPolicy
	l      *list.List
}

func newPolicyQueue(policy ReplacementPolicy) *policyQueue {
	return &policyQueue{policy: policy, l: list.New()}
}

// add records a newly resident page.
func (q *policyQueue) add(pn int64) {
	q.l.PushBack(pn)
}

// remove drops a page from the queue (on eviction).
func (q *policyQueue) remove(pn int64) {
	for e := q.l.Front(); e != nil; e = e.Next() {
		if e.Value.(int64) == pn {
			q.l.Remove(e)
			return
		}
	}
}

// len returns the number of resident pages tracked.
func (q *policyQueue) len() int {
	return q.l.Len()
}

// selectVictim returns the page number to evict, or -1 if no unpinned
// resident page exists.
func (q *policyQueue) selectVictim(pages map[int64]*page) int64 {
	if q.policy == PolicyFIFO {
		for e := q.l.Front(); e != nil; e = e.Next() {
			pn := e.Value.(int64)
			if pages[pn].pin == 0 {
				return pn
			}
		}
		return -1
	}
	// LRU: deterministic tie-break by page number.
	var best int64 = -1
	var bestLast int64
	for e := q.l.Front(); e != nil; e = e.Next() {
		pn := e.Value.(int64)
		p := pages[pn]
		if p.pin > 0 {
			continue
		}
		if best < 0 || p.last < bestLast || (p.last == bestLast && pn < best) {
			best = pn
			bestLast = p.last
		}
	}
	return best
}
