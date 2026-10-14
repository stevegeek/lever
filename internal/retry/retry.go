// Package retry holds the bounded poll loop shared by the backends, the scion
// client and the bridge.
package retry

import (
	"context"
	"errors"
	"time"
)

// ErrExhausted is returned by Until when every attempt ran without fn
// reporting done. Call sites wrap it with their own message.
var ErrExhausted = errors.New("retry: attempts exhausted")

// Until calls fn up to attempts times (unbounded when attempts <= 0) and
// returns nil once fn reports done. An error from fn is returned immediately,
// unwrapped. Between attempts it sleeps interval or returns ctx.Err(); fn
// always runs at least once, and ErrExhausted is returned without a final
// sleep.
func Until(ctx context.Context, attempts int, interval time.Duration, fn func() (done bool, err error)) error {
	for i := 0; attempts <= 0 || i < attempts; i++ {
		done, err := fn()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if attempts > 0 && i == attempts-1 {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return ErrExhausted
}
