package ghpush

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIssues is GitHub's issues API for one repository.
type fakeIssues struct {
	mu       sync.Mutex
	srv      *httptest.Server
	issues   map[int]map[string]any
	comments map[int][]map[string]any
	posted   []string // comment bodies
	patches  []map[string]any
	order    []string // "comment" / "close", in the order they happened
}

func newFakeIssues(t *testing.T) *fakeIssues {
	f := &fakeIssues{issues: map[int]map[string]any{}, comments: map[int][]map[string]any{}}
	add := func(n int, title, state string, pr bool) {
		i := map[string]any{"number": n, "title": title, "state": state, "body": "body of " + title + "\x1b[2J‮",
			"user": map[string]any{"login": "someone"}, "labels": []any{map[string]any{"name": "bug"}},
			"comments": 0, "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z",
			"html_url": "https://github.example/issues/" + title}
		if pr {
			i["pull_request"] = map[string]any{"url": "x"}
		}
		f.issues[n] = i
	}
	add(29, "spa", "open", false)
	add(13, "backup", "open", false)
	add(39, "a pr", "open", true)
	add(5, "old", "closed", false)
	f.issues[5]["state_reason"] = "completed"
	f.issues[29]["comments"] = 2
	f.comments[29] = []map[string]any{
		{"user": map[string]any{"login": "a"}, "body": "first", "created_at": "t1"},
		{"user": map[string]any{"login": "b"}, "body": "key sk-ant-api03-abcdefghijklmnop", "created_at": "t2"},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssues) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+scoped {
		w.WriteHeader(401)
		return
	}
	j := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	p := strings.TrimPrefix(r.URL.Path, "/repos/"+repo)
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		json.Unmarshal(b, &body)
	}
	switch {
	case p == "/issues" && r.Method == "GET":
		var out []map[string]any
		for _, n := range []int{39, 29, 13, 5} {
			i := f.issues[n]
			if st := r.URL.Query().Get("state"); st == "all" || i["state"] == st {
				out = append(out, i)
			}
		}
		j(200, out)
	case strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/comments") && r.Method == "GET":
		var n int
		json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/comments")), &n)
		j(200, f.comments[n])
	case strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/comments") && r.Method == "POST":
		f.posted = append(f.posted, body["body"].(string))
		f.order = append(f.order, "comment")
		j(201, map[string]any{"html_url": "https://github.example/c/1", "body": body["body"]})
	case strings.HasPrefix(p, "/issues/"):
		var n int
		json.Unmarshal([]byte(strings.TrimPrefix(p, "/issues/")), &n)
		i, ok := f.issues[n]
		if !ok {
			j(404, map[string]string{"message": "Not Found"})
			return
		}
		if r.Method == "PATCH" {
			f.patches = append(f.patches, body)
			f.order = append(f.order, "close")
			i["state"], i["state_reason"] = body["state"], body["state_reason"]
		}
		j(200, i)
	default:
		j(404, map[string]string{"message": "Not Found"})
	}
}

func (f *fakeIssues) ops() (*Ops, *fakeTokens, *bytes.Buffer) {
	tk := &fakeTokens{}
	var logBuf bytes.Buffer
	return &Ops{Repos: map[string]bool{repo: true}, Prefix: "agent/",
		API:    &API{Base: f.srv.URL, HTTP: NewAPIClient(f.srv.Client())},
		Tokens: tk, Log: slog.New(slog.NewTextHandler(&logBuf, nil)), TextMax: 8000, Deadline: 10 * time.Second}, tk, &logBuf
}

func TestIssueListLeavesPullRequestsOut(t *testing.T) {
	f := newFakeIssues(t)
	o, tk, _ := f.ops()
	res, err := o.IssueList(context.Background(), "mgr", repo, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 2 || res.Issues[0].Number != 29 || res.Issues[1].Number != 13 || res.Note == "" {
		t.Fatalf("issues: %+v", res.Issues)
	}
	if !maps.Equal(tk.calls[0].perms, Permissions{"issues": "read"}) {
		t.Fatalf("perms %+v", tk.calls[0].perms)
	}
	res, err = o.IssueList(context.Background(), "mgr", repo, "all", "1")
	if err != nil || len(res.Issues) != 1 {
		t.Fatalf("limit: %+v %v", res.Issues, err)
	}
	_, err = o.IssueList(context.Background(), "mgr", repo, "draft", "")
	wantErr(t, err, "want open, closed or all")
	_, err = o.IssueList(context.Background(), "mgr", repo, "", "51")
	wantErr(t, err, "want 1-50")
	_, err = o.IssueList(context.Background(), "mgr", "x/y", "", "")
	wantErr(t, err, "is not one this tool")
}

func TestIssueReadCleansTextAndRefusesPRs(t *testing.T) {
	f := newFakeIssues(t)
	o, _, _ := f.ops()
	res, err := o.IssueRead(context.Background(), "mgr", repo, "29")
	if err != nil {
		t.Fatal(err)
	}
	if res.Body != "body of spa" || len(res.Comments) != 2 || res.Comments[0].Body != "first" {
		t.Fatalf("read: %q %+v", res.Body, res.Comments)
	}
	if strings.Contains(res.Comments[1].Body, "abcdefghijklmnop") {
		t.Fatal("credential-shaped text not masked")
	}
	_, err = o.IssueRead(context.Background(), "mgr", repo, "39")
	wantErr(t, err, "is a pull request")
	_, err = o.IssueRead(context.Background(), "mgr", repo, "404")
	wantErr(t, err, "not found")
}

func TestIssueCommentChecksTheText(t *testing.T) {
	f := newFakeIssues(t)
	o, tk, logBuf := f.ops()
	res, err := o.IssueComment(context.Background(), "mgr", repo, "13", "Tracked in the backlog.")
	if err != nil || res.CommentURL == "" || len(f.posted) != 1 {
		t.Fatalf("comment: %+v %v %v", res, err, f.posted)
	}
	if !maps.Equal(tk.calls[0].perms, Permissions{"issues": "write"}) {
		t.Fatalf("perms %+v", tk.calls[0].perms)
	}
	if strings.Contains(logBuf.String(), "backlog") {
		t.Fatal("comment text in the audit line")
	}
	for body, want := range map[string]string{
		"":                        "required",
		"   ":                     "blank",
		"ping @org/team":          "team mention",
		"fixes #12":               "closing keyword",
		"bad ‮ text":              "body",
		strings.Repeat("x", 8001): "body",
	} {
		_, err := o.IssueComment(context.Background(), "mgr", repo, "13", body)
		wantErr(t, err, want)
	}
	_, err = o.IssueComment(context.Background(), "mgr", repo, "39", "hi")
	wantErr(t, err, "is a pull request")
	if len(f.posted) != 1 {
		t.Fatalf("refused comments were posted: %v", f.posted)
	}
}

func TestIssueCloseCommentsThenCloses(t *testing.T) {
	f := newFakeIssues(t)
	o, _, _ := f.ops()
	res, err := o.IssueClose(context.Background(), "mgr", repo, "29", "not_planned", "Closing as won't fix.")
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "closed" || res.StateReason != "not_planned" || res.CommentURL == "" {
		t.Fatalf("close: %+v", res)
	}
	if strings.Join(f.order, ",") != "comment,close" {
		t.Fatalf("order %v", f.order)
	}
	// Already closed: refused, nothing posted.
	_, err = o.IssueClose(context.Background(), "mgr", repo, "29", "", "again")
	wantErr(t, err, "already closed")
	if len(f.posted) != 1 {
		t.Fatalf("posted on a closed issue: %v", f.posted)
	}
	// No comment: closes as completed.
	res, err = o.IssueClose(context.Background(), "mgr", repo, "13", "", "")
	if err != nil || res.StateReason != "completed" || res.CommentURL != "" {
		t.Fatalf("plain close: %+v %v", res, err)
	}
	_, err = o.IssueClose(context.Background(), "mgr", repo, "39", "", "")
	wantErr(t, err, "is a pull request")
	_, err = o.IssueClose(context.Background(), "mgr", repo, "5", "duplicate", "")
	wantErr(t, err, "want completed or not_planned")
}
