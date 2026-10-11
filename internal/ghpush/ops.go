package ghpush

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The operations this package offers besides push. Each is opt-in (the
// tool's -ops flag) and its own fine-gated capability.
const (
	OpPush     = "push"
	OpPROpen   = "pr_open"
	OpPRStatus = "pr_status"
	OpPRMerge  = "pr_merge"
	OpRelease  = "release"
)

// AllOps lists every operation in a stable order.
var AllOps = []string{OpPush, OpPROpen, OpPRStatus, OpPRMerge, OpRelease}

// OpPermissions is the App permission set each operation's token is
// narrowed to. GitHub adds metadata: read to every token.
var OpPermissions = map[string]Permissions{
	OpPush:     PushPermissions,
	OpPROpen:   {"contents": "read", "pull_requests": "write"},
	OpPRStatus: {"pull_requests": "read", "checks": "read", "statuses": "read"},
	OpPRMerge:  {"contents": "write", "pull_requests": "write", "checks": "read", "statuses": "read"},
	OpRelease:  {"contents": "write"},
}

// MergeMethods are the merge methods GitHub accepts.
var MergeMethods = []string{"squash", "merge", "rebase"}

// DefaultReleaseTag is the default -release-tag pattern.
const DefaultReleaseTag = `^v[0-9]+\.[0-9]+\.[0-9]+$`

// Bounds on what one call reads from GitHub.
const (
	maxCheckRuns = 100 // one page of check runs (and of statuses) per head SHA
	maxTagPages  = 20  // 100 tags a page
)

// CheckPolicy is what pr_merge requires of the checks on the head SHA
// beyond "every one of them passed": at least one check (Any), nothing more
// (None), or the named checks present and successful (Names). The zero
// value is no policy, and pr_merge refuses every merge with it.
type CheckPolicy struct {
	Any   bool
	None  bool
	Names []string
}

// set reports whether p is one of the three policies.
func (p CheckPolicy) set() bool {
	n := 0
	for _, b := range []bool{p.Any, p.None, len(p.Names) > 0} {
		if b {
			n++
		}
	}
	return n == 1
}

var checkNameRE = regexp.MustCompile(`^[^\x00-\x1f\x7f,]{1,200}$`)

// ParseCheckPolicy reads -merge-checks: "any" (at least one check, all
// green), "none" (no check needed; any that exist must still be green), or
// a comma list of check names that must be present and successful. There is
// no default: an empty value is refused.
func ParseCheckPolicy(s string) (CheckPolicy, error) {
	switch strings.TrimSpace(s) {
	case "any":
		return CheckPolicy{Any: true}, nil
	case "none":
		return CheckPolicy{None: true}, nil
	}
	var p CheckPolicy
	for _, n := range strings.Split(s, ",") {
		n = strings.TrimSpace(n)
		if !checkNameRE.MatchString(n) || n == "any" || n == "none" {
			return CheckPolicy{}, fmt.Errorf("-merge-checks: want any, none, or a comma list of check names; got %q", s)
		}
		if !slices.Contains(p.Names, n) {
			p.Names = append(p.Names, n)
		}
	}
	return p, nil
}

// Ops runs the PR and release operations against the GitHub REST API, each
// with a token narrowed to one repository and OpPermissions[op].
type Ops struct {
	Repos        map[string]bool
	Prefix       string // required head branch prefix, e.g. "agent/"
	API          *API
	Tokens       ScopedTokenSource
	Log          *slog.Logger
	TextMax      int      // bytes of PR body or release notes
	MergeMethods []string // allowed merge methods; the first is the default
	MergeChecks  CheckPolicy
	ReleaseTag   *regexp.Regexp // matched against the whole tag
	Deadline     time.Duration  // bounds one whole call (0 = none)

	relOnce sync.Once
	relLock chan struct{}
}

// PRResult is what pr_open returns.
type PRResult struct {
	OK       bool   `json:"ok"`
	Existing bool   `json:"existing"`
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	URL      string `json:"html_url"`
	Head     string `json:"head"`
	Base     string `json:"base"`
}

// Check is one check run or commit status on a head SHA.
type Check struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"` // check_run or status
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// PRStatusResult is what pr_status returns.
type PRStatusResult struct {
	OK             bool    `json:"ok"`
	Repo           string  `json:"repo"`
	Number         int     `json:"number"`
	URL            string  `json:"html_url"`
	State          string  `json:"state"`
	Merged         bool    `json:"merged"`
	Draft          bool    `json:"draft"`
	Mergeable      *bool   `json:"mergeable"`
	MergeableState string  `json:"mergeable_state"`
	Head           string  `json:"head"`
	HeadSHA        string  `json:"head_sha"`
	Base           string  `json:"base"`
	CombinedStatus string  `json:"combined_status"`
	Checks         []Check `json:"checks"`
	ChecksTotal    int     `json:"checks_total"`
	Truncated      bool    `json:"truncated"`
}

// MergeResult is what pr_merge returns.
type MergeResult struct {
	OK       bool    `json:"ok"`
	Repo     string  `json:"repo"`
	Number   int     `json:"number"`
	Method   string  `json:"method"`
	HeadSHA  string  `json:"head_sha"`
	MergeSHA string  `json:"merge_sha"`
	Checks   []Check `json:"checks"`
}

// ReleaseResult is what release returns.
type ReleaseResult struct {
	OK     bool   `json:"ok"`
	Repo   string `json:"repo"`
	Tag    string `json:"tag"`
	Target string `json:"target_sha"`
	URL    string `json:"html_url"`
}

type ghRepo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

type ghRef struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

type ghPullSide struct {
	Ref  string  `json:"ref"`
	SHA  string  `json:"sha"`
	Repo *ghRepo `json:"repo"`
}

type ghPull struct {
	Number         int        `json:"number"`
	State          string     `json:"state"`
	Merged         bool       `json:"merged"`
	Draft          bool       `json:"draft"`
	Mergeable      *bool      `json:"mergeable"`
	MergeableState string     `json:"mergeable_state"`
	HTMLURL        string     `json:"html_url"`
	Head           ghPullSide `json:"head"`
	Base           ghPullSide `json:"base"`
}

type ghCheckRuns struct {
	Total int `json:"total_count"`
	Runs  []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

type ghStatuses struct {
	State    string `json:"state"`
	Total    int    `json:"total_count"`
	Statuses []struct {
		Context string `json:"context"`
		State   string `json:"state"`
	} `json:"statuses"`
}

// textDigest is what the audit line keeps of outward-facing text: its
// length and a sha256 prefix, never the text.
func textDigest(s string) (int, string) {
	h := sha256.Sum256([]byte(s))
	return len(s), hex.EncodeToString(h[:])[:16]
}

func (o *Ops) begin(ctx context.Context, op, repo string) (context.Context, context.CancelFunc, string, error) {
	cancel := context.CancelFunc(func() {})
	if o.Deadline > 0 {
		ctx, cancel = context.WithTimeout(ctx, o.Deadline)
	}
	if err := ValidateRepo(repo, o.Repos); err != nil {
		return ctx, cancel, "", err
	}
	tok, err := o.Tokens.TokenFor(ctx, repo, OpPermissions[op])
	if err != nil {
		return ctx, cancel, "", err
	}
	return ctx, cancel, tok, nil
}

func (o *Ops) audit(op string, err error, attrs ...any) {
	decision, detail := "allow", ""
	if err != nil {
		decision, detail = "deny", err.Error()
	}
	o.Log.Info("github."+op, append(attrs, "decision", decision, "detail", detail)...)
}

func (o *Ops) repoInfo(ctx context.Context, tok, repo string) (ghRepo, error) {
	var r ghRepo
	if err := o.API.do(ctx, tok, http.MethodGet, "/repos/"+repo, nil, &r); err != nil {
		return r, fmt.Errorf("read repository: %w", err)
	}
	if r.DefaultBranch == "" || ValidateRefName(r.DefaultBranch) != nil {
		return r, fmt.Errorf("read repository: unusable default branch %q", r.DefaultBranch)
	}
	return r, nil
}

// ValidateRefName is a loose check of a branch name GitHub reported, before
// it goes into a URL path.
func ValidateRefName(b string) error {
	if len(b) == 0 || len(b) > 255 || !branchRE.MatchString(b) || strings.Contains(b, "..") {
		return fmt.Errorf("ref %q: unexpected characters", b)
	}
	return nil
}

func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// headSHA reads refs/heads/<branch>; found is false on a 404.
func (o *Ops) headSHA(ctx context.Context, tok, repo, branch string) (sha string, found bool, err error) {
	var ref ghRef
	err = o.API.do(ctx, tok, http.MethodGet, "/repos/"+repo+"/git/ref/heads/"+escapeRef(branch), nil, &ref)
	if isStatus(err, http.StatusNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read branch %s: %w", branch, err)
	}
	if ref.Ref != "refs/heads/"+branch || ref.Object.Type != "commit" || !shaRE.MatchString(ref.Object.SHA) {
		return "", false, fmt.Errorf("read branch %s: unexpected answer", branch)
	}
	return ref.Object.SHA, true, nil
}

// PROpen opens a PR from branch into the repository's default branch, or
// returns the open PR that already has that head.
func (o *Ops) PROpen(ctx context.Context, caller, repo, branch, title, body string) (res PRResult, err error) {
	tl, th := textDigest(title)
	bl, bh := textDigest(body)
	defer func() {
		o.audit(OpPROpen, err, "caller", caller, "repo", repo, "branch", branch, "number", res.Number,
			"existing", res.Existing, "title_len", tl, "title_sha256", th, "body_len", bl, "body_sha256", bh)
	}()
	if err := ValidateRepo(repo, o.Repos); err != nil {
		return PRResult{}, err
	}
	if err := ValidateBranch(branch, o.Prefix); err != nil {
		return PRResult{}, err
	}
	if err := ValidateTitle(title); err != nil {
		return PRResult{}, err
	}
	if err := ValidateText("body", body, o.TextMax); err != nil {
		return PRResult{}, err
	}
	ctx, cancel, tok, err := o.begin(ctx, OpPROpen, repo)
	defer cancel()
	if err != nil {
		return PRResult{}, err
	}
	info, err := o.repoInfo(ctx, tok, repo)
	if err != nil {
		return PRResult{}, err
	}
	if branch == info.DefaultBranch {
		return PRResult{}, fmt.Errorf("branch %q is the default branch", branch)
	}
	if _, found, err := o.headSHA(ctx, tok, repo, branch); err != nil {
		return PRResult{}, err
	} else if !found {
		return PRResult{}, fmt.Errorf("branch %q does not exist on GitHub; push it first", branch)
	}
	if existing, found, err := o.openPRFor(ctx, tok, repo, branch); err != nil || found {
		return existing, err
	}
	var created ghPull
	in := map[string]any{"title": title, "body": body, "head": branch, "base": info.DefaultBranch, "maintainer_can_modify": false}
	if err := o.API.do(ctx, tok, http.MethodPost, "/repos/"+repo+"/pulls", in, &created); err != nil {
		// Another call may have opened it meanwhile (GitHub: 422 "A pull
		// request already exists").
		if isStatus(err, http.StatusUnprocessableEntity) {
			if existing, found, lerr := o.openPRFor(ctx, tok, repo, branch); lerr == nil && found {
				return existing, nil
			}
		}
		return PRResult{}, fmt.Errorf("open PR: %w", err)
	}
	if created.Number <= 0 {
		return PRResult{}, fmt.Errorf("open PR: unexpected answer")
	}
	return PRResult{OK: true, Repo: repo, Number: created.Number, URL: created.HTMLURL, Head: branch, Base: info.DefaultBranch}, nil
}

// openPRFor returns the open PR whose head is branch of repo itself, if any.
// Its base is whatever that PR has (Base says which).
func (o *Ops) openPRFor(ctx context.Context, tok, repo, branch string) (PRResult, bool, error) {
	owner := repo[:strings.IndexByte(repo, '/')]
	q := url.Values{"state": {"open"}, "head": {owner + ":" + branch}, "per_page": {"10"}}
	var open []ghPull
	if err := o.API.do(ctx, tok, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil, &open); err != nil {
		return PRResult{}, false, fmt.Errorf("list open PRs: %w", err)
	}
	for _, p := range open {
		if p.Head.Ref == branch && p.State == "open" {
			return PRResult{OK: true, Existing: true, Repo: repo, Number: p.Number, URL: p.HTMLURL, Head: branch, Base: p.Base.Ref}, true, nil
		}
	}
	return PRResult{}, false, nil
}

func (o *Ops) pull(ctx context.Context, tok, repo, number string) (ghPull, error) {
	var p ghPull
	err := o.API.do(ctx, tok, http.MethodGet, "/repos/"+repo+"/pulls/"+number, nil, &p)
	if isStatus(err, http.StatusNotFound) {
		return p, fmt.Errorf("PR #%s not found in %s", number, repo)
	}
	if err != nil {
		return p, fmt.Errorf("read PR #%s: %w", number, err)
	}
	if strconv.Itoa(p.Number) != number || !shaRE.MatchString(p.Head.SHA) {
		return p, fmt.Errorf("read PR #%s: unexpected answer", number)
	}
	return p, nil
}

// checks reads the check runs (latest per name) and the commit statuses
// (latest per context) on sha, one bounded page of each. complete is false
// when GitHub has more than that page.
func (o *Ops) checks(ctx context.Context, tok, repo, sha string) (cs []Check, combined string, total int, complete bool, err error) {
	var runs ghCheckRuns
	q := url.Values{"filter": {"latest"}, "per_page": {strconv.Itoa(maxCheckRuns)}}
	if err := o.API.do(ctx, tok, http.MethodGet, "/repos/"+repo+"/commits/"+sha+"/check-runs?"+q.Encode(), nil, &runs); err != nil {
		return nil, "", 0, false, fmt.Errorf("read check runs: %w", err)
	}
	var sts ghStatuses
	q = url.Values{"per_page": {strconv.Itoa(maxCheckRuns)}}
	if err := o.API.do(ctx, tok, http.MethodGet, "/repos/"+repo+"/commits/"+sha+"/status?"+q.Encode(), nil, &sts); err != nil {
		return nil, "", 0, false, fmt.Errorf("read commit statuses: %w", err)
	}
	if len(runs.Runs) > maxCheckRuns || len(sts.Statuses) > maxCheckRuns {
		return nil, "", 0, false, fmt.Errorf("read checks: more entries than requested")
	}
	for _, r := range runs.Runs {
		cs = append(cs, Check{Name: r.Name, Kind: "check_run", Status: r.Status, Conclusion: r.Conclusion})
	}
	for _, s := range sts.Statuses {
		cs = append(cs, Check{Name: s.Context, Kind: "status", Status: "completed", Conclusion: s.State})
		if s.State == "pending" {
			cs[len(cs)-1].Status, cs[len(cs)-1].Conclusion = "pending", ""
		}
	}
	total = max(runs.Total, len(runs.Runs)) + max(sts.Total, len(sts.Statuses))
	return cs, sts.State, total, total == len(cs), nil
}

// PRStatus reports a PR's state, mergeability and the checks on its head.
func (o *Ops) PRStatus(ctx context.Context, caller, repo, number string) (res PRStatusResult, err error) {
	defer func() {
		o.audit(OpPRStatus, err, "caller", caller, "repo", repo, "number", number)
	}()
	if err := ValidateNumber(number); err != nil {
		return res, err
	}
	ctx, cancel, tok, err := o.begin(ctx, OpPRStatus, repo)
	defer cancel()
	if err != nil {
		return res, err
	}
	p, err := o.pull(ctx, tok, repo, number)
	if err != nil {
		return res, err
	}
	res = PRStatusResult{OK: true, Repo: repo, Number: p.Number, URL: p.HTMLURL, State: p.State, Merged: p.Merged,
		Draft: p.Draft, Mergeable: p.Mergeable, MergeableState: p.MergeableState,
		Head: p.Head.Ref, HeadSHA: p.Head.SHA, Base: p.Base.Ref}
	cs, combined, total, complete, err := o.checks(ctx, tok, repo, p.Head.SHA)
	if err != nil {
		return PRStatusResult{}, err
	}
	res.Checks, res.CombinedStatus, res.ChecksTotal, res.Truncated = cs, combined, total, !complete
	if res.Checks == nil {
		res.Checks = []Check{}
	}
	return res, nil
}

// checkGate is pr_merge's check rule: every check on the head completed
// with success, neutral or skipped (a status: success), the policy's names
// are present and succeeded, and (unless the policy is None) at least one
// check exists.
func checkGate(cs []Check, complete bool, pol CheckPolicy) error {
	if !pol.set() {
		return fmt.Errorf("no merge check policy configured (-merge-checks); refusing to merge")
	}
	if !complete {
		return fmt.Errorf("more than %d check runs or statuses on the head; cannot verify them all", maxCheckRuns)
	}
	var bad []string
	succeeded := map[string]bool{}
	for _, c := range cs {
		ok := false
		switch {
		case c.Kind == "status":
			ok = c.Status == "completed" && c.Conclusion == "success"
		case c.Status != "completed":
		default:
			ok = c.Conclusion == "success" || c.Conclusion == "neutral" || c.Conclusion == "skipped"
		}
		if !ok {
			state := c.Conclusion
			if c.Status != "completed" {
				state = c.Status
			}
			bad = append(bad, fmt.Sprintf("%s (%s)", checkLabel(c.Name), checkLabel(state)))
			continue
		}
		if c.Conclusion == "success" {
			succeeded[c.Name] = true
		}
	}
	if len(bad) > 0 {
		if len(bad) > 10 {
			bad = append(bad[:10], fmt.Sprintf("and %d more", len(bad)-10))
		}
		return fmt.Errorf("checks on the head are not all green: %s; wait for them or fix the branch", strings.Join(bad, ", "))
	}
	// Skipped or neutral checks alone are not a check: at least one must
	// have succeeded.
	if len(succeeded) == 0 && !pol.None {
		return fmt.Errorf("no successful check on the head; refusing to merge unchecked code")
	}
	for _, n := range pol.Names {
		if !succeeded[n] {
			return fmt.Errorf("required check %q is missing or did not succeed on the head", n)
		}
	}
	return nil
}

// checkLabel is a check's name as it may appear in an error: whoever can
// create a check chooses its name.
func checkLabel(name string) string {
	name = strings.Map(dropControl, name)
	if r := []rune(name); len(r) > 100 {
		name = string(r[:100]) + "…"
	}
	return name
}

// PRMerge merges an open PR from a prefix branch of the same repository into
// the default branch, once its checks passed, pinned to the checked head SHA.
func (o *Ops) PRMerge(ctx context.Context, caller, repo, number, method string) (res MergeResult, err error) {
	defer func() {
		o.audit(OpPRMerge, err, "caller", caller, "repo", repo, "number", number, "method", method,
			"head", res.HeadSHA, "merge", res.MergeSHA)
	}()
	if err := ValidateNumber(number); err != nil {
		return res, err
	}
	if method == "" && len(o.MergeMethods) > 0 {
		method = o.MergeMethods[0]
	}
	if !slices.Contains(o.MergeMethods, method) {
		return res, fmt.Errorf("merge method %q not allowed; allowed: %s", method, strings.Join(o.MergeMethods, ", "))
	}
	ctx, cancel, tok, err := o.begin(ctx, OpPRMerge, repo)
	defer cancel()
	if err != nil {
		return res, err
	}
	info, err := o.repoInfo(ctx, tok, repo)
	if err != nil {
		return res, err
	}
	p, err := o.pull(ctx, tok, repo, number)
	if err != nil {
		return res, err
	}
	switch {
	case p.Merged:
		return res, fmt.Errorf("PR #%s is already merged", number)
	case p.State != "open":
		return res, fmt.Errorf("PR #%s is %s, not open", number, p.State)
	case p.Draft:
		return res, fmt.Errorf("PR #%s is a draft", number)
	case p.Head.Repo == nil || !strings.EqualFold(p.Head.Repo.FullName, repo):
		return res, fmt.Errorf("PR #%s comes from another repository (a fork); refused", number)
	case p.Base.Repo == nil || !strings.EqualFold(p.Base.Repo.FullName, repo):
		return res, fmt.Errorf("PR #%s targets another repository; refused", number)
	case p.Base.Ref != info.DefaultBranch:
		return res, fmt.Errorf("PR #%s targets %q, not the default branch %q", number, p.Base.Ref, info.DefaultBranch)
	}
	if err := ValidateBranch(p.Head.Ref, o.Prefix); err != nil {
		return res, fmt.Errorf("PR #%s head: %w", number, err)
	}
	if p.Mergeable == nil {
		return res, fmt.Errorf("PR #%s: GitHub is still computing mergeability; retry shortly", number)
	}
	if !*p.Mergeable {
		return res, fmt.Errorf("PR #%s is not mergeable (%s)", number, p.MergeableState)
	}
	res = MergeResult{Repo: repo, Number: p.Number, Method: method, HeadSHA: p.Head.SHA}
	cs, _, _, complete, err := o.checks(ctx, tok, repo, p.Head.SHA)
	if err != nil {
		return res, err
	}
	res.Checks = cs
	if err := checkGate(cs, complete, o.MergeChecks); err != nil {
		return res, err
	}
	// sha pins the merge to the checked head: GitHub refuses (409) when the
	// branch moved since. No retry: a moved head needs a fresh check.
	var out struct {
		Merged bool   `json:"merged"`
		SHA    string `json:"sha"`
	}
	in := map[string]string{"merge_method": method, "sha": p.Head.SHA}
	if err := o.API.do(ctx, tok, http.MethodPut, "/repos/"+repo+"/pulls/"+number+"/merge", in, &out); err != nil {
		if isStatus(err, http.StatusConflict) {
			return res, fmt.Errorf("PR #%s head moved after the check; not merged (check again): %w", number, err)
		}
		return res, fmt.Errorf("merge PR #%s: %w", number, err)
	}
	if !out.Merged {
		return res, fmt.Errorf("merge PR #%s: GitHub did not merge it", number)
	}
	res.OK, res.MergeSHA = true, out.SHA
	return res, nil
}

func (o *Ops) lockRelease(ctx context.Context) (func(), error) {
	o.relOnce.Do(func() { o.relLock = make(chan struct{}, 1) })
	select {
	case o.relLock <- struct{}{}:
		return func() { <-o.relLock }, nil
	case <-ctx.Done():
		return nil, errors.New("another release is in progress; retry shortly")
	}
}

// Release creates tag on the default branch's current head, and a GitHub
// release for it with notes. The tag must match ReleaseTag, not exist, and
// be greater than every existing tag that matches.
func (o *Ops) Release(ctx context.Context, caller, repo, tag, notes string) (res ReleaseResult, err error) {
	nl, nh := textDigest(notes)
	defer func() {
		o.audit(OpRelease, err, "caller", caller, "repo", repo, "tag", tag, "target", res.Target,
			"notes_len", nl, "notes_sha256", nh)
	}()
	if err := ValidateTagName(tag); err != nil {
		return res, err
	}
	if !o.ReleaseTag.MatchString(tag) {
		return res, fmt.Errorf("tag %q does not match the release pattern %s", tag, o.ReleaseTag)
	}
	want, err := parseSemver(tag)
	if err != nil {
		return res, fmt.Errorf("tag: %w", err)
	}
	if err := ValidateText("notes", notes, o.TextMax); err != nil {
		return res, err
	}
	ctx, cancel, tok, err := o.begin(ctx, OpRelease, repo)
	defer cancel()
	if err != nil {
		return res, err
	}
	unlock, err := o.lockRelease(ctx)
	if err != nil {
		return res, err
	}
	defer unlock()
	for page := 1; ; page++ {
		if page > maxTagPages {
			return res, fmt.Errorf("more than %d tags; cannot compare them all", maxTagPages*100)
		}
		var tags []struct {
			Name string `json:"name"`
		}
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := o.API.do(ctx, tok, http.MethodGet, "/repos/"+repo+"/tags?"+q.Encode(), nil, &tags); err != nil {
			return res, fmt.Errorf("list tags: %w", err)
		}
		for _, t := range tags {
			if t.Name == tag {
				return res, fmt.Errorf("tag %q already exists", tag)
			}
			if !o.ReleaseTag.MatchString(t.Name) {
				continue
			}
			have, err := parseSemver(t.Name)
			if err != nil {
				return res, fmt.Errorf("existing tag %q matches the release pattern but is not a semantic version; cannot order it", t.Name)
			}
			if compareSemver(want, have) <= 0 {
				return res, fmt.Errorf("tag %q is not greater than the existing tag %q", tag, t.Name)
			}
		}
		if len(tags) < 100 {
			break
		}
	}
	var ref ghRef
	err = o.API.do(ctx, tok, http.MethodGet, "/repos/"+repo+"/git/ref/tags/"+url.PathEscape(tag), nil, &ref)
	if err == nil {
		return res, fmt.Errorf("tag %q already exists", tag)
	}
	if !isStatus(err, http.StatusNotFound) {
		return res, fmt.Errorf("read tag: %w", err)
	}
	info, err := o.repoInfo(ctx, tok, repo)
	if err != nil {
		return res, err
	}
	sha, found, err := o.headSHA(ctx, tok, repo, info.DefaultBranch)
	if err != nil {
		return res, err
	}
	if !found {
		return res, fmt.Errorf("default branch %q not found", info.DefaultBranch)
	}
	res = ReleaseResult{Repo: repo, Tag: tag, Target: sha}
	// The tag ref first, at the SHA just read: GitHub refuses (422) a tag
	// that appeared since the check, where a release would silently adopt
	// it, and the tag lands on exactly that commit even if the branch moves.
	var created ghRef
	if err := o.API.do(ctx, tok, http.MethodPost, "/repos/"+repo+"/git/refs",
		map[string]string{"ref": "refs/tags/" + tag, "sha": sha}, &created); err != nil {
		if isStatus(err, http.StatusUnprocessableEntity) {
			return res, fmt.Errorf("create tag %s: refused (it may exist now, or a tag ruleset blocks the App): %w", tag, err)
		}
		return res, fmt.Errorf("create tag %s: %w", tag, err)
	}
	if created.Ref != "refs/tags/"+tag || created.Object.SHA != sha {
		return res, fmt.Errorf("create tag %s: unexpected answer", tag)
	}
	var out struct {
		HTMLURL string `json:"html_url"`
		TagName string `json:"tag_name"`
	}
	in := map[string]any{"tag_name": tag, "name": tag, "body": notes, "draft": false, "prerelease": false}
	if err := o.API.do(ctx, tok, http.MethodPost, "/repos/"+repo+"/releases", in, &out); err != nil {
		return res, fmt.Errorf("tag %s created at %s, but create release: %w", tag, sha, err)
	}
	if out.TagName != tag {
		return res, fmt.Errorf("create release: unexpected answer")
	}
	res.OK, res.URL = true, out.HTMLURL
	return res, nil
}
