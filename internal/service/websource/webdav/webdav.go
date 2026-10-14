// Package webdav implements the "webdav" web-source type: files of a WebDAV
// folder (Nextcloud, ownCloud, SharePoint/OneDrive WebDAV, Apache mod_dav),
// listed recursively with PROPFIND and synced incrementally by ETag.
package webdav

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/filesrc"
)

// Type is the collection type served by this fetcher.
const Type = "webdav"

const maxDirs = 5000

// Config is the provider config.
type Config struct {
	filesrc.Common
	URL   string `json:"url"`
	User  string `json:"user,omitempty"`
	Token string `json:"password,omitempty"`
}

// Fetcher syncs a WebDAV folder.
type Fetcher struct {
	websource.Schema
	client *http.Client
}

// New creates the fetcher.
func New() *Fetcher {
	fields := append([]websource.Field{
		{Key: "url", Label: "Folder URL", Kind: websource.FieldString, Required: true,
			Placeholder: "https://cloud.example.com/remote.php/dav/files/me/Docs/"},
		{Key: "user", Label: "User (empty = bearer token)", Kind: websource.FieldString},
		{Key: "password", Label: "Password / app token", Kind: websource.FieldSecret},
	}, filesrc.Fields()...)

	return &Fetcher{client: websource.DefaultHTTPClient(), Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:        Type,
		Label:       "WebDAV folder (Nextcloud, ownCloud, …)",
		Description: "Index documents of a WebDAV folder recursively. Unchanged files are skipped by ETag.",
		Fields:      fields,
	}}}
}

func decode(raw json.RawMessage) (Config, error) {
	cfg, err := websource.DecodeConfig[Config](raw, Type)
	cfg.URL = strings.TrimSpace(cfg.URL)
	if cfg.URL != "" && !strings.HasSuffix(cfg.URL, "/") {
		cfg.URL += "/"
	}

	return cfg, err
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decode(raw)
	if err != nil {
		return err
	}
	if err := websource.ValidHTTPURL(cfg.URL); err != nil {
		return fmt.Errorf("webdav url %w", err)
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

type davProp struct {
	ETag         string `xml:"getetag"`
	Length       int64  `xml:"getcontentlength"`
	Type         string `xml:"getcontenttype"`
	LastModified string `xml:"getlastmodified"`
	DisplayName  string `xml:"displayname"`
	ResourceType struct {
		Collection *struct{} `xml:"collection"`
	} `xml:"resourcetype"`
}

type multistatus struct {
	Responses []struct {
		Href  string `xml:"href"`
		Props []struct {
			Prop   davProp `xml:"prop"`
			Status string  `xml:"status"`
		} `xml:"propstat"`
	} `xml:"response"`
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop>` +
	`<d:getetag/><d:getcontentlength/><d:getcontenttype/><d:getlastmodified/><d:displayname/><d:resourcetype/>` +
	`</d:prop></d:propfind>`

func (f *Fetcher) propfind(ctx context.Context, cfg Config, dir *url.URL) (*multistatus, error) {
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", dir.String(), strings.NewReader(propfindBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	websource.ApplyAuth(req, cfg.User, cfg.Token)
	res, err := websource.Do(f.client, req, 32<<20)
	if err != nil {
		return nil, err
	}
	var ms multistatus
	if err := xml.Unmarshal(res.Body, &ms); err != nil {
		return nil, fmt.Errorf("decode PROPFIND response; %w", err)
	}

	return &ms, nil
}

// list walks the folder tree breadth-first, one PROPFIND (Depth: 1) per
// directory, since many servers refuse Depth: infinity.
func (f *Fetcher) list(ctx context.Context, cfg Config) ([]websource.ListedItem, int, error) {
	root, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, 0, err
	}
	rootPath := root.EscapedPath()
	queue := []*url.URL{root}
	seenDirs := map[string]bool{rootPath: true}
	var items []websource.ListedItem
	scanned := 0

	for len(queue) > 0 {
		if len(seenDirs) > maxDirs {
			return nil, 0, fmt.Errorf("webdav folder has more than %d directories; narrow the URL", maxDirs)
		}
		dir := queue[0]
		queue = queue[1:]
		ms, err := f.propfind(ctx, cfg, dir)
		if err != nil {
			return nil, 0, err
		}
		for _, r := range ms.Responses {
			href, err := url.Parse(strings.TrimSpace(r.Href))
			if err != nil {
				continue
			}
			abs := dir.ResolveReference(href)
			if abs.Host != root.Host || !strings.HasPrefix(abs.EscapedPath(), rootPath) {
				continue
			}
			p := abs.EscapedPath()
			if p == dir.EscapedPath() || p+"/" == dir.EscapedPath() {
				continue
			}
			var prop *davProp
			for i := range r.Props {
				if strings.Contains(r.Props[i].Status, " 200") || r.Props[i].Status == "" {
					prop = &r.Props[i].Prop
					break
				}
			}
			if prop == nil {
				continue
			}
			if prop.ResourceType.Collection != nil {
				if !strings.HasSuffix(p, "/") {
					abs.Path += "/"
				}
				if !seenDirs[abs.EscapedPath()] {
					seenDirs[abs.EscapedPath()] = true
					queue = append(queue, abs)
				}
				continue
			}
			scanned++
			rel, _ := url.PathUnescape(strings.TrimPrefix(p, rootPath))
			if strings.HasPrefix(path.Base(rel), ".") || !cfg.Select(rel, prop.Type, prop.Length) {
				continue
			}
			mod, _ := http.ParseTime(prop.LastModified)
			version := strings.Trim(prop.ETag, `"`)
			if version == "" {
				version = prop.LastModified + fmt.Sprint(prop.Length)
			}
			items = append(items, websource.ListedItem{
				Key: rel, Version: version, URL: abs.String(), UpdatedAt: mod,
				Title: strings.TrimSuffix(prop.DisplayName, path.Ext(prop.DisplayName)),
			})
		}
	}

	return items, scanned, nil
}

func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	if err := f.Validate(raw); err != nil {
		return websource.PreviewResult{}, err
	}
	cfg, _ := decode(raw)
	items, scanned, err := f.list(ctx, cfg)

	return filesrc.Preview(items, scanned, cfg.MaxFiles), err
}

func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, state json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decode(col.Config)
	if err != nil {
		return nil, err
	}
	items, _, err := f.list(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := cfg.Check(items); err != nil {
		return nil, err
	}

	return filesrc.Sync(ctx, state, cfg.Signature(cfg.URL), items, func(ctx context.Context, item websource.ListedItem) ([]byte, string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.URL, nil)
		if err != nil {
			return nil, "", err
		}
		websource.ApplyAuth(req, cfg.User, cfg.Token)
		client := *f.client
		client.Timeout = 5 * time.Minute
		res, err := websource.Do(&client, req, cfg.MaxBytes())
		if err != nil {
			return nil, "", err
		}

		return res.Body, res.Header.Get("Content-Type"), nil
	}, cfg.MaxBytes(), emit)
}
