package config

import "testing"

func TestRejectNoHost(t *testing.T) {
	rejectNoHost(t, "name: x\nbackend: vmware\ntree: ./tree\nmanager: {}\n", msgUnknownBackend)
}
