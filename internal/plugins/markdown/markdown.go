// Package markdown implements the plugins.SourceConnector seam over a folder of
// local files. It began as the deliberate *second* connector (PRD §2.3, §20.4) —
// building it against the same interface as the Notion connector validated the
// seam. It now ingests any format the doctext extraction layer understands
// (Markdown, plain text, HTML, DOCX — #234), converting each to text before it
// enters the pipeline; the source-URI scheme stays `markdown://` for continuity.
package markdown

import (
	"context"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"time"

	"github.com/programmism/brainiac/internal/doctext"
	"github.com/programmism/brainiac/internal/plugins"
)

// Connector reads Markdown files from a root directory (recursively).
type Connector struct {
	root string
	ocr  doctext.OCRFunc // optional OCR fallback for scanned PDFs (#356); nil = off
}

// Option customizes a Connector.
type Option func(*Connector)

// WithOCR sets the opt-in OCR fallback used for image-only PDFs (#356).
func WithOCR(fn doctext.OCRFunc) Option { return func(c *Connector) { c.ocr = fn } }

// New builds a Markdown connector rooted at dir.
func New(dir string, opts ...Option) *Connector {
	c := &Connector{root: dir}
	for _, o := range opts {
		o(c)
	}
	return c
}

var _ plugins.SourceConnector = (*Connector)(nil)

// isSupported reports whether the file is one the doctext layer can extract (#234).
func isSupported(name string) bool { return doctext.Supported(name) }

// Fetch yields one RawDoc per supported file, its bytes converted to text by the
// doctext layer, with a portable `markdown://<rel>` source URI. An unconvertible
// file (e.g. a corrupt .docx) yields its error and is skipped by ingest.
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
			if d.IsDir() || !isSupported(d.Name()) {
				return nil
			}
			data, readErr := os.ReadFile(path) //nolint:gosec // operator-provided root
			if readErr != nil {
				if !yield(plugins.RawDoc{}, readErr) {
					return fs.SkipAll
				}
				return nil
			}
			text, convErr := doctext.ToTextOCR(d.Name(), data, c.ocr)
			if convErr != nil {
				if !yield(plugins.RawDoc{}, convErr) {
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
				Text:          text,
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
			if d.IsDir() || !isSupported(d.Name()) {
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
