package ghpush

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCI is GitHub's Actions API for one repository plus a log host.
type fakeCI struct {
	t        *testing.T
	mu       sync.Mutex
	api      *httptest.Server
	logs     *httptest.Server // TLS, like GitHub's signed log URLs
	location string           // overrides the log redirect
	logBody  string
	logAuth  []string // Authorization headers the log host saw
	logReply int      // the log host's status (0 = 200); 302 sends it elsewhere
	logsCode int      // the logs endpoint's status instead of 302 (0 = 302)
	jobTotal int      // total_count in the jobs answer (0 = len)
	jobsAsks []string
}

func newFakeCI(t *testing.T) *fakeCI {
	f := &fakeCI{t: t}
	f.logs = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.logAuth = append(f.logAuth, r.Header.Get("Authorization"))
		body, reply := f.logBody, f.logReply
		f.mu.Unlock()
		if reply == http.StatusFound {
			http.Redirect(w, r, "https://evil.example/stolen", http.StatusFound)
			return
		}
		if reply != 0 {
			w.WriteHeader(reply)
			return
		}
		fmt.Fprint(w, body)
	}))
	f.api = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() { f.api.Close(); f.logs.Close() })
	return f
}

func (f *fakeCI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+scoped {
		w.WriteHeader(401)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/repos/"+repo)
	j := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
	switch {
	case p == "/actions/runs":
		if r.URL.Query().Get("branch") != "agent/x" {
			j(map[string]any{"total_count": 0, "workflow_runs": []any{}})
			return
		}
		var runs []map[string]any
		for i, c := range []string{"failure", "success", "failure", "cancelled", "failure"} {
			runs = append(runs, map[string]any{"id": 100 + i, "name": "CI\x1b[31m", "event": "push", "status": "completed",
				"conclusion": c, "head_sha": shaHead, "run_attempt": 1, "html_url": "https://github.example/run", "created_at": "2026-10-12T13:44:43Z"})
		}
		if n, _ := strconv.Atoi(r.URL.Query().Get("per_page")); n > 0 && n < len(runs) {
			runs = runs[:n]
		}
		j(map[string]any{"total_count": 5, "workflow_runs": runs})
	case strings.HasPrefix(p, "/actions/runs/") && strings.HasSuffix(p, "/jobs"):
		f.jobsAsks = append(f.jobsAsks, p)
		total := f.jobTotal
		if total == 0 {
			total = 2
		}
		j(map[string]any{"total_count": total, "jobs": []any{
			map[string]any{"id": 9000000002, "name": "lint", "status": "completed", "conclusion": "success"},
			map[string]any{"id": 9000000001, "name": "build-test", "status": "completed", "conclusion": "failure",
				"steps": []any{map[string]any{"name": "vet", "conclusion": "success"}, map[string]any{"name": "test", "conclusion": "failure"}}}}})
	case p == "/actions/jobs/9000000001/logs":
		if f.logsCode != 0 {
			w.WriteHeader(f.logsCode)
			j(map[string]string{"message": "Resource not accessible by integration\x1b[2J"})
			return
		}
		loc := f.location
		if loc == "" {
			loc = f.logs.URL + "/signed/log.txt?sig=abc"
		}
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusFound)
	default:
		w.WriteHeader(404)
		j(map[string]string{"message": "Not Found"})
	}
}

func (f *fakeCI) ops(t *testing.T) (*Ops, *fakeTokens, *bytes.Buffer) {
	t.Helper()
	tk := &fakeTokens{}
	var logBuf bytes.Buffer
	u, _ := url.Parse(f.logs.URL)
	return &Ops{
		Repos: map[string]bool{repo: true}, Prefix: "agent/",
		API:    &API{Base: f.api.URL, HTTP: NewAPIClient(f.api.Client())},
		Tokens: tk, Log: slog.New(slog.NewTextHandler(&logBuf, nil)), Deadline: 10 * time.Second,
		LogHosts: []string{u.Host}, LogHTTP: f.logs.Client(),
	}, tk, &logBuf
}

func TestCIRunsListsRunsAndTheJobsOfFailedOnes(t *testing.T) {
	f := newFakeCI(t)
	o, tk, logBuf := f.ops(t)
	res, err := o.CIRuns(context.Background(), "mgr", repo, "agent/x", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runs) != 5 || res.Runs[0].Name != "CI" {
		t.Fatalf("runs: %+v", res.Runs)
	}
	// Jobs for the three newest runs that did not pass, none for the green one.
	if len(f.jobsAsks) != 3 || res.Runs[1].Jobs != nil || res.Runs[4].Jobs != nil {
		t.Fatalf("jobs asked %v; runs %+v", f.jobsAsks, res.Runs)
	}
	if res.Runs[0].JobsTruncated {
		t.Fatal("jobs_truncated with every job listed")
	}
	job := res.Runs[0].Jobs[0] // failed jobs first
	if job.ID != 9000000001 || job.Conclusion != "failure" || strings.Join(job.FailedSteps, ",") != "test" {
		t.Fatalf("job: %+v", job)
	}
	if len(tk.calls) != 1 || !maps.Equal(tk.calls[0].perms, Permissions{"actions": "read"}) {
		t.Fatalf("token asked with %+v, want actions: read only", tk.calls)
	}
	if !strings.Contains(logBuf.String(), "github.ci_runs") || !strings.Contains(logBuf.String(), "decision=allow") {
		t.Fatalf("audit: %s", logBuf.String())
	}
}

func TestCIRunsRefusals(t *testing.T) {
	f := newFakeCI(t)
	o, tk, _ := f.ops(t)
	_, err := o.CIRuns(context.Background(), "mgr", "other/repo", "main", "")
	wantErr(t, err, "is not one this tool")
	_, err = o.CIRuns(context.Background(), "mgr", repo, "a..b", "")
	wantErr(t, err, "unexpected characters")
	for _, l := range []string{"0", "21", "x"} {
		_, err = o.CIRuns(context.Background(), "mgr", repo, "main", l)
		wantErr(t, err, "want 1-20")
	}
	if len(tk.calls) != 0 {
		t.Fatalf("a token was minted for a refused call: %+v", tk.calls)
	}
}

func TestCILogReadsTheSignedDownloadWithoutTheToken(t *testing.T) {
	f := newFakeCI(t)
	var b strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "2026-10-12T13:45:%02d.1234567Z line %d\n", i%60, i)
	}
	b.WriteString("2026-10-12T13:46:00.0000000Z --- FAIL: TestX (0.01s)\n")
	b.WriteString("2026-10-12T13:46:00.0000000Z \x1b[31m##[error]Process completed with exit code 1.\x1b[0m\n")
	b.WriteString("2026-10-12T13:46:01.0000000Z leaked " + scoped + " here\n")
	f.logBody = b.String()
	o, tk, _ := f.ops(t)
	res, err := o.CILog(context.Background(), "mgr", repo, "9000000001", "20")
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalLines != 303 || len(res.Tail) != 20 || !strings.HasPrefix(res.Tail[19], "leaked ") || res.Tail[0] != "line 283" {
		t.Fatalf("tail: %d %q", res.TotalLines, res.Tail)
	}
	if strings.Contains(strings.Join(res.Tail, "\n"), scoped) {
		t.Fatal("token in the tail")
	}
	if strings.Join(res.ErrorLines, "|") != "--- FAIL: TestX (0.01s)|##[error]Process completed with exit code 1." {
		t.Fatalf("error lines: %q", res.ErrorLines)
	}
	if res.Note == "" {
		t.Fatal("no data-not-instructions note")
	}
	if len(f.logAuth) != 1 || f.logAuth[0] != "" {
		t.Fatalf("the log download carried a credential: %q", f.logAuth)
	}
	if !maps.Equal(tk.calls[0].perms, Permissions{"actions": "read"}) {
		t.Fatalf("token perms %+v", tk.calls[0].perms)
	}
}

func TestCILogRefusesAnUnexpectedRedirect(t *testing.T) {
	for name, loc := range map[string]string{
		"plain http":   "http://127.0.0.1:1/log",
		"other host":   "https://evil.example/log",
		"userinfo":     "https://u:p@127.0.0.1/log",
		"suffix trick": "https://x.actions.githubusercontent.com.evil.example/log",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeCI(t)
			f.location = loc
			o, _, _ := f.ops(t)
			o.LogHosts = append(o.LogHosts, DefaultLogHosts...)
			_, err := o.CILog(context.Background(), "mgr", repo, "9000000001", "")
			wantErr(t, err, "unexpected place")
			if len(f.logAuth) != 0 {
				t.Fatal("followed a refused redirect")
			}
		})
	}
}

func TestCILogRefusalsAndMissingJob(t *testing.T) {
	f := newFakeCI(t)
	o, _, _ := f.ops(t)
	for _, id := range []string{"", "0", "12a", "99999999999999999999"} {
		_, err := o.CILog(context.Background(), "mgr", repo, id, "")
		wantErr(t, err, "job_id")
	}
	_, err := o.CILog(context.Background(), "mgr", repo, "9000000001", "401")
	wantErr(t, err, "want 1-400")
	_, err = o.CILog(context.Background(), "mgr", repo, "123", "")
	wantErr(t, err, "not found")
}

func TestLogHostAllowed(t *testing.T) {
	o := &Ops{}
	for in, want := range map[string]bool{
		"https://pipelinesghubeus1.actions.githubusercontent.com/x":     true,
		"https://productionresultssa1.blob.core.windows.net/x?sig=1":    true,
		"https://actions.githubusercontent.com/x":                       false,
		"https://evil.example/x":                                        false,
		"http://pipelines.actions.githubusercontent.com/x":              false,
		"https://pipelines.actions.githubusercontent.com.evil.example/": false,
		"https://x.blob.core.windows.net:8443/x":                        false,
		"https://x.blob.core.windows.net:443/x":                         true,
	} {
		u, _ := url.Parse(in)
		if got := o.logHostAllowed(u); got != want {
			t.Errorf("logHostAllowed(%s) = %v, want %v", in, got, want)
		}
	}
}

func TestCIRunsSaysWhenJobsAreCut(t *testing.T) {
	f := newFakeCI(t)
	f.jobTotal = 120
	o, _, _ := f.ops(t)
	res, err := o.CIRuns(context.Background(), "mgr", repo, "agent/x", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Runs[0].JobsTruncated {
		t.Fatal("a run with more jobs than one page is not marked")
	}
}

// TestCILogKeepsReadingPastAHugeLine: one line over the scanner's limit
// must neither stop the read nor hide what follows it.
func TestCILogKeepsReadingPastAHugeLine(t *testing.T) {
	f := newFakeCI(t)
	f.logBody = "a\n" + strings.Repeat("x", 3<<20) + "\n##[error]real failure\nlast\n"
	o, _, _ := f.ops(t)
	res, err := o.CILog(context.Background(), "mgr", repo, "9000000001", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalLines != 4 || res.Truncated || len(res.Tail) != 4 || res.Tail[3] != "last" {
		t.Fatalf("total=%d truncated=%v tail=%d %q", res.TotalLines, res.Truncated, len(res.Tail), res.Tail[len(res.Tail)-1])
	}
	if n := len([]rune(res.Tail[1])); n > ciLogLineMax+1 {
		t.Fatalf("huge line kept at %d runes", n)
	}
	if strings.Join(res.ErrorLines, "|") != "##[error]real failure" {
		t.Fatalf("error lines %q", res.ErrorLines)
	}
}

func TestCILogStopsAtTheByteCap(t *testing.T) {
	old := ciLogMaxBytes
	ciLogMaxBytes = 100
	defer func() { ciLogMaxBytes = old }()
	f := newFakeCI(t)
	f.logBody = strings.Repeat("0123456789\n", 50)
	o, _, _ := f.ops(t)
	res, err := o.CILog(context.Background(), "mgr", repo, "9000000001", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.TotalLines > 10 {
		t.Fatalf("truncated=%v total=%d", res.Truncated, res.TotalLines)
	}
}

func TestCILogDropsDisguisingCharacters(t *testing.T) {
	f := newFakeCI(t)
	f.logBody = "safe\u202egnp.exe\u200b\u2028x\ufeff\u0085\u009b2Jy\n"
	o, _, _ := f.ops(t)
	res, err := o.CILog(context.Background(), "mgr", repo, "9000000001", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Tail[0] != "safegnp.exex2Jy" {
		t.Fatalf("tail %q", res.Tail[0])
	}
	if got := label("ok\u202e\u0085x"); got != "okx" {
		t.Fatalf("label %q", got)
	}
}

func TestCILogLogHostAnswers(t *testing.T) {
	f := newFakeCI(t)
	f.logReply = http.StatusFound
	o, _, _ := f.ops(t)
	o.LogHTTP = nil // the production download client
	o.API.HTTP = NewAPIClient(f.logs.Client())
	o.API.Base = f.api.URL
	_, err := o.CILog(context.Background(), "mgr", repo, "9000000001", "")
	wantErr(t, err, "download failed")
	if strings.Contains(err.Error(), "sig=") || strings.Contains(err.Error(), "evil") {
		t.Fatalf("error repeats a URL: %v", err)
	}
	f.logReply = http.StatusForbidden
	o2, _, _ := f.ops(t)
	_, err = o2.CILog(context.Background(), "mgr", repo, "9000000001", "")
	wantErr(t, err, "HTTP 403")
}

func TestCILogKeepsGitHubsRefusal(t *testing.T) {
	f := newFakeCI(t)
	f.logsCode = http.StatusForbidden
	o, _, _ := f.ops(t)
	_, err := o.CILog(context.Background(), "mgr", repo, "9000000001", "")
	wantErr(t, err, "HTTP 403: Resource not accessible by integration")
	if strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("escape in error %q", err)
	}
}
