// Package fs implements the "fs" web-source type: a local or mounted
// directory (NFS, SMB, a synced Drive folder, a docs export) whose
// convertible files are indexed incrementally by modification time and size.
//
// Access is limited to the roots an operator allows through the
// KRABBY_SOURCES_FS_ROOTS environment variable (colon-separated); with none
// set the type rejects every path, so a REST/MCP client cannot point Krabby at
// arbitrary server files.
package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/filesrc"
)

// Type is the collection type served by this fetcher.
const Type = "fs"

// RootsEnv names the allowed-roots environment variable.
const RootsEnv = "KRABBY_SOURCES_FS_ROOTS"

// Config is the provider config.
type Config struct {
	filesrc.Common
	Path    string `json:"path"`
	BaseURL string `json:"base_url,omitempty"`
}

// Fetcher syncs a directory tree.
type Fetcher struct {
	websource.Schema
	roots []string
}

// New creates the fetcher. roots are the directories paths must live under;
// when nil they are read from RootsEnv.
func New(roots []string) *Fetcher {
	if roots == nil {
		for r := range strings.SplitSeq(os.Getenv(RootsEnv), string(os.PathListSeparator)) {
			if r = strings.TrimSpace(r); r != "" {
				roots = append(roots, r)
			}
		}
	}
	clean := make([]string, 0, len(roots))
	for _, r := range roots {
		if abs, err := filepath.Abs(r); err == nil {
			if resolved, err := filepath.EvalSymlinks(abs); err == nil {
				abs = resolved
			}
			clean = append(clean, filepath.Clean(abs))
		}
	}

	fields := append([]websource.Field{
		{Key: "path", Label: "Directory path", Kind: websource.FieldString, Required: true, Placeholder: "/data/docs",
			Help: "Must be under a root allowed by " + RootsEnv},
		{Key: "base_url", Label: "Link base URL (optional)", Kind: websource.FieldString, Placeholder: "https://files.example.com/docs/",
			Help: "Prefix joined with the relative path to build a link to each file"},
	}, filesrc.Fields()...)

	return &Fetcher{roots: clean, Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:        Type,
		Label:       "Local / mounted folder",
		Description: "Index Markdown, text, HTML, PDF and Office files of a server-side directory (local disk, NFS/SMB mount, synced folder). Paths are restricted to " + RootsEnv + ".",
		Fields:      fields,
	}}}
}

func decode(raw json.RawMessage) (Config, error) {
	return websource.DecodeConfig[Config](raw, Type)
}

// resolve returns the real absolute directory, refusing anything outside the
// allowed roots (symlinks included).
func (f *Fetcher) resolve(p string) (string, error) {
	if len(f.roots) == 0 {
		return "", fmt.Errorf("fs sources are disabled; set %s to the directories Krabby may read", RootsEnv)
	}
	abs, err := filepath.Abs(strings.TrimSpace(p))
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("fs path %s; %w", p, err)
	}
	for _, root := range f.roots {
		if real == root || strings.HasPrefix(real, root+string(filepath.Separator)) {
			return real, nil
		}
	}

	return "", fmt.Errorf("fs path %s is outside the allowed roots (%s)", p, RootsEnv)
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decode(raw)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Path) == "" {
		return fmt.Errorf("fs path is required")
	}
	if _, err := f.resolve(cfg.Path); err != nil {
		return err
	}
	if cfg.BaseURL != "" {
		if err := websource.ValidHTTPURL(cfg.BaseURL); err != nil {
			return fmt.Errorf("base_url %w", err)
		}
	}

	return nil
}

func (f *Fetcher) MergeConfig(current, update json.RawMessage) (json.RawMessage, error) {
	merged, err := f.MergeObjects(current, update)
	if err != nil {
		return nil, err
	}

	return merged, f.Validate(merged)
}

func (f *Fetcher) ConfigView(raw json.RawMessage) any { return f.View(raw) }

func (f *Fetcher) list(ctx context.Context, cfg Config) (string, []websource.ListedItem, int, error) {
	dir, err := f.resolve(cfg.Path)
	if err != nil {
		return "", nil, 0, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", nil, 0, err
	}
	defer func() { _ = root.Close() }()

	var items []websource.ListedItem
	scanned := 0
	err = iofs.WalkDir(root.FS(), ".", func(rel string, d iofs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return iofs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		scanned++
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !cfg.Select(rel, "", info.Size()) {
			return nil
		}
		item := websource.ListedItem{
			Key:       rel,
			Version:   strconv.FormatInt(info.ModTime().UnixNano(), 36) + "-" + strconv.FormatInt(info.Size(), 36),
			UpdatedAt: info.ModTime(),
		}
		if cfg.BaseURL != "" {
			item.URL = strings.TrimRight(cfg.BaseURL, "/") + "/" + rel
		}
		items = append(items, item)

		return nil
	})

	return dir, items, scanned, err
}

func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	if err := f.Validate(raw); err != nil {
		return websource.PreviewResult{}, err
	}
	cfg, _ := decode(raw)
	_, items, scanned, err := f.list(ctx, cfg)

	return filesrc.Preview(items, scanned, cfg.MaxFiles), err
}

func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, state json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decode(col.Config)
	if err != nil {
		return nil, err
	}
	dir, items, _, err := f.list(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := cfg.Check(items); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	return filesrc.Sync(ctx, state, cfg.Signature(dir, cfg.BaseURL), items, func(_ context.Context, item websource.ListedItem) ([]byte, string, error) {
		file, err := root.Open(item.Key)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = file.Close() }()
		data, err := readLimited(file, cfg.MaxBytes())

		return data, "", err
	}, cfg.MaxBytes(), emit)
}

func readLimited(f *os.File, limit int64) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, errors.New("file exceeds size limit")
	}
	buf := make([]byte, info.Size())
	n, err := f.ReadAt(buf, 0)
	if err != nil && n != len(buf) {
		return nil, err
	}

	return buf[:n], nil
}
