package voice

import (
	"context"
	"testing"
	"time"
)

// A waiting dictation clip goes before every waiting agent clip, whatever
// the order they came in.
func TestQueuePrefersDictation(t *testing.T) {
	var q Queue
	if err := q.Acquire(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	order := make(chan string, 3)
	started := make(chan struct{}, 3)
	wait := func(name string, high bool) {
		started <- struct{}{}
		if err := q.Acquire(context.Background(), high); err != nil {
			t.Error(err)
			return
		}
		order <- name
		q.Release()
	}
	go wait("agent", false)
	<-started
	time.Sleep(20 * time.Millisecond)
	go wait("dictation", true)
	<-started
	time.Sleep(20 * time.Millisecond)
	q.Release()
	if a, b := <-order, <-order; a != "dictation" || b != "agent" {
		t.Fatalf("order %s, %s", a, b)
	}
	// Free again: an agent clip gets it at once.
	if err := q.Acquire(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	q.Release()
}

// A waiter whose context ends leaves the queue; the GPU still passes on.
func TestQueueCanceledWaiter(t *testing.T) {
	var q Queue
	_ = q.Acquire(context.Background(), true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := q.Acquire(ctx, false); err == nil {
		t.Fatal("a canceled wait got the GPU")
	}
	q.Release()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := q.Acquire(ctx2, false); err != nil {
		t.Fatalf("the GPU did not free: %v", err)
	}
	q.Release()
}
