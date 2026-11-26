package config

import (
	"testing"
	"time"
)

func TestMessageQueueConfig(t *testing.T) {
	base := `name: demo
backend: lima
tree: ./tree
manager:
  image: lever.local/lever-claude:latest
broker:
  llm_auth: subscription
`
	app, err := LoadNoHostChecks(writeConfig(t, base))
	if err != nil {
		t.Fatal(err)
	}
	if got := app.EffectiveMessageQueue(); got != (MessageQueue{MaxMessages: 20, MaxBytes: 256 << 10, Expiry: 7 * 24 * time.Hour}) || app.Broker.Messaging.Queue != nil {
		t.Fatalf("defaults %+v (set %+v)", got, app.Broker.Messaging.Queue)
	}
	app, err = LoadNoHostChecks(writeConfig(t, base+"  messaging:\n    queue:\n      max_messages: 5\n      max_bytes: 4096\n      expiry: 48h\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := app.EffectiveMessageQueue(); got != (MessageQueue{MaxMessages: 5, MaxBytes: 4096, Expiry: 48 * time.Hour}) {
		t.Fatalf("set %+v", got)
	}
	app, err = LoadNoHostChecks(writeConfig(t, base+"  messaging:\n    queue:\n      max_messages: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := app.EffectiveMessageQueue(); got.MaxMessages != 3 || got.MaxBytes != DefaultQueueMaxBytes || got.Expiry != DefaultQueueExpiry {
		t.Fatalf("partly set %+v", got)
	}
	q := "  messaging:\n    queue:\n"
	rejectNoHost(t, base+q+"      max_messages: -1\n", "broker.messaging.queue.max_messages -1 out of range")
	rejectNoHost(t, base+q+"      max_messages: 1001\n", "max_messages 1001 out of range")
	rejectNoHost(t, base+q+"      max_bytes: 100\n", "max_bytes 100 out of range")
	rejectNoHost(t, base+q+"      max_bytes: 999999999\n", "max_bytes 999999999 out of range")
	rejectNoHost(t, base+q+"      expiry: 1m\n", "expiry 1m0s out of range")
	rejectNoHost(t, base+q+"      expiry: 2400h\n", "out of range")
}
