package config

import (
	"fmt"
	"time"
)

// The message queue's defaults and bounds (broker.messaging.queue).
const (
	DefaultQueueMaxMessages = 20
	DefaultQueueMaxBytes    = 256 << 10
	DefaultQueueExpiry      = 7 * 24 * time.Hour

	queueMaxMessagesLimit = 1000
	queueMinBytes         = 1 << 10
	queueMaxBytesLimit    = 64 << 20
	queueMinExpiry        = time.Hour
	queueMaxExpiry        = 90 * 24 * time.Hour
)

// EffectiveMessageQueue is broker.messaging.queue with its defaults.
func (a *App) EffectiveMessageQueue() MessageQueue {
	q := MessageQueue{MaxMessages: DefaultQueueMaxMessages, MaxBytes: DefaultQueueMaxBytes, Expiry: DefaultQueueExpiry}
	if c := a.Broker.Messaging.Queue; c != nil {
		if c.MaxMessages > 0 {
			q.MaxMessages = c.MaxMessages
		}
		if c.MaxBytes > 0 {
			q.MaxBytes = c.MaxBytes
		}
		if c.Expiry > 0 {
			q.Expiry = c.Expiry
		}
	}
	return q
}

// validateMessageQueue refuses limits the queue cannot keep: negative ones,
// and ones past the bounds that keep its disk use small.
func (a *App) validateMessageQueue() error {
	c := a.Broker.Messaging.Queue
	if c == nil {
		return nil
	}
	if c.MaxMessages < 0 || c.MaxMessages > queueMaxMessagesLimit {
		return fmt.Errorf("config: broker.messaging.queue.max_messages %d out of range (1-%d, or unset for %d)", c.MaxMessages, queueMaxMessagesLimit, DefaultQueueMaxMessages)
	}
	if c.MaxBytes != 0 && (c.MaxBytes < queueMinBytes || c.MaxBytes > queueMaxBytesLimit) {
		return fmt.Errorf("config: broker.messaging.queue.max_bytes %d out of range (%d-%d, or unset for %d)", c.MaxBytes, queueMinBytes, queueMaxBytesLimit, DefaultQueueMaxBytes)
	}
	if c.Expiry != 0 && (c.Expiry < queueMinExpiry || c.Expiry > queueMaxExpiry) {
		return fmt.Errorf("config: broker.messaging.queue.expiry %s out of range (%s-%s, or unset for %s)", c.Expiry, queueMinExpiry, queueMaxExpiry, DefaultQueueExpiry)
	}
	return nil
}
