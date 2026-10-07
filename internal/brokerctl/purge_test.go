package brokerctl

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

type fakeDeleter struct {
	calls []string
	err   error
}

func (d *fakeDeleter) Delete(_ context.Context, worker, project string) error {
	d.calls = append(d.calls, worker+"@"+project)
	return d.err
}

// PurgeWorker deletes the record, then the guest ticket directory, and runs
// nothing else: no command names the workspace.
func TestPurgeWorkerDeletesRecordThenTicket(t *testing.T) {
	sc := &fakeDeleter{}
	jr := proc.NewFakeRunner()
	jr.Script("sh -c", proc.Result{})
	ticketErr, err := PurgeWorker(context.Background(), sc, jr, "deal-1", "/lever")
	if err != nil || ticketErr != nil {
		t.Fatalf("PurgeWorker = %v, %v", ticketErr, err)
	}
	if len(sc.calls) != 1 || sc.calls[0] != "deal-1@/lever" {
		t.Fatalf("deletes = %v", sc.calls)
	}
	if len(jr.Calls) != 1 || !strings.HasSuffix(jr.Calls[0].Argv(), " _ deal-1") {
		t.Fatalf("guest calls = %+v, want the one ticket removal", jr.Calls)
	}
}

func TestPurgeWorkerStopsWhenTheDeleteFails(t *testing.T) {
	sc := &fakeDeleter{err: errors.New("hub down")}
	jr := proc.NewFakeRunner()
	_, err := PurgeWorker(context.Background(), sc, jr, "deal-1", "/lever")
	if err == nil || !strings.Contains(err.Error(), "deleting worker \"deal-1\" scion record") {
		t.Fatalf("err = %v", err)
	}
	if len(jr.Calls) != 0 {
		t.Fatal("the ticket must stay when the record delete failed")
	}
}

func TestPurgeWorkerReportsATicketFailureSeparately(t *testing.T) {
	sc := &fakeDeleter{}
	jr := proc.NewFakeRunner() // unscripted: the removal fails
	ticketErr, err := PurgeWorker(context.Background(), sc, jr, "deal-1", "/lever")
	if err != nil || ticketErr == nil {
		t.Fatalf("PurgeWorker = %v, %v: want only a ticket error", ticketErr, err)
	}
}
