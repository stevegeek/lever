package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUntilDoneOnFirstTry(t *testing.T) {
	calls := 0
	err := Until(context.Background(), 3, time.Hour, func() (bool, error) {
		calls++
		return true, nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want nil/1", err, calls)
	}
}

func TestUntilDoneOnNthTry(t *testing.T) {
	calls := 0
	err := Until(context.Background(), 5, 0, func() (bool, error) {
		calls++
		return calls == 3, nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d, want nil/3", err, calls)
	}
}

func TestUntilErrorShortCircuits(t *testing.T) {
	boom := errors.New("boom")
	calls := 0
	err := Until(context.Background(), 5, 0, func() (bool, error) {
		calls++
		return false, boom
	})
	if err != boom || calls != 1 {
		t.Fatalf("err=%v calls=%d, want boom/1 (unwrapped, no further attempts)", err, calls)
	}
}

func TestUntilCtxCancelDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Until(ctx, 5, time.Hour, func() (bool, error) {
		calls++
		cancel()
		return false, nil
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v calls=%d, want context.Canceled/1", err, calls)
	}
}

func TestUntilExhausted(t *testing.T) {
	calls := 0
	start := time.Now()
	err := Until(context.Background(), 3, 0, func() (bool, error) {
		calls++
		return false, nil
	})
	if !errors.Is(err, ErrExhausted) || calls != 3 {
		t.Fatalf("err=%v calls=%d, want ErrExhausted/3", err, calls)
	}
	if time.Since(start) > time.Second {
		t.Fatal("exhaustion must not sleep after the last attempt")
	}
}

func TestUntilUnboundedStopsOnError(t *testing.T) {
	boom := errors.New("boom")
	calls := 0
	err := Until(context.Background(), 0, 0, func() (bool, error) {
		calls++
		if calls == 4 {
			return false, boom
		}
		return false, nil
	})
	if err != boom || calls != 4 {
		t.Fatalf("err=%v calls=%d, want boom/4", err, calls)
	}
}
