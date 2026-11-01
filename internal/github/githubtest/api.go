package githubtest

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// APIOptions configure a fake API. AppID, InstallationID and AppSlug must
// match the App under test; the rest shape the token reply and default to
// a well-behaved GitHub.
type APIOptions struct {
	AppID          int64
	InstallationID int64
	AppSlug        string
	// BotUserID is the user ID the fake gives the App's bot, the author
	// of every issue it creates.
	BotUserID int64
	// Now is the fake's clock, for JWT checks and token expiry. Tests that
	// move the App's clock give both the same one. Nil is time.Now.
	Now func() time.Time

	// TokenTTL is how long a minted token lasts; zero is an hour.
	TokenTTL time.Duration
	// ExpiresAt, when set, replaces the reply's expires_at verbatim.
	ExpiresAt string
	// ExtraPermissions are added to, or replace levels in, the
	// permissions the reply grants.
	ExtraPermissions map[string]string
	// ExtraRepositoryIDs are added to the repositories the reply names.
	ExtraRepositoryIDs []int64
	// OmitRepositories leaves repositories out of the reply.
	OmitRepositories bool

	// EchoAuthorization puts the request's Authorization header into the
	// body and headers of every injected apiFailure, as a hostile or broken
	// server might.
	EchoAuthorization bool
	// RedirectTo, when set, answers every request with a 307 to it.
	RedirectTo string
}

// Request is one request the fake received.
type Request struct {
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
}

// Authorization returns the request's Authorization header.
func (r Request) Authorization() string { return r.Header.Get("Authorization") }

// StoredComment is a comment as the fake holds it. AppID is the
// performed_via_github_app ID, zero for a comment a person wrote.
type StoredComment struct {
	ID    int64
	Issue int
	Body  string
	AppID int64
}

// StoredIssue is an issue as the fake holds it. UserID is its author.
type StoredIssue struct {
	Number    int
	Title     string
	Body      string
	Labels    []string
	Assignees []string
	Open      bool
	UserID    int64
}

type fakePull struct {
	Number   int
	Head     string
	Base     string
	Login    string
	UserType string
	Draft    bool
}

type apiFailure struct {
	method, prefix string
	status         int
	headers        map[string]string
	body           string
}

// API is a fake GitHub REST API: the installation token exchange, issue
// comments, reactions and pull requests, held in memory. It checks the
// App's JWT against Key's public key and hands out CanaryToken, and only
// that token opens the repository endpoints.
type API struct {
	URL string

	t    testing.TB
	opts APIOptions
	mux  *http.ServeMux

	mu        sync.Mutex
	requests  []Request
	mints     int
	nextID    int64
	comments  map[int64]*StoredComment
	reactions map[string]bool
	pulls     []*fakePull
	failures  []apiFailure
	race      *fakePull
	checkRuns map[string][]fakeCheckRun
	issues    []*StoredIssue
	// graphQLErrors makes the next GraphQL request answer 200 with an
	// errors array carrying this message.
	graphQLErrors string
}

type fakeCheckRun struct {
	ID         int64
	Name       string
	Status     string
	Conclusion string
	AppSlug    string
}

// NewAPI starts a fake API, closed when the test ends.
func NewAPI(t testing.TB, opts APIOptions) *API {
	t.Helper()
	if opts.AppID <= 0 || opts.InstallationID <= 0 || opts.AppSlug == "" {
		t.Fatal("githubtest.NewAPI needs AppID, InstallationID and AppSlug")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.TokenTTL == 0 {
		opts.TokenTTL = time.Hour
	}
	f := &API{
		t: t, opts: opts, nextID: 1000, comments: map[int64]*StoredComment{}, reactions: map[string]bool{},
		checkRuns: map[string][]fakeCheckRun{},
	}
	f.mux = http.NewServeMux()
	f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", f.mintToken)
	f.mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{n}/comments", f.repo(f.createComment))
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/issues/comments/{id}", f.repo(f.getComment))
	// {kind} rather than a literal "comments": the literal would conflict
	// with the pattern above on the path .../issues/comments/comments.
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{n}/{kind}", f.repo(f.listComments))
	f.mux.HandleFunc("PATCH /repos/{owner}/{repo}/issues/comments/{id}", f.repo(f.editComment))
	f.mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{n}/reactions", f.repo(f.addReaction))
	f.mux.HandleFunc("POST /repos/{owner}/{repo}/issues/comments/{id}/reactions", f.repo(f.addReaction))
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", f.repo(f.listPulls))
	f.mux.HandleFunc("POST /repos/{owner}/{repo}/pulls", f.repo(f.createPull))
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{n}", f.repo(f.getPull))
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", f.repo(f.listCheckRuns))
	f.mux.HandleFunc("POST /graphql", f.repo(f.graphQL))
	f.mux.HandleFunc("POST /repos/{owner}/{repo}/issues", f.repo(f.createIssue))
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/issues", f.repo(f.listIssues))
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// serve records the request, then answers with a redirect or an injected
// apiFailure if one applies, and otherwise routes it.
func (f *API) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	f.mu.Lock()
	f.requests = append(f.requests, Request{
		Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone(), Body: body,
	})
	fail, failing := f.takeFailure(r)
	f.mu.Unlock()

	if f.opts.RedirectTo != "" {
		http.Redirect(w, r, f.opts.RedirectTo, http.StatusTemporaryRedirect)
		return
	}
	if failing {
		f.writeFailure(w, r, fail)
		return
	}
	f.mux.ServeHTTP(w, r)
}

// FailNext makes the next request whose method is method (any, if empty)
// and whose path begins with pathPrefix fail with status, headers and
// body. Each call injects one apiFailure.
func (f *API) FailNext(method, pathPrefix string, status int, headers map[string]string, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, apiFailure{method: method, prefix: pathPrefix, status: status, headers: headers, body: body})
}

func (f *API) takeFailure(r *http.Request) (apiFailure, bool) {
	for i, fl := range f.failures {
		if (fl.method == "" || fl.method == r.Method) && strings.HasPrefix(r.URL.Path, fl.prefix) {
			f.failures = append(f.failures[:i], f.failures[i+1:]...)
			return fl, true
		}
	}
	return apiFailure{}, false
}

func (f *API) writeFailure(w http.ResponseWriter, r *http.Request, fl apiFailure) {
	for k, v := range fl.headers {
		w.Header().Set(k, v)
	}
	body := fl.body
	if f.opts.EchoAuthorization {
		auth := r.Header.Get("Authorization")
		w.Header().Set("X-Echo-Authorization", auth)
		w.Header().Set("X-GitHub-Request-Id", auth)
		body += fmt.Sprintf(`{"message":"Bad credentials: %s","documentation_url":"%s"}`, auth, auth)
	}
	w.WriteHeader(fl.status)
	_, _ = io.WriteString(w, body)
}

// Requests returns every request received so far.
func (f *API) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// RequestsTo returns the requests whose path begins with prefix.
func (f *API) RequestsTo(prefix string) []Request {
	var out []Request
	for _, r := range f.Requests() {
		if strings.HasPrefix(r.Path, prefix) {
			out = append(out, r)
		}
	}
	return out
}

// Mints returns how many installation tokens the fake has handed out.
func (f *API) Mints() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints
}

// Comments returns every stored comment, in ID order.
func (f *API) Comments() []StoredComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []StoredComment
	for id := int64(1000); id < f.nextID; id++ {
		if c, ok := f.comments[id]; ok {
			out = append(out, *c)
		}
	}
	return out
}

// SeedComment stores a comment as if appID's App posted it on issue, or a
// person when appID is zero, and returns its ID.
func (f *API) SeedComment(issue int, body string, appID int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.storeComment(issue, body, appID)
}

// SeedPull stores an open pull request from head to base opened by login,
// whose account type is userType ("User" or "Bot"), and returns its number.
func (f *API) SeedPull(head, base, login, userType string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.storePull(&fakePull{Head: head, Base: base, Login: login, UserType: userType})
}

// SeedDraftPull stores an open draft pull request, as SeedPull does, and
// returns its number.
func (f *API) SeedDraftPull(head, base, login, userType string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.storePull(&fakePull{Head: head, Base: base, Login: login, UserType: userType, Draft: true})
}

// PullDraft reports whether pull request number is a draft.
func (f *API) PullDraft(number int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if number < 1 || number > len(f.pulls) {
		f.t.Fatalf("githubtest: no pull request #%d", number)
	}
	return f.pulls[number-1].Draft
}

// SeedCheckRun stores a check run on commit sha, created by the App with
// appSlug, and returns its ID. Runs are listed in the order seeded.
func (f *API) SeedCheckRun(sha, name, status, conclusion, appSlug string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID
	f.nextID++
	f.checkRuns[sha] = append(f.checkRuns[sha], fakeCheckRun{ID: id, Name: name, Status: status, Conclusion: conclusion, AppSlug: appSlug})
	return id
}

// FailNextGraphQL makes the next GraphQL request answer 200 with an
// errors array whose message is msg, as GitHub reports a refused
// mutation.
func (f *API) FailNextGraphQL(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.graphQLErrors = msg
}

// RaceNextPullCreate makes the next pull request create lose a race: the
// fake stores the pull request as opened by login and answers 422, as
// GitHub does when another request opened it first.
func (f *API) RaceNextPullCreate(login, userType string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.race = &fakePull{Login: login, UserType: userType}
}

// Pulls returns how many pull requests the fake holds.
func (f *API) Pulls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pulls)
}

func (f *API) storeComment(issue int, body string, appID int64) int64 {
	id := f.nextID
	f.nextID++
	f.comments[id] = &StoredComment{ID: id, Issue: issue, Body: body, AppID: appID}
	return id
}

func (f *API) storePull(p *fakePull) int {
	p.Number = len(f.pulls) + 1
	f.pulls = append(f.pulls, p)
	return p.Number
}

// mintToken answers the token exchange: a valid JWT gets CanaryToken for
// the repositories and permissions asked for, as the options shape them.
func (f *API) mintToken(w http.ResponseWriter, r *http.Request) {
	if err := f.verifyJWT(r.Header.Get("Authorization")); err != nil {
		writeAPIJSON(w, http.StatusUnauthorized, map[string]string{"message": "A JSON web token could not be decoded"})
		return
	}
	if r.PathValue("id") != strconv.FormatInt(f.opts.InstallationID, 10) {
		writeAPIJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	var req struct {
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid request"})
		return
	}
	f.mu.Lock()
	f.mints++
	f.mu.Unlock()
	writeAPIJSON(w, http.StatusCreated, f.tokenReply(req.RepositoryIDs, req.Permissions))
}

func (f *API) tokenReply(repoIDs []int64, perms map[string]string) map[string]any {
	granted := map[string]string{}
	for k, v := range perms {
		granted[k] = v
	}
	for k, v := range f.opts.ExtraPermissions {
		granted[k] = v
	}
	expires := f.opts.Now().Add(f.opts.TokenTTL).UTC().Format(time.RFC3339)
	if f.opts.ExpiresAt != "" {
		expires = f.opts.ExpiresAt
	}
	reply := map[string]any{
		"token": CanaryToken, "expires_at": expires, "permissions": granted, "repository_selection": "selected",
	}
	if !f.opts.OmitRepositories {
		var repos []map[string]any
		for _, id := range append(append([]int64(nil), repoIDs...), f.opts.ExtraRepositoryIDs...) {
			repos = append(repos, map[string]any{"id": id})
		}
		reply["repositories"] = repos
	}
	return reply
}

// verifyJWT checks the App JWT as GitHub does: RS256 under the App's key,
// issued by the App, already valid, and living at most ten minutes.
func (f *API) verifyJWT(auth string) error {
	raw, ok := strings.CutPrefix(auth, "Bearer ")
	parts := strings.Split(raw, ".")
	if !ok || len(parts) != 3 {
		return errors.New("not a bearer JWT")
	}
	enc := base64.RawURLEncoding
	header, err1 := enc.DecodeString(parts[0])
	payload, err2 := enc.DecodeString(parts[1])
	sig, err3 := enc.DecodeString(parts[2])
	if err := errors.Join(err1, err2, err3); err != nil {
		return err
	}
	if string(header) != `{"alg":"RS256","typ":"JWT"}` {
		return errors.New("header is not RS256")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&Key(f.t).PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		return err
	}
	var claims struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return err
	}
	now := f.opts.Now().Unix()
	if claims.ISS != strconv.FormatInt(f.opts.AppID, 10) || claims.IAT > now || claims.EXP <= now || claims.EXP-claims.IAT > 600 {
		return errors.New("claims out of bounds")
	}
	return nil
}

// repo wraps a repository endpoint so only the installation token opens
// it.
func (f *API) repo(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+CanaryToken {
			writeAPIJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		h(w, r)
	}
}

func (f *API) createComment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body string `json:"body"`
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || json.NewDecoder(r.Body).Decode(&in) != nil {
		writeAPIJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid request"})
		return
	}
	f.mu.Lock()
	id := f.storeComment(n, in.Body, f.opts.AppID)
	c := *f.comments[id]
	f.mu.Unlock()
	writeAPIJSON(w, http.StatusCreated, f.commentJSON(r, c))
}

// listComments answers GitHub's issue comment list: every comment on the
// issue, whoever wrote it, oldest first, paged by per_page (default 30,
// at most 100) and page (from 1), as GitHub pages it.
func (f *API) listComments(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || r.PathValue("kind") != "comments" {
		writeAPIJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	perPage := queryInt(r, "per_page", 30)
	page := queryInt(r, "page", 1)
	if perPage < 1 || perPage > 100 || page < 1 {
		writeAPIJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid request"})
		return
	}
	var onIssue []StoredComment
	for _, c := range f.Comments() {
		if c.Issue == n {
			onIssue = append(onIssue, c)
		}
	}
	out := []map[string]any{}
	for i := (page - 1) * perPage; i < len(onIssue) && i < page*perPage; i++ {
		out = append(out, f.commentJSON(r, onIssue[i]))
	}
	writeAPIJSON(w, http.StatusOK, out)
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	return n
}

func (f *API) getComment(w http.ResponseWriter, r *http.Request) {
	c, ok := f.lookupComment(r)
	if !ok {
		writeAPIJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeAPIJSON(w, http.StatusOK, f.commentJSON(r, c))
}

func (f *API) editComment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body string `json:"body"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeAPIJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid request"})
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f.mu.Lock()
	c, ok := f.comments[id]
	var got StoredComment
	if ok {
		c.Body = in.Body
		got = *c
	}
	f.mu.Unlock()
	if !ok {
		writeAPIJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeAPIJSON(w, http.StatusOK, f.commentJSON(r, got))
}

func (f *API) lookupComment(r *http.Request) (StoredComment, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return StoredComment{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.comments[id]
	if !ok {
		return StoredComment{}, false
	}
	return *c, true
}

func (f *API) commentJSON(r *http.Request, c StoredComment) map[string]any {
	out := map[string]any{
		"id":       c.ID,
		"body":     c.Body,
		"html_url": fmt.Sprintf("https://github.example/%s/%s/issues/%d#issuecomment-%d", r.PathValue("owner"), r.PathValue("repo"), c.Issue, c.ID),
	}
	if c.AppID != 0 {
		out["performed_via_github_app"] = map[string]any{"id": c.AppID}
	}
	return out
}

// addReaction answers 201 for a new reaction and 200 for one already
// there, as GitHub does.
func (f *API) addReaction(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content string `json:"content"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeAPIJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid request"})
		return
	}
	key := r.URL.Path + " " + in.Content
	f.mu.Lock()
	seen := f.reactions[key]
	f.reactions[key] = true
	f.mu.Unlock()
	status := http.StatusCreated
	if seen {
		status = http.StatusOK
	}
	writeAPIJSON(w, status, map[string]any{"id": 1, "content": in.Content})
}

func (f *API) listPulls(w http.ResponseWriter, r *http.Request) {
	owner, head, _ := strings.Cut(r.URL.Query().Get("head"), ":")
	if owner != r.PathValue("owner") || r.URL.Query().Get("state") != "open" {
		writeAPIJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid head or state"})
		return
	}
	f.mu.Lock()
	out := []map[string]any{}
	for _, p := range f.pulls {
		if p.Head == head {
			out = append(out, pullJSON(p))
		}
	}
	f.mu.Unlock()
	writeAPIJSON(w, http.StatusOK, out)
}

// createPull opens a pull request, or answers 422 when one is already open
// from the head, as GitHub does. A pending race stores one first.
func (f *API) createPull(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title, Head, Base, Body string
		Draft                   bool
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Title == "" {
		writeAPIJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid request"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.race != nil {
		f.race.Head, f.race.Base = in.Head, in.Base
		f.storePull(f.race)
		f.race = nil
	}
	for _, p := range f.pulls {
		if p.Head == in.Head && p.Base == in.Base {
			writeAPIJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "A pull request already exists"})
			return
		}
	}
	p := &fakePull{Head: in.Head, Base: in.Base, Login: f.opts.AppSlug + "[bot]", UserType: "Bot", Draft: in.Draft}
	f.storePull(p)
	writeAPIJSON(w, http.StatusCreated, pullJSON(p))
}

func pullJSON(p *fakePull) map[string]any {
	return map[string]any{
		"number":   p.Number,
		"node_id":  "PR_node_" + strconv.Itoa(p.Number),
		"html_url": "https://github.example/pull/" + strconv.Itoa(p.Number),
		"draft":    p.Draft,
		"user":     map[string]any{"login": p.Login, "type": p.UserType},
		"head":     map[string]any{"ref": p.Head},
		"base":     map[string]any{"ref": p.Base},
	}
}

func writeAPIJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// getPull answers one pull request by number.
func (f *API) getPull(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil || n < 1 || n > len(f.pulls) {
		writeAPIJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeAPIJSON(w, http.StatusOK, pullJSON(f.pulls[n-1]))
}

// listCheckRuns answers the check runs on a commit, paginated as GitHub
// does with per_page and page, and total_count over every page.
func (f *API) listCheckRuns(w http.ResponseWriter, r *http.Request) {
	perPage, page := queryInt(r, "per_page", 30), queryInt(r, "page", 1)
	f.mu.Lock()
	all := f.checkRuns[r.PathValue("sha")]
	f.mu.Unlock()
	runs := []map[string]any{}
	for i := (page - 1) * perPage; i >= 0 && i < len(all) && i < page*perPage; i++ {
		c := all[i]
		run := map[string]any{"id": c.ID, "name": c.Name, "status": c.Status, "app": map[string]any{"slug": c.AppSlug}}
		if c.Conclusion != "" {
			run["conclusion"] = c.Conclusion
		} else {
			run["conclusion"] = nil
		}
		runs = append(runs, run)
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"total_count": len(all), "check_runs": runs})
}

// graphQL answers markPullRequestReadyForReview, the only mutation the
// client sends: it takes the pull request with the given node ID out of
// draft. An unknown node or an injected failure answers 200 with errors,
// as GitHub does.
func (f *API) graphQL(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query     string            `json:"query"`
		Variables map[string]string `json:"variables"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || !strings.Contains(in.Query, "markPullRequestReadyForReview") {
		writeAPIJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if msg := f.graphQLErrors; msg != "" {
		f.graphQLErrors = ""
		writeAPIJSON(w, http.StatusOK, map[string]any{"data": nil, "errors": []map[string]string{{"message": msg}}})
		return
	}
	for _, p := range f.pulls {
		if "PR_node_"+strconv.Itoa(p.Number) == in.Variables["id"] {
			p.Draft = false
			writeAPIJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"pullRequest": map[string]any{"isDraft": false}}},
			})
			return
		}
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"data": nil, "errors": []map[string]string{{"message": "Could not resolve to a node with the global id of '" + in.Variables["id"] + "'"}},
	})
}
