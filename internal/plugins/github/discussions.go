package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/programmism/brainiac/internal/plugins"
)

// GitHub Discussions live only in the GraphQL API (#381), so — unlike issues/PRs —
// they need a POST query with cursor pagination rather than a REST walk. Opt-in via
// WithDiscussions; blind-implemented and unit-tested against a fake GraphQL endpoint.

const discussionsQuery = `query($owner:String!,$name:String!,$after:String){
  repository(owner:$owner,name:$name){
    discussions(first:100,after:$after){
      pageInfo{hasNextPage endCursor}
      nodes{number title body url updatedAt author{login}}
    }
  }
}`

type discussion struct {
	Number    int                    `json:"number"`
	Title     string                 `json:"title"`
	Body      string                 `json:"body"`
	URL       string                 `json:"url"`
	UpdatedAt string                 `json:"updatedAt"`
	Author    struct{ Login string } `json:"author"`
}

type graphqlResponse struct {
	Data struct {
		Repository struct {
			Discussions struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []discussion `json:"nodes"`
			} `json:"discussions"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// fetchDiscussions yields one RawDoc per discussion in repo, paginating via GraphQL
// cursors. Returns false only when yield signalled stop (so Fetch returns too); a
// query error is yielded as a non-fatal doc error and ends this repo's discussions.
func (c *Connector) fetchDiscussions(ctx context.Context, repo string, yield func(plugins.RawDoc, error) bool) bool {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return yield(plugins.RawDoc{}, fmt.Errorf("github discussions: %q is not owner/repo", repo))
	}
	after := "" // GraphQL start cursor
	for {
		if ctx.Err() != nil {
			return false
		}
		page, err := c.discussionsPage(ctx, owner, name, after)
		if err != nil {
			return yield(plugins.RawDoc{}, err) // non-fatal (#241): stop this repo's discussions
		}
		for _, d := range page.Data.Repository.Discussions.Nodes {
			doc, ok := discussionDoc(repo, d)
			if !ok {
				continue
			}
			if !yield(doc, nil) {
				return false
			}
		}
		pi := page.Data.Repository.Discussions.PageInfo
		if !pi.HasNextPage || pi.EndCursor == "" {
			return true
		}
		after = pi.EndCursor
	}
}

func (c *Connector) discussionsPage(ctx context.Context, owner, name, after string) (*graphqlResponse, error) {
	vars := map[string]any{"owner": owner, "name": name}
	if after != "" {
		vars["after"] = after
	}
	payload, err := json.Marshal(map[string]any{"query": discussionsQuery, "variables": vars})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/graphql", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github discussions request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("github %s discussions: status %d: %s", owner+"/"+name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out graphqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode github graphql response: %w", err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("github %s discussions: graphql error: %s", owner+"/"+name, out.Errors[0].Message)
	}
	return &out, nil
}

// discussionDoc converts a discussion to a RawDoc, or ok=false to skip an empty one.
func discussionDoc(repo string, d discussion) (plugins.RawDoc, bool) {
	title := strings.TrimSpace(d.Title)
	body := strings.TrimSpace(d.Body)
	if title == "" && body == "" {
		return plugins.RawDoc{}, false
	}
	text := title
	if body != "" {
		text = strings.TrimSpace(title + "\n\n" + body)
	}
	uri := d.URL
	if uri == "" {
		uri = fmt.Sprintf("github://%s/discussion/%d", repo, d.Number)
	}
	return plugins.RawDoc{
		Text:          text,
		SourceURI:     uri,
		SourceLocator: map[string]any{"repo": repo, "number": d.Number, "kind": "discussion"},
		Metadata:      map[string]any{"source": "github", "kind": "discussion", "author": d.Author.Login, "repo": repo},
		ModifiedAt:    parseTime(d.UpdatedAt),
	}, true
}
