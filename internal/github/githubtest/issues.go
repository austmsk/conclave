package githubtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

// Issues returns a copy of every issue the fake holds, in creation order.
func (f *API) Issues() []StoredIssue {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]StoredIssue, len(f.issues))
	for i, is := range f.issues {
		out[i] = *is
	}
	return out
}

// SeedIssue adds an open issue by userID, as a person or another App
// might open one, and returns its number.
func (f *API) SeedIssue(title, body string, labels []string, userID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.storeIssue(&StoredIssue{Title: title, Body: body, Labels: labels, Open: true, UserID: userID})
}

// CloseIssue closes issue n, as the owner would after reading an alert.
func (f *API) CloseIssue(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, is := range f.issues {
		if is.Number == n {
			is.Open = false
		}
	}
}

func (f *API) storeIssue(is *StoredIssue) int {
	is.Number = len(f.issues) + 1
	f.issues = append(f.issues, is)
	return is.Number
}

func (f *API) createIssue(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		Labels    []string `json:"labels"`
		Assignees []string `json:"assignees"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Title == "" {
		writeAPIJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid request"})
		return
	}
	f.mu.Lock()
	is := &StoredIssue{Title: in.Title, Body: in.Body, Labels: in.Labels, Assignees: in.Assignees, Open: true, UserID: f.opts.BotUserID}
	f.storeIssue(is)
	got := *is
	f.mu.Unlock()
	writeAPIJSON(w, http.StatusCreated, issueJSON(r, got))
}

// listIssues answers GitHub's issue list for state=open and one label,
// oldest first, paged as listComments pages.
func (f *API) listIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	perPage := queryInt(r, "per_page", 30)
	page := queryInt(r, "page", 1)
	if q.Get("state") != "open" || perPage < 1 || perPage > 100 || page < 1 {
		writeAPIJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid request"})
		return
	}
	var match []StoredIssue
	for _, is := range f.Issues() {
		if is.Open && slices.Contains(is.Labels, q.Get("labels")) {
			match = append(match, is)
		}
	}
	out := []map[string]any{}
	for i := (page - 1) * perPage; i < len(match) && i < page*perPage; i++ {
		out = append(out, issueJSON(r, match[i]))
	}
	writeAPIJSON(w, http.StatusOK, out)
}

func issueJSON(r *http.Request, is StoredIssue) map[string]any {
	return map[string]any{
		"number":   is.Number,
		"html_url": fmt.Sprintf("https://github.example/%s/%s/issues/%d", r.PathValue("owner"), r.PathValue("repo"), is.Number),
		"body":     is.Body,
		"user":     map[string]any{"id": is.UserID},
		"state":    map[bool]string{true: "open", false: "closed"}[is.Open],
	}
}
