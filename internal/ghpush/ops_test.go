package ghpush

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	shaMain = "1111111111111111111111111111111111111111"
	shaHead = "2222222222222222222222222222222222222222"
	shaNew  = "3333333333333333333333333333333333333333"
	scoped  = "ghs_SCOPEDTOKEN"
)

type scopedCall struct {
	repo  string
	perms Permissions
}

type fakeTokens struct {
	mu    sync.Mutex
	calls []scopedCall
	err   error
}

func (f *fakeTokens) TokenFor(_ context.Context, repo string, perms Permissions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, scopedCall{repo, maps.Clone(perms)})
	return scoped, f.err
}

type fakeRun struct{ Name, Status, Conclusion string }
type fakeStatus struct{ Context, State string }

// fakeGH is an in-memory GitHub REST API for one repository.
type fakeGH struct {
	t             *testing.T
	mu            sync.Mutex
	repo          string
	defaultBranch string
	heads         map[string]string // branch -> sha
	tags          []string
	pulls         map[int]map[string]any
	runs          map[string][]fakeRun
	runsTotal     int // overrides total_count when set
	statuses      map[string][]fakeStatus
	mergeStatus   int              // forces the merge answer's status
	created       []map[string]any // POST /pulls bodies
	refs          []map[string]any // POST /git/refs bodies
	hideOpenList  int              // list calls that miss the open PRs (a race)
	tagRace       bool             // the tag appears before the ref is created
	merges        []map[string]any
	reviews       map[int][]map[string]any
	retargetTo    string // the PR's base after a merge (it was retargeted)
	reqs          []string
	srv           *httptest.Server
}

func newFakeGH(t *testing.T) *fakeGH {
	f := &fakeGH{t: t, repo: repo, defaultBranch: "main",
		heads:    map[string]string{"main": shaMain, "agent/x": shaHead},
		pulls:    map[int]map[string]any{},
		runs:     map[string][]fakeRun{},
		statuses: map[string][]fakeStatus{},
		reviews:  map[int][]map[string]any{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// review is one review as the reviews API returns it.
func review(id int64, login, typ, state, commit string) map[string]any {
	return map[string]any{"id": id * 1000, "state": state, "commit_id": commit,
		"user": map[string]any{"login": login, "id": id, "type": typ}}
}

// addPull adds an open, mergeable PR from agent/x into main, approved on its
// head by a person.
func (f *fakeGH) addPull(n int, mod func(p map[string]any)) {
	f.reviews[n] = []map[string]any{review(1, "alice", "User", "APPROVED", shaHead)}
	p := map[string]any{
		"number": n, "state": "open", "merged": false, "draft": false, "mergeable": true,
		"mergeable_state": "clean", "html_url": fmt.Sprintf("https://github.example/%s/pull/%d", repo, n),
		"head": map[string]any{"ref": "agent/x", "sha": shaHead, "repo": map[string]any{"full_name": repo}},
		"base": map[string]any{"ref": "main", "sha": shaMain, "repo": map[string]any{"full_name": repo}},
	}
	if mod != nil {
		mod(p)
	}
	f.pulls[n] = p
}

func (f *fakeGH) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var (
	reReviews = regexp.MustCompile(`^/pulls/([0-9]+)/reviews$`)
	rePull    = regexp.MustCompile(`^/pulls/([0-9]+)$`)
	reMerge   = regexp.MustCompile(`^/pulls/([0-9]+)/merge$`)
	reRuns    = regexp.MustCompile(`^/commits/([0-9a-f]+)/check-runs$`)
	reStatus  = regexp.MustCompile(`^/commits/([0-9a-f]+)/status$`)
)

func (f *fakeGH) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
	if r.Header.Get("Authorization") != "Bearer "+scoped {
		f.json(w, 401, map[string]string{"message": "Bad credentials"})
		return
	}
	if strings.Contains(r.URL.RawQuery, scoped) || strings.Contains(r.URL.Path, scoped) {
		f.t.Errorf("token in URL %s", r.URL)
	}
	prefix := "/repos/" + f.repo
	if !strings.HasPrefix(r.URL.Path, prefix) {
		f.json(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	p := strings.TrimPrefix(r.URL.Path, prefix)
	var body map[string]any
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			_ = json.Unmarshal(b, &body)
		}
	}
	q := r.URL.Query()
	switch {
	case p == "" && r.Method == "GET":
		f.json(w, 200, map[string]any{"full_name": f.repo, "default_branch": f.defaultBranch})
	case strings.HasPrefix(p, "/git/ref/heads/") && r.Method == "GET":
		b := strings.TrimPrefix(p, "/git/ref/heads/")
		sha, ok := f.heads[b]
		if !ok {
			f.json(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		f.json(w, 200, map[string]any{"ref": "refs/heads/" + b, "object": map[string]any{"sha": sha, "type": "commit"}})
	case strings.HasPrefix(p, "/git/ref/tags/") && r.Method == "GET":
		tg := strings.TrimPrefix(p, "/git/ref/tags/")
		for _, x := range f.tags {
			if x == tg {
				f.json(w, 200, map[string]any{"ref": "refs/tags/" + tg, "object": map[string]any{"sha": shaMain, "type": "commit"}})
				return
			}
		}
		f.json(w, 404, map[string]string{"message": "Not Found"})
	case p == "/pulls" && r.Method == "GET":
		var out []map[string]any
		for _, pl := range f.pulls {
			if f.hideOpenList > 0 {
				break
			}
			h := pl["head"].(map[string]any)
			if q.Get("state") == "open" && pl["state"] == "open" && q.Get("head") == strings.Split(f.repo, "/")[0]+":"+h["ref"].(string) {
				out = append(out, pl)
			}
		}
		if f.hideOpenList > 0 {
			f.hideOpenList--
		}
		if out == nil {
			out = []map[string]any{}
		}
		f.json(w, 200, out)
	case p == "/pulls" && r.Method == "POST":
		for _, pl := range f.pulls {
			if pl["state"] == "open" && pl["head"].(map[string]any)["ref"] == body["head"] {
				f.json(w, 422, map[string]any{"message": "Validation Failed", "errors": []map[string]string{{"message": "A pull request already exists for x:agent/x."}}})
				return
			}
		}
		f.created = append(f.created, body)
		n := 100 + len(f.created)
		f.addPull(n, func(pl map[string]any) { pl["head"].(map[string]any)["ref"] = body["head"] })
		f.json(w, 201, f.pulls[n])
	case rePull.MatchString(p) && r.Method == "GET":
		n, _ := strconv.Atoi(rePull.FindStringSubmatch(p)[1])
		pl, ok := f.pulls[n]
		if !ok {
			f.json(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		f.json(w, 200, pl)
	case reMerge.MatchString(p) && r.Method == "PUT":
		n, _ := strconv.Atoi(reMerge.FindStringSubmatch(p)[1])
		f.merges = append(f.merges, body)
		if f.mergeStatus != 0 {
			f.json(w, f.mergeStatus, map[string]string{"message": "Head branch was modified. Review and try the merge again. " + scoped})
			return
		}
		pl := f.pulls[n]
		if body["sha"] != pl["head"].(map[string]any)["sha"] {
			f.json(w, 409, map[string]string{"message": "Head branch was modified"})
			return
		}
		pl["state"], pl["merged"] = "closed", true
		if f.retargetTo != "" {
			pl["base"].(map[string]any)["ref"] = f.retargetTo
		}
		f.json(w, 200, map[string]any{"merged": true, "sha": shaNew, "message": "Pull Request successfully merged"})
	case reReviews.MatchString(p) && r.Method == "GET":
		n, _ := strconv.Atoi(reReviews.FindStringSubmatch(p)[1])
		page, _ := strconv.Atoi(q.Get("page"))
		per, _ := strconv.Atoi(q.Get("per_page"))
		if page < 1 || per != 100 {
			f.t.Errorf("reviews query %q", r.URL.RawQuery)
		}
		out := []map[string]any{}
		all := f.reviews[n]
		for i := (page - 1) * per; i < len(all) && i < page*per; i++ {
			out = append(out, all[i])
		}
		f.json(w, 200, out)
	case reRuns.MatchString(p) && r.Method == "GET":
		if q.Get("filter") != "latest" || q.Get("per_page") != "100" {
			f.t.Errorf("check runs query %q", r.URL.RawQuery)
		}
		var runs []map[string]any
		for _, x := range f.runs[reRuns.FindStringSubmatch(p)[1]] {
			var c any
			if x.Conclusion != "" {
				c = x.Conclusion
			}
			runs = append(runs, map[string]any{"name": x.Name, "status": x.Status, "conclusion": c})
		}
		total := len(runs)
		if f.runsTotal != 0 {
			total = f.runsTotal
		}
		f.json(w, 200, map[string]any{"total_count": total, "check_runs": runs})
	case reStatus.MatchString(p) && r.Method == "GET":
		var sts []map[string]any
		state := "pending"
		for _, x := range f.statuses[reStatus.FindStringSubmatch(p)[1]] {
			sts = append(sts, map[string]any{"context": x.Context, "state": x.State})
			state = x.State
		}
		f.json(w, 200, map[string]any{"state": state, "total_count": len(sts), "statuses": sts})
	case p == "/tags" && r.Method == "GET":
		page, _ := strconv.Atoi(q.Get("page"))
		per, _ := strconv.Atoi(q.Get("per_page"))
		var out []map[string]any
		for i := (page - 1) * per; i < len(f.tags) && i < page*per; i++ {
			out = append(out, map[string]any{"name": f.tags[i]})
		}
		if out == nil {
			out = []map[string]any{}
		}
		f.json(w, 200, out)
	case p == "/git/refs" && r.Method == "POST":
		ref, _ := body["ref"].(string)
		tg := strings.TrimPrefix(ref, "refs/tags/")
		for _, x := range f.tags {
			if x == tg {
				f.json(w, 422, map[string]string{"message": "Reference already exists"})
				return
			}
		}
		if f.tagRace {
			f.json(w, 422, map[string]string{"message": "Reference already exists"})
			return
		}
		f.refs = append(f.refs, body)
		f.tags = append(f.tags, tg)
		f.json(w, 201, map[string]any{"ref": ref, "object": map[string]any{"sha": body["sha"], "type": "commit"}})
	case p == "/releases" && r.Method == "POST":
		f.t.Errorf("release must create only the tag; a GitHub release was requested: %v", body)
		f.json(w, 201, map[string]any{})
	default:
		f.json(w, 404, map[string]string{"message": "Not Found"})
	}
}

func newOps(t *testing.T, f *fakeGH) (*Ops, *fakeTokens, *bytes.Buffer) {
	t.Helper()
	tk := &fakeTokens{}
	var logBuf bytes.Buffer
	o := &Ops{
		Repos: map[string]bool{repo: true}, Prefix: "agent/",
		API:    &API{Base: f.srv.URL, HTTP: NewAPIClient(f.srv.Client())},
		Tokens: tk, Log: slog.New(slog.NewTextHandler(&logBuf, nil)),
		TextMax: 8000, MergeMethods: []string{"squash"}, MergeChecks: CheckPolicy{Any: true},
		ReleaseTag: regexp.MustCompile(`^(?:` + DefaultReleaseTag + `)$`), Deadline: 10 * time.Second,
	}
	return o, tk, &logBuf
}

func wantErr(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("want an error containing %q, got %v", sub, err)
	}
	if strings.Contains(err.Error(), scoped) {
		t.Fatalf("token in error: %v", err)
	}
}

func TestOpPermissionsNarrowing(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}}
	o, tk, _ := newOps(t, f)
	ctx := context.Background()
	if _, err := o.PROpen(ctx, "manager", repo, "agent/x", "t", "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.PRStatus(ctx, "manager", repo, "7"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.PRMerge(ctx, "manager", repo, "7", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Release(ctx, "manager", repo, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	want := []Permissions{
		{"contents": "read", "pull_requests": "write"},
		{"pull_requests": "read", "checks": "read", "statuses": "read"},
		{"contents": "write", "pull_requests": "write", "checks": "read", "statuses": "read"},
		{"contents": "write"},
	}
	if len(tk.calls) != len(want) {
		t.Fatalf("token calls %v", tk.calls)
	}
	for i, c := range tk.calls {
		if c.repo != repo || !maps.Equal(c.perms, want[i]) {
			t.Errorf("call %d: %s %v, want %v", i, c.repo, c.perms, want[i])
		}
	}
	for _, p := range OpPermissions {
		if p["workflows"] != "" || p["administration"] != "" {
			t.Fatalf("an op asks for a CI or admin permission: %v", p)
		}
	}
}

func TestPROpen(t *testing.T) {
	f := newFakeGH(t)
	o, _, logBuf := newOps(t, f)
	body := "secret-ish body text\n\twith a tab"
	res, err := o.PROpen(context.Background(), "manager", repo, "agent/x", "Add a thing", body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Existing || res.Number != 101 || res.Base != "main" || !strings.HasSuffix(res.URL, "/pull/101") {
		t.Fatalf("res %+v", res)
	}
	c := f.created[0]
	if c["base"] != "main" || c["head"] != "agent/x" || c["title"] != "Add a thing" || c["body"] != body || c["maintainer_can_modify"] != false {
		t.Fatalf("create body %v", c)
	}
	log := logBuf.String()
	if strings.Contains(log, "secret-ish") || strings.Contains(log, "Add a thing") {
		t.Fatalf("audit line holds the text: %s", log)
	}
	if !strings.Contains(log, "github.pr_open") || !strings.Contains(log, "body_len="+strconv.Itoa(len(body))) ||
		!strings.Contains(log, "body_sha256=") || !strings.Contains(log, "decision=allow") || !strings.Contains(log, "caller=manager") {
		t.Fatalf("audit line %s", log)
	}

	// A second call returns the open PR instead of opening another.
	res2, err := o.PROpen(context.Background(), "manager", repo, "agent/x", "Again", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res2.OK || !res2.Existing || res2.Number != 101 || len(f.created) != 1 {
		t.Fatalf("existing PR: %+v, %d created", res2, len(f.created))
	}
}

func TestPROpenBaseIsTheDefaultBranch(t *testing.T) {
	f := newFakeGH(t)
	f.defaultBranch = "trunk"
	f.heads["trunk"] = shaMain
	o, _, _ := newOps(t, f)
	res, err := o.PROpen(context.Background(), "manager", repo, "agent/x", "t", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Base != "trunk" || f.created[0]["base"] != "trunk" {
		t.Fatalf("base %q / %v", res.Base, f.created[0]["base"])
	}
}

func TestPROpenRefusals(t *testing.T) {
	f := newFakeGH(t)
	o, _, logBuf := newOps(t, f)
	ctx := context.Background()
	for _, c := range []struct {
		repo, branch, title, body, want string
	}{
		{"other/repo", "agent/x", "t", "", "not one this tool"},
		{repo, "main", "t", "", "must start with"},
		{repo, "agent/main", "t", "", "protected"},
		{repo, "agent/missing", "t", "", "does not exist on GitHub"},
		{repo, "agent/x", "", "", "title must be"},
		{repo, "agent/x", "   ", "", "title must be"},
		{repo, "agent/x", strings.Repeat("é", 257), "", "title must be"},
		{repo, "agent/x", "a\nb", "", "control"},
		{repo, "agent/x", "a\tb", "", "control"},
		{repo, "agent/x", "t", "a\rb", "control"},
		{repo, "agent/x", "t", "a\x1b[31mb", "control"},
		{repo, "agent/x", "t", "a\u202eb", "formatting"},
		{repo, "agent/x", "t", "\xff", "UTF-8"},
		{repo, "agent/x", "t", strings.Repeat("x", 8001), "max 8000"},
	} {
		_, err := o.PROpen(ctx, "manager", c.repo, c.branch, c.title, c.body)
		wantErr(t, err, c.want)
	}
	if len(f.created) != 0 {
		t.Fatalf("a refused call opened a PR: %v", f.created)
	}
	if !strings.Contains(logBuf.String(), "decision=deny") {
		t.Fatal("no deny audit line")
	}
	// Exactly at the bounds is fine.
	if _, err := o.PROpen(ctx, "manager", repo, "agent/x", strings.Repeat("é", 256), strings.Repeat("x", 8000)); err != nil {
		t.Fatal(err)
	}
}

func TestPRStatus(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}, {"lint", "in_progress", ""}}
	f.statuses[shaHead] = []fakeStatus{{"ci/legacy", "pending"}}
	o, _, _ := newOps(t, f)
	res, err := o.PRStatus(context.Background(), "manager", repo, "7")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.State != "open" || res.Mergeable == nil || !*res.Mergeable || res.MergeableState != "clean" ||
		res.HeadSHA != shaHead || res.Base != "main" || res.Head != "agent/x" || res.ChecksTotal != 3 || res.Truncated {
		t.Fatalf("res %+v", res)
	}
	want := []Check{{"test", "check_run", "completed", "success"}, {"lint", "check_run", "in_progress", ""}, {"ci/legacy", "status", "pending", ""}}
	if len(res.Checks) != 3 {
		t.Fatalf("checks %+v", res.Checks)
	}
	for i := range want {
		if res.Checks[i] != want[i] {
			t.Errorf("check %d: %+v, want %+v", i, res.Checks[i], want[i])
		}
	}
	f.runsTotal = 250
	res, err = o.PRStatus(context.Background(), "manager", repo, "7")
	if err != nil || !res.Truncated || res.ChecksTotal != 251 {
		t.Fatalf("truncated: %+v %v", res, err)
	}
	for _, n := range []string{"0", "-1", "x", "1.5", "12345678901", ""} {
		_, err := o.PRStatus(context.Background(), "manager", repo, n)
		wantErr(t, err, "number")
	}
	_, err = o.PRStatus(context.Background(), "manager", repo, "99")
	wantErr(t, err, "not found")
	_, err = o.PRStatus(context.Background(), "manager", "other/repo", "7")
	wantErr(t, err, "not one this tool")
}

func TestPRMerge(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}, {"docs", "completed", "skipped"}, {"opt", "completed", "neutral"}}
	f.statuses[shaHead] = []fakeStatus{{"ci/legacy", "success"}}
	o, _, logBuf := newOps(t, f)
	res, err := o.PRMerge(context.Background(), "manager", repo, "7", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.MergeSHA != shaNew || res.HeadSHA != shaHead || res.Method != "squash" {
		t.Fatalf("res %+v", res)
	}
	if len(f.merges) != 1 || f.merges[0]["sha"] != shaHead || f.merges[0]["merge_method"] != "squash" {
		t.Fatalf("merge body %v", f.merges)
	}
	if !strings.Contains(logBuf.String(), "github.pr_merge") || !strings.Contains(logBuf.String(), "number=7") {
		t.Fatalf("audit %s", logBuf.String())
	}
}

func TestPRMergeRefusals(t *testing.T) {
	green := []fakeRun{{"test", "completed", "success"}}
	for _, c := range []struct {
		name     string
		mod      func(p map[string]any)
		runs     []fakeRun
		statuses []fakeStatus
		policy   string
		method   string
		number   string
		want     string
	}{
		{name: "closed", mod: func(p map[string]any) { p["state"] = "closed" }, want: "not open"},
		{name: "merged", mod: func(p map[string]any) { p["state"], p["merged"] = "closed", true }, want: "already merged"},
		{name: "draft", mod: func(p map[string]any) { p["draft"] = true }, want: "draft"},
		{name: "fork", mod: func(p map[string]any) { p["head"].(map[string]any)["repo"] = map[string]any{"full_name": "evil/lever"} }, want: "fork"},
		{name: "deleted fork", mod: func(p map[string]any) { p["head"].(map[string]any)["repo"] = nil }, want: "fork"},
		{name: "head prefix", mod: func(p map[string]any) { p["head"].(map[string]any)["ref"] = "feature/x" }, want: "must start with"},
		{name: "base", mod: func(p map[string]any) { p["base"].(map[string]any)["ref"] = "release" }, want: "not the default branch"},
		{name: "unknown mergeable", mod: func(p map[string]any) { p["mergeable"] = nil }, want: "still computing"},
		{name: "not mergeable", mod: func(p map[string]any) { p["mergeable"], p["mergeable_state"] = false, "dirty" }, want: "not mergeable (dirty)"},
		{name: "no checks", runs: []fakeRun{}, want: "no successful check"},
		{name: "pending", runs: []fakeRun{{"test", "completed", "success"}, {"lint", "queued", ""}}, want: "lint (queued)"},
		{name: "failed", runs: []fakeRun{{"test", "completed", "failure"}}, want: "test (failure)"},
		{name: "cancelled", runs: []fakeRun{{"test", "completed", "cancelled"}}, want: "test (cancelled)"},
		{name: "status failure", statuses: []fakeStatus{{"ci/legacy", "error"}}, want: "ci/legacy (error)"},
		{name: "status pending", statuses: []fakeStatus{{"ci/legacy", "pending"}}, want: "ci/legacy (pending)"},
		{name: "required missing", policy: "test,e2e", want: `"e2e" is missing`},
		{name: "required neutral", runs: []fakeRun{{"test", "completed", "neutral"}, {"lint", "completed", "success"}}, policy: "test", want: `"test" is missing or did not succeed`},
		{name: "method", method: "rebase", want: "not allowed"},
		{name: "bad number", number: "7/merge", want: "number"},
		{name: "unknown PR", number: "8", want: "not found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeGH(t)
			f.addPull(7, c.mod)
			f.runs[shaHead] = green
			if c.runs != nil {
				f.runs[shaHead] = c.runs
			}
			f.statuses[shaHead] = c.statuses
			o, _, _ := newOps(t, f)
			if c.policy != "" {
				pol, err := ParseCheckPolicy(c.policy)
				if err != nil {
					t.Fatal(err)
				}
				o.MergeChecks = pol
			}
			n := "7"
			if c.number != "" {
				n = c.number
			}
			_, err := o.PRMerge(context.Background(), "manager", repo, n, c.method)
			wantErr(t, err, c.want)
			if len(f.merges) != 0 {
				t.Fatalf("a refused merge reached GitHub: %v", f.merges)
			}
		})
	}
}

func TestPRMergeCheckPolicies(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	o, _, _ := newOps(t, f)
	o.MergeChecks = CheckPolicy{None: true}
	if _, err := o.PRMerge(context.Background(), "manager", repo, "7", ""); err != nil {
		t.Fatalf("-merge-checks none must merge a PR without checks: %v", err)
	}
	// none still refuses a red check.
	f.addPull(8, nil)
	f.runs[shaHead] = []fakeRun{{"test", "completed", "failure"}}
	_, err := o.PRMerge(context.Background(), "manager", repo, "8", "")
	wantErr(t, err, "not all green")
	// named checks present and successful merge.
	f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}}
	f.statuses[shaHead] = []fakeStatus{{"e2e", "success"}}
	o.MergeChecks = CheckPolicy{Names: []string{"test", "e2e"}}
	if _, err := o.PRMerge(context.Background(), "manager", repo, "8", ""); err != nil {
		t.Fatal(err)
	}
}

func TestPRMergeRefusesWithoutACheckPolicy(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}}
	o, _, _ := newOps(t, f)
	for _, pol := range []CheckPolicy{{}, {Any: true, None: true}, {Any: true, Names: []string{"test"}}} {
		o.MergeChecks = pol
		_, err := o.PRMerge(context.Background(), "manager", repo, "7", "")
		wantErr(t, err, "no merge check policy")
	}
	if len(f.merges) != 0 {
		t.Fatalf("merged without a policy: %v", f.merges)
	}
}

func TestPRMergeTooManyChecks(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}}
	f.runsTotal = 101
	o, _, _ := newOps(t, f)
	_, err := o.PRMerge(context.Background(), "manager", repo, "7", "")
	wantErr(t, err, "cannot verify them all")
}

func TestPRMergeHeadMoved(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}}
	f.mergeStatus = http.StatusConflict
	o, _, _ := newOps(t, f)
	_, err := o.PRMerge(context.Background(), "manager", repo, "7", "")
	wantErr(t, err, "head moved")
	if len(f.merges) != 1 {
		t.Fatalf("want exactly one merge attempt (no retry), got %d", len(f.merges))
	}
}

func TestPRMergeMethods(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}}
	o, _, _ := newOps(t, f)
	o.MergeMethods = []string{"merge", "rebase"}
	if _, err := o.PRMerge(context.Background(), "manager", repo, "7", "rebase"); err != nil {
		t.Fatal(err)
	}
	if f.merges[0]["merge_method"] != "rebase" {
		t.Fatalf("method %v", f.merges[0])
	}
	f.addPull(8, nil)
	_, err := o.PRMerge(context.Background(), "manager", repo, "8", "squash")
	wantErr(t, err, "not allowed")
}

func TestRelease(t *testing.T) {
	f := newFakeGH(t)
	f.tags = []string{"v0.9.0", "v0.35.0", "v0.4.1", "nightly", "v1.0.0-rc.1"}
	o, _, logBuf := newOps(t, f)
	o.WebBase = "https://github.example"
	res, err := o.Release(context.Background(), "manager", repo, "v0.36.0")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Tag != "v0.36.0" || res.Target != shaMain || res.URL != "https://github.example/"+repo+"/tree/v0.36.0" ||
		res.CommitURL != "https://github.example/"+repo+"/commit/"+shaMain {
		t.Fatalf("res %+v", res)
	}
	if len(f.refs) != 1 || f.refs[0]["ref"] != "refs/tags/v0.36.0" || f.refs[0]["sha"] != shaMain {
		t.Fatalf("tag ref %v", f.refs)
	}
	for _, r := range f.reqs {
		if strings.Contains(r, "/releases") {
			t.Fatalf("release must not call the releases API: %v", f.reqs)
		}
	}
	log := logBuf.String()
	if !strings.Contains(log, "github.release") || !strings.Contains(log, "tag=v0.36.0") || !strings.Contains(log, "target="+shaMain) {
		t.Fatalf("audit %s", log)
	}
	// The default web base.
	o.WebBase = ""
	res, err = o.Release(context.Background(), "manager", repo, "v0.37.0")
	if err != nil || res.URL != "https://github.com/"+repo+"/tree/v0.37.0" {
		t.Fatalf("res %+v %v", res, err)
	}
}

func TestReleaseTargetsTheDefaultBranchHead(t *testing.T) {
	f := newFakeGH(t)
	f.defaultBranch = "trunk"
	f.heads["trunk"] = shaNew
	o, _, _ := newOps(t, f)
	res, err := o.Release(context.Background(), "manager", repo, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if res.Target != shaNew || f.refs[0]["sha"] != shaNew {
		t.Fatalf("target %q / %v", res.Target, f.refs[0])
	}
}

func TestReleaseRefusals(t *testing.T) {
	for _, c := range []struct {
		name, tag string
		tags      []string
		want      string
	}{
		{name: "pattern", tag: "1.2.3", want: "does not match"},
		{name: "prerelease", tag: "v1.2.3-rc.1", want: "does not match"},
		{name: "charset", tag: "v1.2.3/x", want: "want letters"},
		{name: "dash", tag: "-v1", want: "want letters"},
		{name: "exists", tag: "v0.35.0", tags: []string{"v0.35.0"}, want: "already exists"},
		{name: "older", tag: "v0.34.9", tags: []string{"v0.35.0"}, want: `not greater than the existing tag "v0.35.0"`},
		{name: "numeric not lexical", tag: "v0.9.0", tags: []string{"v0.10.0"}, want: "not greater"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeGH(t)
			f.tags = c.tags
			o, _, _ := newOps(t, f)
			_, err := o.Release(context.Background(), "manager", repo, c.tag)
			wantErr(t, err, c.want)
			if len(f.refs) != 0 {
				t.Fatalf("a refused release reached GitHub: %v", f.refs)
			}
		})
	}
	f := newFakeGH(t)
	o, _, _ := newOps(t, f)
	_, err := o.Release(context.Background(), "manager", "other/repo", "v1.0.0")
	wantErr(t, err, "not one this tool")
}

func TestReleaseTagAppearsAfterTheCheck(t *testing.T) {
	f := newFakeGH(t)
	f.tagRace = true
	o, _, _ := newOps(t, f)
	_, err := o.Release(context.Background(), "manager", repo, "v1.0.0")
	wantErr(t, err, "create tag v1.0.0: refused")
}

func TestPROpenRaceReturnsTheExistingPR(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.hideOpenList = 1 // the first list misses PR 7; the create then gets a 422
	o, _, _ := newOps(t, f)
	res, err := o.PROpen(context.Background(), "manager", repo, "agent/x", "t", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Existing || res.Number != 7 || len(f.created) != 0 {
		t.Fatalf("res %+v, created %v", res, f.created)
	}
}

func TestPRMergeSkippedOnlyIsNotChecked(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.runs[shaHead] = []fakeRun{{"docs", "completed", "skipped"}, {"opt", "completed", "neutral"}}
	o, _, _ := newOps(t, f)
	_, err := o.PRMerge(context.Background(), "manager", repo, "7", "")
	wantErr(t, err, "no successful check")
	o.MergeChecks = CheckPolicy{None: true}
	if _, err := o.PRMerge(context.Background(), "manager", repo, "7", ""); err != nil {
		t.Fatalf("-merge-checks none: %v", err)
	}
}

func TestPRMergeCheckNamesAreSanitised(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.runs[shaHead] = []fakeRun{{"evil\nline\x1b[2J" + strings.Repeat("x", 300), "completed", "failure"}}
	o, _, _ := newOps(t, f)
	_, err := o.PRMerge(context.Background(), "manager", repo, "7", "")
	wantErr(t, err, "evil line")
	if strings.ContainsAny(err.Error(), "\n\x1b") || strings.Contains(err.Error(), strings.Repeat("x", 101)) {
		t.Fatalf("check name not sanitised: %q", err)
	}
}

func TestTextRunes(t *testing.T) {
	for _, s := range []string{"a\u200bb", "a\ufeffb", "a\u2066b", "a\u00adb", "a\u2028b"} {
		if ValidateTitle(s) == nil || ValidateText("body", s, 100) == nil {
			t.Errorf("%q must be refused", s)
		}
	}
	for _, s := range []string{"Fix the 👩\u200d💻 page", "naïve café 日本語"} {
		if err := ValidateTitle(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestReleaseTagPagination(t *testing.T) {
	f := newFakeGH(t)
	for i := 0; i < 150; i++ {
		f.tags = append(f.tags, fmt.Sprintf("v0.%d.0", i))
	}
	o, _, _ := newOps(t, f)
	_, err := o.Release(context.Background(), "manager", repo, "v0.149.0")
	wantErr(t, err, "already exists")
	_, err = o.Release(context.Background(), "manager", repo, "v0.148.1")
	wantErr(t, err, "not greater")
	if _, err := o.Release(context.Background(), "manager", repo, "v0.150.0"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxTagPages*100; i++ {
		f.tags = append(f.tags, fmt.Sprintf("x%d", i))
	}
	_, err = o.Release(context.Background(), "manager", repo, "v9.0.0")
	wantErr(t, err, "cannot compare them all")
}

func TestReleaseUnorderableExistingTag(t *testing.T) {
	f := newFakeGH(t)
	f.tags = []string{"v1.0.0", "release-x"}
	o, _, _ := newOps(t, f)
	o.ReleaseTag = regexp.MustCompile(`^(?:v[0-9.]+|release-.*)$`)
	_, err := o.Release(context.Background(), "manager", repo, "v2.0.0")
	wantErr(t, err, "not a semantic version")
}

func TestOpsAPIErrorsAreScrubbedAndBounded(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}}
	f.mergeStatus = http.StatusMethodNotAllowed
	o, _, logBuf := newOps(t, f)
	_, err := o.PRMerge(context.Background(), "manager", repo, "7", "")
	wantErr(t, err, "405")
	if strings.Contains(logBuf.String(), scoped) {
		t.Fatal("token in the audit log")
	}
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 2<<10))
	}))
	defer big.Close()
	a := &API{Base: big.URL, HTTP: NewAPIClient(big.Client()), MaxResponse: 1 << 10}
	err = a.do(context.Background(), scoped, http.MethodGet, "/x", nil, nil)
	wantErr(t, err, "over 1024 bytes")
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusMovedPermanently)
	}))
	defer redir.Close()
	a = &API{Base: redir.URL, HTTP: NewAPIClient(redir.Client())}
	err = a.do(context.Background(), scoped, http.MethodGet, "/x", nil, nil)
	wantErr(t, err, "redirected")
}

func TestTokenErrorRefuses(t *testing.T) {
	f := newFakeGH(t)
	o, tk, _ := newOps(t, f)
	tk.err = fmt.Errorf("github app token: HTTP 422: permission not granted")
	_, err := o.PROpen(context.Background(), "manager", repo, "agent/x", "t", "")
	wantErr(t, err, "422")
	if len(f.reqs) != 0 {
		t.Fatalf("API called without a token: %v", f.reqs)
	}
}

func TestParseCheckPolicy(t *testing.T) {
	for in, want := range map[string]CheckPolicy{
		"any":              {Any: true},
		"none":             {None: true},
		"test":             {Names: []string{"test"}},
		" test , e2e ,e2e": {Names: []string{"test", "e2e"}},
	} {
		got, err := ParseCheckPolicy(in)
		if err != nil || got.Any != want.Any || got.None != want.None || strings.Join(got.Names, ",") != strings.Join(want.Names, ",") {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", " ", "test,", "any,test", "a\x01b"} {
		if _, err := ParseCheckPolicy(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestSemverOrder(t *testing.T) {
	order := []string{"v0.9.0", "v0.10.0", "v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0", "1.0.1", "v99999999999999999999.0.0"}
	for i := 0; i+1 < len(order); i++ {
		a, err := parseSemver(order[i])
		if err != nil {
			t.Fatal(err)
		}
		b, err := parseSemver(order[i+1])
		if err != nil {
			t.Fatal(err)
		}
		if compareSemver(a, b) != -1 || compareSemver(b, a) != 1 {
			t.Errorf("%s < %s", order[i], order[i+1])
		}
	}
	a, _ := parseSemver("v1.0.0+build.1")
	b, _ := parseSemver("v1.0.0")
	if compareSemver(a, b) != 0 {
		t.Error("build metadata must not order")
	}
	for _, bad := range []string{"v01.0.0", "v1.0", "1.0.0.0", "vv1.0.0", "v1.0.0-"} {
		if _, err := parseSemver(bad); err == nil {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestSemverHyphenIdentifierIsNotNumeric(t *testing.T) {
	// "-1" is an alphanumeric identifier: it orders after every numeric one,
	// and by ASCII among alphanumeric ones.
	order := []string{"v1.0.0-1", "v1.0.0-2", "v1.0.0--1", "v1.0.0--2", "v1.0.0-a"}
	for i := 0; i+1 < len(order); i++ {
		a, err := parseSemver(order[i])
		if err != nil {
			t.Fatal(err)
		}
		b, err := parseSemver(order[i+1])
		if err != nil {
			t.Fatal(err)
		}
		if compareSemver(a, b) != -1 || compareSemver(b, a) != 1 {
			t.Errorf("%s < %s", order[i], order[i+1])
		}
	}
	if x, ok := numericID("-1"); ok {
		t.Errorf("-1 read as numeric %v", x)
	}
	if _, ok := numericID("+1"); ok {
		t.Error("+1 read as numeric")
	}
}

func TestPRMergeReviewGate(t *testing.T) {
	const shaOld = "4444444444444444444444444444444444444444"
	appr := func(id int64, login, typ, commit string) map[string]any {
		return review(id, login, typ, "APPROVED", commit)
	}
	for _, c := range []struct {
		name    string
		slug    string
		reviews []map[string]any
		want    string // "" = merges
		by      string
	}{
		{name: "approved on head", reviews: []map[string]any{appr(1, "alice", "User", shaHead)}, by: "alice"},
		{name: "no reviews", reviews: nil, want: "no approving review"},
		{name: "approval of an older commit", reviews: []map[string]any{appr(1, "alice", "User", shaOld)}, want: "no approving review"},
		{name: "bot approval", reviews: []map[string]any{appr(2, "ci-helper[bot]", "Bot", shaHead)}, want: "no approving review"},
		{name: "app approval by slug", slug: "lever-dev", reviews: []map[string]any{appr(3, "lever-dev[bot]", "User", shaHead)}, want: "no approving review"},
		{name: "app approval, plain slug", slug: "Lever-Dev", reviews: []map[string]any{appr(3, "lever-dev", "User", shaHead)}, want: "no approving review"},
		{name: "organization", reviews: []map[string]any{appr(4, "org", "Organization", shaHead)}, want: "no approving review"},
		{name: "deleted user", reviews: []map[string]any{{"id": 1, "state": "APPROVED", "commit_id": shaHead, "user": nil}}, want: "no approving review"},
		{name: "commented only", reviews: []map[string]any{review(1, "alice", "User", "COMMENTED", shaHead)}, want: "no approving review"},
		{name: "dismissed", reviews: []map[string]any{review(1, "alice", "User", "DISMISSED", shaHead)}, want: "no approving review"},
		{name: "changes requested", reviews: []map[string]any{appr(1, "alice", "User", shaHead), review(2, "bob", "User", "CHANGES_REQUESTED", shaOld)}, want: "changes requested by bob"},
		{name: "changes requested by a bot block too", reviews: []map[string]any{appr(1, "alice", "User", shaHead), review(2, "x[bot]", "Bot", "CHANGES_REQUESTED", shaHead)}, want: "changes requested by x[bot]"},
		{name: "approval after own change request", reviews: []map[string]any{review(1, "alice", "User", "CHANGES_REQUESTED", shaOld), appr(1, "alice", "User", shaHead)}, by: "alice"},
		{name: "change request after own approval", reviews: []map[string]any{appr(1, "alice", "User", shaHead), review(1, "alice", "User", "CHANGES_REQUESTED", shaHead)}, want: "changes requested by alice"},
		{name: "comment after approval keeps it", reviews: []map[string]any{appr(1, "alice", "User", shaHead), review(1, "alice", "User", "COMMENTED", shaHead)}, by: "alice"},
		{name: "comment after change request keeps it", reviews: []map[string]any{review(2, "bob", "User", "CHANGES_REQUESTED", shaOld), review(2, "bob", "User", "COMMENTED", shaHead), appr(1, "alice", "User", shaHead)}, want: "changes requested by bob"},
		{name: "dismissed change request", reviews: []map[string]any{review(2, "bob", "User", "DISMISSED", shaOld), appr(1, "alice", "User", shaHead)}, by: "alice"},
		{name: "two approvers", reviews: []map[string]any{appr(1, "alice", "User", shaHead), appr(2, "carol", "User", shaHead)}, by: "alice,carol"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeGH(t)
			f.addPull(7, nil)
			f.reviews[7] = c.reviews
			f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}}
			o, _, logBuf := newOps(t, f)
			o.AppSlug = c.slug
			res, err := o.PRMerge(context.Background(), "manager", repo, "7", "")
			if c.want != "" {
				wantErr(t, err, c.want)
				if len(f.merges) != 0 {
					t.Fatalf("an unreviewed merge reached GitHub: %v", f.merges)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(res.ApprovedBy, ",") != c.by || len(f.merges) != 1 {
				t.Fatalf("approved_by %v, merges %v", res.ApprovedBy, f.merges)
			}
			if !strings.Contains(logBuf.String(), "approved_by="+c.by) || !strings.Contains(logBuf.String(), "review_gate=true") {
				t.Fatalf("audit %s", logBuf.String())
			}
		})
	}
}

func TestPRMergeReviewPagination(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}}
	var rs []map[string]any
	for i := 0; i < 150; i++ {
		rs = append(rs, review(int64(10+i), fmt.Sprintf("u%d", i), "User", "COMMENTED", shaHead))
	}
	// The approval is on page 2, and a change request after it on page 2 too.
	f.reviews[7] = append(rs, review(1, "alice", "User", "APPROVED", shaHead))
	o, _, _ := newOps(t, f)
	if _, err := o.PRMerge(context.Background(), "manager", repo, "7", ""); err != nil {
		t.Fatal(err)
	}
	f.addPull(8, nil)
	f.reviews[8] = append(append([]map[string]any{}, f.reviews[7]...), review(2, "bob", "User", "CHANGES_REQUESTED", shaHead))
	_, err := o.PRMerge(context.Background(), "manager", repo, "8", "")
	wantErr(t, err, "changes requested by bob")
	f.addPull(9, nil)
	for i := 0; i < maxReviewPages*100; i++ {
		f.reviews[9] = append(f.reviews[9], review(int64(10+i), "u", "User", "COMMENTED", shaHead))
	}
	_, err = o.PRMerge(context.Background(), "manager", repo, "9", "")
	wantErr(t, err, "cannot verify them all")
}

func TestPRMergeAllowUnreviewed(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.reviews[7] = nil
	f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}}
	o, _, logBuf := newOps(t, f)
	o.AllowUnreviewed = true
	if _, err := o.PRMerge(context.Background(), "manager", repo, "7", ""); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.reqs {
		if strings.Contains(r, "/reviews") {
			t.Fatalf("reviews read with the gate off: %v", f.reqs)
		}
	}
	if !strings.Contains(logBuf.String(), "review_gate=false") {
		t.Fatalf("audit %s", logBuf.String())
	}
}

func TestPRMergeReportsARetarget(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, nil)
	f.runs[shaHead] = []fakeRun{{"test", "completed", "success"}}
	o, _, logBuf := newOps(t, f)
	res, err := o.PRMerge(context.Background(), "manager", repo, "7", "")
	if err != nil || res.Retargeted || res.Base != "main" || res.Warning != "" {
		t.Fatalf("res %+v %v", res, err)
	}
	f.addPull(8, nil)
	f.retargetTo = "release"
	res, err = o.PRMerge(context.Background(), "manager", repo, "8", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.Retargeted || res.Base != "release" || !strings.Contains(res.Warning, `retargeted: it merged into "release"`) {
		t.Fatalf("res %+v", res)
	}
	log := logBuf.String()
	if !strings.Contains(log, "event=retargeted") || !strings.Contains(log, "base=release") || !strings.Contains(log, "retargeted=true") {
		t.Fatalf("audit %s", log)
	}
}
