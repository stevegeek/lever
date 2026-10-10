package voice

import (
	"context"
	"sync"
)

// Queue lets one transcription at a time reach whisper-server (which works
// on one clip at a time anyway, whisper.go assumption 4). A waiting
// dictation clip (the operator's, from the remote proxy) always goes before
// a waiting agent clip; within each kind, first come first served.
type Queue struct {
	mu        sync.Mutex
	busy      bool
	high, low []chan struct{}
}

// Acquire waits for the GPU. high is a dictation clip. It returns ctx's
// error when ctx ends first; otherwise the caller must Release.
func (q *Queue) Acquire(ctx context.Context, high bool) error {
	q.mu.Lock()
	if !q.busy && len(q.high) == 0 && (high || len(q.low) == 0) {
		q.busy = true
		q.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	if high {
		q.high = append(q.high, ch)
	} else {
		q.low = append(q.low, ch)
	}
	q.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		q.mu.Lock()
		if remove(&q.high, ch) || remove(&q.low, ch) {
			q.mu.Unlock()
			return ctx.Err()
		}
		q.mu.Unlock()
		// Granted meanwhile: hand it on.
		q.Release()
		return ctx.Err()
	}
}

// Release frees the GPU for the next waiter, dictation first.
func (q *Queue) Release() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, l := range []*[]chan struct{}{&q.high, &q.low} {
		if len(*l) > 0 {
			ch := (*l)[0]
			*l = (*l)[1:]
			close(ch) // busy stays true: the GPU passes to the waiter
			return
		}
	}
	q.busy = false
}

func remove(l *[]chan struct{}, ch chan struct{}) bool {
	for i, c := range *l {
		if c == ch {
			*l = append((*l)[:i:i], (*l)[i+1:]...)
			return true
		}
	}
	return false
}
