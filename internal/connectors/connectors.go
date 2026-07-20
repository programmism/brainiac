// Package connectors builds a plugins.SourceConnector for a configured source
// type — the one place the cmd adapters (CLI import, MCP ingest, the HTTP
// background sync) share so connector construction isn't duplicated (#428). OAuth
// access tokens are resolved (and auto-refreshed) via the core (#246); the OCR
// fallback is passed in by the caller so the shell-out stays in the cmd layer.
package connectors

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/config"
	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/doctext"
	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/plugins/confluence"
	"github.com/programmism/brainiac/internal/plugins/gdrive"
	"github.com/programmism/brainiac/internal/plugins/github"
	"github.com/programmism/brainiac/internal/plugins/gitlab"
	"github.com/programmism/brainiac/internal/plugins/gmail"
	"github.com/programmism/brainiac/internal/plugins/jira"
	"github.com/programmism/brainiac/internal/plugins/linear"
	"github.com/programmism/brainiac/internal/plugins/markdown"
	"github.com/programmism/brainiac/internal/plugins/notion"
	"github.com/programmism/brainiac/internal/plugins/slack"
)

// Build constructs the connector for a source type. path is an optional targeted
// import (a page/channel id, an owner/repo, or a markdown dir); ocr is the optional
// OCR fallback for the markdown connector (nil = off). Returns an error when the
// source isn't configured.
func Build(ctx context.Context, kb *core.Core, cfg *config.Config, source, path string, ocr doctext.OCRFunc) (plugins.SourceConnector, error) {
	switch source {
	case "notion":
		sc := cfg.Source("notion")
		if sc == nil || sc.Token == "" {
			return nil, fmt.Errorf("notion source not configured (set NOTION_TOKEN or config.yaml)")
		}
		if path != "" {
			return notion.NewForPages(sc.Token, []string{path}), nil
		}
		return notion.New(sc.Token), nil
	case "slack":
		sc := cfg.Source("slack")
		if sc == nil || sc.Token == "" {
			return nil, fmt.Errorf("slack source not configured (set SLACK_TOKEN or config.yaml)")
		}
		if path != "" {
			return slack.NewForChannels(sc.Token, []string{path}), nil
		}
		return slack.New(sc.Token), nil
	case "github":
		sc := cfg.Source("github")
		if sc == nil || sc.Token == "" {
			return nil, fmt.Errorf("github source not configured (set GITHUB_TOKEN or config.yaml)")
		}
		repos := sc.Repos
		if path != "" {
			repos = []string{path}
		}
		if len(repos) == 0 {
			return nil, fmt.Errorf("github needs a repo: a target owner/repo, or sources[].repos / GITHUB_REPOS")
		}
		ghOpts := []github.Option{github.WithFiles(sc.Files)}
		if sc.Discussions {
			ghOpts = append(ghOpts, github.WithDiscussions())
		}
		return github.New(sc.Token, repos, ghOpts...), nil
	case "linear":
		sc := cfg.Source("linear")
		if sc == nil || sc.Token == "" {
			return nil, fmt.Errorf("linear source not configured (set an API key via LINEAR_TOKEN)")
		}
		return linear.New(sc.Token), nil
	case "gitlab":
		sc := cfg.Source("gitlab")
		if sc == nil || sc.Token == "" {
			return nil, fmt.Errorf("gitlab source not configured (set GITLAB_TOKEN)")
		}
		projects := sc.Repos
		if path != "" {
			projects = []string{path}
		}
		if len(projects) == 0 {
			return nil, fmt.Errorf("gitlab needs a project: a target group/project, or sources[].repos / GITLAB_PROJECTS")
		}
		return gitlab.New(sc.Token, sc.BaseURL, projects), nil
	case "jira":
		sc := cfg.Source("jira")
		if sc == nil || sc.Token == "" || sc.BaseURL == "" || sc.Email == "" {
			return nil, fmt.Errorf("jira source not configured (set JIRA_BASE_URL, JIRA_EMAIL, JIRA_TOKEN)")
		}
		return jira.New(sc.BaseURL, sc.Email, sc.Token), nil
	case "confluence":
		sc := cfg.Source("confluence")
		if sc == nil || sc.Token == "" || sc.BaseURL == "" || sc.Email == "" {
			return nil, fmt.Errorf("confluence source not configured (set CONFLUENCE_BASE_URL, CONFLUENCE_EMAIL, CONFLUENCE_TOKEN)")
		}
		return confluence.New(sc.BaseURL, sc.Email, sc.Token), nil
	case "gdrive":
		tok, err := oauthToken(ctx, kb, cfg, "gdrive")
		if err != nil {
			return nil, err
		}
		if tok == "" {
			return nil, fmt.Errorf("gdrive not configured (set GDRIVE_TOKEN or `kb oauth set --source gdrive`)")
		}
		return gdrive.New(tok), nil
	case "gmail":
		tok, err := oauthToken(ctx, kb, cfg, "gmail")
		if err != nil {
			return nil, err
		}
		if tok == "" {
			return nil, fmt.Errorf("gmail not configured (set GMAIL_TOKEN or `kb oauth set --source gmail`)")
		}
		q := ""
		if sc := cfg.Source("gmail"); sc != nil {
			q = sc.Query
		}
		return gmail.New(tok, gmail.WithQuery(q)), nil
	case "markdown":
		dir := path
		if dir == "" {
			if sc := cfg.Source("markdown"); sc != nil {
				dir = sc.Path
			}
		}
		if dir == "" {
			return nil, fmt.Errorf("markdown source needs a directory (a target path or sources[].path)")
		}
		return markdown.New(dir, markdown.WithOCR(ocr)), nil
	default:
		return nil, fmt.Errorf("unknown source %q", source)
	}
}

// oauthToken resolves a connector's access token (#246): a stored, auto-refreshed
// OAuth credential if present, else the source's <TYPE>_TOKEN env value.
func oauthToken(ctx context.Context, kb *core.Core, cfg *config.Config, source string) (string, error) {
	env := ""
	if sc := cfg.Source(source); sc != nil {
		env = sc.Token
	}
	return kb.ResolveSourceToken(ctx, source, env)
}
