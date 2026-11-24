package broker

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path"
	"time"
)

// `lever agent gc`: for each agent destroyed at least OlderThan ago, delete
// its workspace archive and its conversation backups, and drop its record,
// keeping its name as retired (AgentsState.Retired) so it is never drawn
// again and stays revoked. Only the operator's socket serves it. A dry run
// changes nothing and reports what would go.

// PathOperatorAgentGC is `lever agent gc`'s route on the operator socket.
const PathOperatorAgentGC = "/operator/agent/gc"

// GCRequest is the body of PathOperatorAgentGC.
type GCRequest struct {
	// OlderThan is in seconds: only agents destroyed at least that long ago.
	OlderThan int64 `json:"older_than_seconds"`
	DryRun    bool  `json:"dry_run,omitempty"`
}

// GCResponse answers PathOperatorAgentGC.
type GCResponse struct {
	// Agents are the destroyed agents collected (or, on a dry run, that
	// would be).
	Agents []string `json:"agents"`
	// Freed is the bytes of archives and backups deleted (or that would be).
	Freed  int64 `json:"freed_bytes"`
	DryRun bool  `json:"dry_run,omitempty"`
	// Warnings are agents left as they were, and why.
	Warnings []string `json:"warnings,omitempty"`
}

func (b *Broker) handleOperatorAgentGC(w http.ResponseWriter, r *http.Request) {
	var req GCRequest
	if err := decodeStrict(w, r, smallBodyLimit, &req); err != nil || req.OlderThan < 0 || req.OlderThan > math.MaxInt64/int64(time.Second) {
		b.audit("agent", operatorActor, "deny", "gc: bad body")
		refuse(w, ReasonBadRequest, "bad request", http.StatusBadRequest)
		return
	}
	cutoff := b.now().Add(-time.Duration(req.OlderThan) * time.Second)
	out := GCResponse{Agents: []string{}, DryRun: req.DryRun}
	for _, rec := range b.workers.recordList() {
		if rec.State != AgentDestroyed {
			continue
		}
		if rec.DestroyedAt.IsZero() || rec.DestroyedAt.After(cutoff) {
			continue
		}
		freed, err := b.collect(rec.Name, req.DryRun)
		if err != nil {
			out.Warnings = append(out.Warnings, rec.Name+": "+err.Error())
			b.audit("agent", operatorActor, "error", "gc "+rec.Name+": "+err.Error())
			continue
		}
		out.Agents = append(out.Agents, rec.Name)
		out.Freed += freed
	}
	if !req.DryRun {
		b.audit("agent", operatorActor, "allow", fmt.Sprintf("gc: %d destroyed agent(s) collected, %d bytes freed", len(out.Agents), out.Freed))
	}
	writeJSON(w, out)
}

// collect deletes destroyed agent name's archive and backups and retires its
// record; dryRun only counts. A failure leaves the record, so a later gc
// tries again.
func (b *Broker) collect(name string, dryRun bool) (int64, error) {
	unlock, ok := b.tryLockWorker(name)
	if !ok {
		return 0, errors.New("busy")
	}
	defer unlock()
	var freed int64
	if b.archiveDir != "" {
		p := path.Join(b.archiveDir, name+".tar.gz")
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return 0, fmt.Errorf("its archive: %w", err)
		case !fi.Mode().IsRegular():
			return 0, errors.New("its archive is not a regular file; left for the operator")
		default:
			freed += fi.Size()
			if !dryRun {
				if err := os.Remove(p); err != nil {
					return 0, fmt.Errorf("removing its archive: %w", err)
				}
			}
		}
	}
	if b.removeConversations != nil {
		n, err := b.removeConversations(name, dryRun)
		if err != nil {
			return freed, fmt.Errorf("its conversation backups: %w", err)
		}
		freed += n
	}
	if dryRun {
		return freed, nil
	}
	if err := b.workers.retire(name); err != nil {
		return freed, fmt.Errorf("the registry did not record it as retired: %w", err)
	}
	return freed, nil
}
