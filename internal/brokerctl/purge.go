package brokerctl

import (
	"context"
	"fmt"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
)

// RecordDeleter is the scion call a worker purge makes (*scion.Client).
type RecordDeleter interface {
	Delete(ctx context.Context, worker, project string) error
}

// PurgeWorker is the one teardown of a worker record: `lever worker purge`
// and the broker's recycle route both run it. It deletes the worker's scion
// record in project, then its staged ticket directory in the guest. It never
// touches the worker's workspace: that is its work product.
//
// err is the record delete's failure, with nothing else done. ticketErr is
// the ticket removal's failure after the record is gone: only a warning, as
// the next start stages a fresh ticket over whatever is left.
func PurgeWorker(ctx context.Context, sc RecordDeleter, jr proc.Runner, worker, project string) (ticketErr, err error) {
	if err := sc.Delete(ctx, worker, project); err != nil {
		return nil, fmt.Errorf("deleting worker %q scion record: %w", worker, err)
	}
	return jail.RemoveWorkerTicket(ctx, jr, worker), nil
}

// jailWorkerPurger is broker.WorkerPurger over the host scion client and the
// jail runner, in the instance project.
type jailWorkerPurger struct {
	sc      RecordDeleter
	r       proc.Runner
	project string
}

func (p jailWorkerPurger) PurgeWorker(ctx context.Context, worker string) (ticketErr, err error) {
	return PurgeWorker(ctx, p.sc, p.r, worker, p.project)
}
