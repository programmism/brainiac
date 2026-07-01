// Package markdown implements the plugins.SourceConnector seam over a folder of
// Markdown files. It is the deliberate *second* connector (PRD §2.3, §20.4):
// building it against the same interface as the Notion connector validates the
// seam before we call it stable. The interface fit both with no changes.
package markdown

import (
	"context"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/programmism/brainiac/internal/plugins"
)

// Connector reads Markdown files from a root directory (recursively).
type Connector struct {
	root string
}

// New builds a Markdown connector rooted at dir.
func New(dir string) *Connector { return &Connector{root: dir} }

var _ plugins.SourceConnector = (*Connector)(nil)

func isMarkdown(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".md" || ext == ".markdown"
}

// Fetch yields one RawDoc per Markdown file, with a portable `markdown://<rel>`
// source URI.
func (c *Connector) Fetch(ctx context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		if info, err := os.Stat(c.root); err != nil || !info.IsDir() {
			return // missing/empty root — nothing to import (not an error)
		}
		_ = filepath.WalkDir(c.root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				yield(plugins.RawDoc{}, err)
				return err
			}
			if ctx.Err() != nil {
				return fs.SkipAll
			}
			if d.IsDir() || !isMarkdown(d.Name()) {
				return nil
			}
			data, readErr := os.ReadFile(path) //nolint:gosec // operator-provided root
			if readErr != nil {
				if !yield(plugins.RawDoc{}, readErr) {
					return fs.SkipAll
				}
				return nil
			}
			rel, _ := filepath.Rel(c.root, path)
			var modified *time.Time
			if info, statErr := d.Info(); statErr == nil {
				m := info.ModTime()
				modified = &m
			}
			if !yield(plugins.RawDoc{
				Text:          string(data),
				SourceURI:     "markdown://" + filepath.ToSlash(rel),
				SourceLocator: map[string]any{"path": filepath.ToSlash(rel)},
				Metadata:      map[string]any{"source": "markdown"},
				ModifiedAt:    modified,
			}, nil) {
				return fs.SkipAll
			}
			return nil
		})
	}
}

// Watch yields an upsert per Markdown file (content-hash dedup in ingest makes
// re-runs cheap). File-mtime incremental sync is a later refinement.
func (c *Connector) Watch(ctx context.Context) iter.Seq2[plugins.Change, error] {
	return func(yield func(plugins.Change, error) bool) {
		_ = filepath.WalkDir(c.root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				yield(plugins.Change{}, err)
				return err
			}
			if ctx.Err() != nil {
				return fs.SkipAll
			}
			if d.IsDir() || !isMarkdown(d.Name()) {
				return nil
			}
			rel, _ := filepath.Rel(c.root, path)
			if !yield(plugins.Change{SourceURI: "markdown://" + filepath.ToSlash(rel), Kind: plugins.ChangeUpserted}, nil) {
				return fs.SkipAll
			}
			return nil
		})
	}
}
