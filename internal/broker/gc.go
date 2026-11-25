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
// its archive (archives.go: the combined directory, or the old split
// workspace archive) and its conversation backups, and drop its record,
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

// errCollectBusy: another verb holds the agent's lock (a restore reading
// its archive).
var errCollectBusy = errors.New("busy")

// collect deletes destroyed agent name's archive and backups and retires its
// record; dryRun only counts. A failure leaves the record, so a later gc
// tries again.
func (b *Broker) collect(name string, dryRun bool) (int64, error) {
	unlock, ok := b.tryLockWorker(name)
	if !ok {
		return 0, errCollectBusy
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
	if b.archiveDir != "" {
		n, err := b.removeArchiveDir(name, dryRun)
		if err != nil {
			return freed, err
		}
		freed += n
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

// removeArchiveDir deletes name's combined archive directory, which holds
// only regular files, and returns their size; dryRun only counts.
func (b *Broker) removeArchiveDir(name string, dryRun bool) (int64, error) {
	p := path.Join(b.archiveDir, name)
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if _, err := b.agentArchiveDir(name, false); err != nil {
		return 0, fmt.Errorf("its archive: %w", err)
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return 0, fmt.Errorf("its archive: %w", err)
	}
	var size int64
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			return 0, fmt.Errorf("its archive: %w", err)
		}
		if !fi.Mode().IsRegular() {
			return 0, fmt.Errorf("its archive holds %s, not a regular file; left for the operator", e.Name())
		}
		size += fi.Size()
	}
	if dryRun {
		return size, nil
	}
	for _, e := range ents {
		if err := os.Remove(path.Join(p, e.Name())); err != nil {
			return 0, fmt.Errorf("removing its archive: %w", err)
		}
	}
	if err := os.Remove(p); err != nil {
		return 0, fmt.Errorf("removing its archive: %w", err)
	}
	return size, nil
}
