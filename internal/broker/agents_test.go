package broker

import (
	"slices"
	"sync"
	"testing"
)

// workerSpecOf is a test helper: name's spec, which must be registered.
func workerSpecOf(b *Broker, name string) WorkerSpec {
	s, ok := b.workers.get(name)
	if !ok {
		panic("no worker " + name)
	}
	return s
}

func TestAgentRegistryReads(t *testing.T) {
	r := newAgentRegistry([]WorkerSpec{{Name: "b", Image: "x"}, {Name: "a"}})
	if got := r.names(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("names = %v, want sorted [a b]", got)
	}
	s, ok := r.get("b")
	if !ok || s.Image != "x" {
		t.Fatalf("get(b) = %+v, %v", s, ok)
	}
	// get hands out a copy: editing it changes nothing registered.
	s.Image = "y"
	if s2, _ := r.get("b"); s2.Image != "x" {
		t.Fatalf("registered spec changed through a copy: %q", s2.Image)
	}
	if r.has("c") {
		t.Fatal("has(c) on an unregistered name")
	}
	r.put(WorkerSpec{Name: "c"})
	if !r.has("c") || len(r.names()) != 3 {
		t.Fatalf("put did not register c: %v", r.names())
	}
}

// Readers and a writer at once: run under -race.
func TestAgentRegistryConcurrent(t *testing.T) {
	r := newAgentRegistry([]WorkerSpec{{Name: "a"}})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = r.names()
				_, _ = r.get("a")
			}
		}()
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.put(WorkerSpec{Name: "a", Image: string(rune('a' + i))})
			}
		}(i)
	}
	wg.Wait()
}
