package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---- GitHub ------------------------------------------------------------------

type github struct{ *httpClient }

type ghUser struct {
	Login string `json:"login"`
}

type ghIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	HTMLURL     string    `json:"html_url"`
	User        ghUser    `json:"user"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	CommentsURL string    `json:"comments_url"`
	Comments    int       `json:"comments"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct {
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
}

type ghComment struct {
	User      ghUser    `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func (g *github) list(ctx context.Context, since time.Time, fn func(Item) (bool, error)) error {
	q := url.Values{"state": {g.cfg.State}, "sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	first := g.cfg.apiBase() + "/repos/" + repoPath(g.cfg.Repo) + "/issues?" + q.Encode()

	return g.pageLinks(ctx, first, func(body []byte) (bool, error) {
		var issues []ghIssue
		if err := json.Unmarshal(body, &issues); err != nil {
			return false, fmt.Errorf("decode github issues; %w", err)
		}
		for _, is := range issues {
			it := Item{
				Kind: "issue", Number: strconv.Itoa(is.Number), Title: is.Title, Body: is.Body, State: is.State,
				Author: is.User.Login, URL: is.HTMLURL, Created: is.CreatedAt, Updated: is.UpdatedAt,
			}
			if is.Comments > 0 {
				it.commentsURL = is.CommentsURL
			}
			if is.PullRequest != nil {
				it.Kind = "pull"
				if is.PullRequest.MergedAt != nil {
					it.State = "merged"
				}
			}
			for _, l := range is.Labels {
				it.Labels = append(it.Labels, l.Name)
			}
			if stop, err := fn(it); err != nil || stop {
				return stop, err
			}
		}
		return false, nil
	})
}

func (g *github) comments(ctx context.Context, it Item) ([]Comment, error) {
	if it.commentsURL == "" {
		return nil, nil
	}
	var out []Comment
	err := g.pageLinks(ctx, it.commentsURL+"?per_page=100", func(body []byte) (bool, error) {
		var cs []ghComment
		if err := json.Unmarshal(body, &cs); err != nil {
			return false, err
		}
		for _, c := range cs {
			out = append(out, Comment{Author: c.User.Login, Body: c.Body, Created: c.CreatedAt})
		}
		return len(out) >= maxCommentsPerItem, nil
	})

	return out, err
}

// ---- Gitea / Forgejo -----------------------------------------------------------

type gitea struct{ *httpClient }

func (g *gitea) list(ctx context.Context, since time.Time, fn func(Item) (bool, error)) error {
	base := g.cfg.apiBase() + "/repos/" + repoPath(g.cfg.Repo) + "/issues"
	for page := 1; page < 10_000; page++ {
		q := url.Values{"state": {g.cfg.State}, "limit": {"50"}, "page": {strconv.Itoa(page)}}
		if !since.IsZero() {
			q.Set("since", since.UTC().Format(time.RFC3339))
		}
		var issues []ghIssue
		if _, err := g.get(ctx, base+"?"+q.Encode(), &issues); err != nil {
			return err
		}
		for _, is := range issues {
			it := Item{
				Kind: "issue", Number: strconv.Itoa(is.Number), Title: is.Title, Body: is.Body, State: is.State,
				Author: is.User.Login, URL: is.HTMLURL, Created: is.CreatedAt, Updated: is.UpdatedAt,
			}
			if is.Comments > 0 {
				it.commentsURL = base + "/" + it.Number + "/comments"
			}
			if is.PullRequest != nil {
				it.Kind = "pull"
			}
			for _, l := range is.Labels {
				it.Labels = append(it.Labels, l.Name)
			}
			if stop, err := fn(it); err != nil || stop {
				return err
			}
		}
		if len(issues) < 50 {
			return nil
		}
	}

	return nil
}

func (g *gitea) comments(ctx context.Context, it Item) ([]Comment, error) {
	if it.commentsURL == "" {
		return nil, nil
	}
	var cs []ghComment
	if _, err := g.get(ctx, it.commentsURL, &cs); err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(cs))
	for _, c := range cs {
		out = append(out, Comment{Author: c.User.Login, Body: c.Body, Created: c.CreatedAt})
	}

	return out, nil
}

// ---- GitLab ------------------------------------------------------------------

type gitlab struct{ *httpClient }

type glItem struct {
	IID         int       `json:"iid"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	State       string    `json:"state"`
	WebURL      string    `json:"web_url"`
	Labels      []string  `json:"labels"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Author      struct {
		Username string `json:"username"`
	} `json:"author"`
	SourceBranch   string `json:"source_branch"`
	UserNotesCount int    `json:"user_notes_count"`
}

func (g *gitlab) project() string { return g.cfg.apiBase() + "/projects/" + url.PathEscape(g.cfg.Repo) }

func (g *gitlab) list(ctx context.Context, since time.Time, fn func(Item) (bool, error)) error {
	state := g.cfg.State
	if state == "open" {
		state = "opened"
	}
	for _, kind := range []string{"issue", "pull"} {
		if kind == "issue" && !on(g.cfg.Issues) || kind == "pull" && !on(g.cfg.Pulls) {
			continue
		}
		path := "/issues"
		if kind == "pull" {
			path = "/merge_requests"
		}
		q := url.Values{"order_by": {"updated_at"}, "sort": {"asc"}, "per_page": {"100"}}
		if state != "all" {
			q.Set("state", state)
		}
		if !since.IsZero() {
			q.Set("updated_after", since.UTC().Format(time.RFC3339))
		}
		stopped := false
		err := g.pageLinks(ctx, g.project()+path+"?"+q.Encode(), func(body []byte) (bool, error) {
			var items []glItem
			if err := json.Unmarshal(body, &items); err != nil {
				return false, fmt.Errorf("decode gitlab %s; %w", path, err)
			}
			for _, x := range items {
				it := Item{
					Kind: kind, Number: strconv.Itoa(x.IID), Title: x.Title, Body: x.Description, State: x.State,
					Author: x.Author.Username, URL: x.WebURL, Labels: x.Labels, Created: x.CreatedAt, Updated: x.UpdatedAt,
					Branch: x.SourceBranch,
				}
				if x.UserNotesCount > 0 {
					it.commentsURL = g.project() + path + "/" + it.Number + "/notes?sort=asc&per_page=100"
				}
				if kind == "pull" {
					it.Number = "!" + it.Number
				}
				stop, err := fn(it)
				if err != nil || stop {
					stopped = stop
					return stop, err
				}
			}
			return false, nil
		})
		if err != nil || stopped {
			return err
		}
	}

	return nil
}

func (g *gitlab) comments(ctx context.Context, it Item) ([]Comment, error) {
	if it.commentsURL == "" {
		return nil, nil
	}
	var out []Comment
	err := g.pageLinks(ctx, it.commentsURL, func(body []byte) (bool, error) {
		var notes []struct {
			Body      string    `json:"body"`
			System    bool      `json:"system"`
			CreatedAt time.Time `json:"created_at"`
			Author    struct {
				Username string `json:"username"`
			} `json:"author"`
		}
		if err := json.Unmarshal(body, &notes); err != nil {
			return false, err
		}
		for _, n := range notes {
			if !n.System {
				out = append(out, Comment{Author: n.Author.Username, Body: n.Body, Created: n.CreatedAt})
			}
		}
		return len(out) >= maxCommentsPerItem, nil
	})

	return out, err
}

// ---- Bitbucket Cloud -----------------------------------------------------------

type bitbucket struct{ *httpClient }

type bbPage struct {
	Next   string            `json:"next"`
	Values []json.RawMessage `json:"values"`
}

type bbItem struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Description string `json:"description"`
	Content     struct {
		Raw string `json:"raw"`
	} `json:"content"`
	Author struct {
		DisplayName string `json:"display_name"`
	} `json:"author"`
	Reporter struct {
		DisplayName string `json:"display_name"`
	} `json:"reporter"`
	CreatedOn time.Time `json:"created_on"`
	UpdatedOn time.Time `json:"updated_on"`
	Links     struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
		Comments struct {
			Href string `json:"href"`
		} `json:"comments"`
	} `json:"links"`
	Source struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"source"`
}

func (b *bitbucket) walk(ctx context.Context, first string, fn func(json.RawMessage) (bool, error)) error {
	next := first
	for page := 0; next != "" && page < 10_000; page++ {
		var p bbPage
		if _, err := b.get(ctx, next, &p); err != nil {
			return err
		}
		for _, v := range p.Values {
			if stop, err := fn(v); err != nil || stop {
				return err
			}
		}
		next = p.Next
	}

	return nil
}

func (b *bitbucket) list(ctx context.Context, since time.Time, fn func(Item) (bool, error)) error {
	base := b.cfg.apiBase() + "/repositories/" + repoPath(b.cfg.Repo)
	filter := func(field string) string {
		var parts []string
		if !since.IsZero() {
			parts = append(parts, fmt.Sprintf(`%s>="%s"`, field, since.UTC().Format(time.RFC3339)))
		}
		return strings.Join(parts, " AND ")
	}
	type kindSpec struct{ kind, path, states string }
	specs := []kindSpec{}
	if on(b.cfg.Pulls) {
		states := "state=OPEN&state=MERGED&state=DECLINED&state=SUPERSEDED"
		switch b.cfg.State {
		case "open":
			states = "state=OPEN"
		case "closed":
			states = "state=MERGED&state=DECLINED&state=SUPERSEDED"
		}
		specs = append(specs, kindSpec{"pull", "/pullrequests", states})
	}
	if on(b.cfg.Issues) {
		specs = append(specs, kindSpec{"issue", "/issues", ""})
	}
	for _, s := range specs {
		q := url.Values{"pagelen": {"50"}, "sort": {"updated_on"}}
		if f := filter("updated_on"); f != "" {
			q.Set("q", f)
		}
		endpoint := base + s.path + "?" + q.Encode()
		if s.states != "" {
			endpoint += "&" + s.states
		}
		stopped := false
		err := b.walk(ctx, endpoint, func(raw json.RawMessage) (bool, error) {
			var x bbItem
			if err := json.Unmarshal(raw, &x); err != nil {
				return false, err
			}
			it := Item{
				Kind: s.kind, Number: strconv.Itoa(x.ID), Title: x.Title, State: strings.ToLower(x.State),
				Body: firstNonEmpty(x.Description, x.Content.Raw), Author: firstNonEmpty(x.Author.DisplayName, x.Reporter.DisplayName),
				URL: x.Links.HTML.Href, Created: x.CreatedOn, Updated: x.UpdatedOn, Branch: x.Source.Branch.Name,
				commentsURL: x.Links.Comments.Href,
			}
			stop, err := fn(it)
			stopped = stop
			return stop, err
		})
		if err != nil {
			// Repositories without the issue tracker answer 404 on /issues.
			if s.kind == "issue" && strings.Contains(err.Error(), "404") {
				continue
			}
			return err
		}
		if stopped {
			return nil
		}
	}

	return nil
}

func (b *bitbucket) comments(ctx context.Context, it Item) ([]Comment, error) {
	if it.commentsURL == "" {
		return nil, nil
	}
	var out []Comment
	err := b.walk(ctx, it.commentsURL+"?pagelen=100", func(raw json.RawMessage) (bool, error) {
		var c struct {
			Content struct {
				Raw string `json:"raw"`
			} `json:"content"`
			User struct {
				DisplayName string `json:"display_name"`
			} `json:"user"`
			CreatedOn time.Time `json:"created_on"`
			Deleted   bool      `json:"deleted"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return false, err
		}
		if !c.Deleted {
			out = append(out, Comment{Author: c.User.DisplayName, Body: c.Content.Raw, Created: c.CreatedOn})
		}
		return len(out) >= maxCommentsPerItem, nil
	})

	return out, err
}
