// Package retry holds the one bounded poll loop the backends, the scion client
// and the bridge previously each hand-rolled.
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
// returns nil as soon as fn reports done. A non-nil error from fn is returned
// immediately, unwrapped. Between attempts Until sleeps interval, or returns
// ctx.Err() when ctx is cancelled first; ctx is not consulted before the
// first attempt, so fn always runs at least once. After the last attempt Until
// returns ErrExhausted without sleeping.
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
