// Package sharepoint implements the "sharepoint" web-source type: files of a
// SharePoint document library or OneDrive folder through Microsoft Graph,
// authenticated as an Entra ID (Azure AD) application with client
// credentials. Incremental by eTag.
package sharepoint

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2/clientcredentials"

	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/filesrc"
)

// Type is the collection type served by this fetcher.
const Type = "sharepoint"

const maxFolders = 5000

// Config is the provider config.
type Config struct {
	filesrc.Common
	TenantID     string `json:"tenant_id"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	SiteURL      string `json:"site_url,omitempty"`
	DriveID      string `json:"drive_id,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	FolderPath   string `json:"folder_path,omitempty"`
}

// Fetcher syncs SharePoint / OneDrive files.
type Fetcher struct {
	websource.Schema
	graph  string
	client func(ctx context.Context, cfg Config) *http.Client
}

// New creates the fetcher.
func New() *Fetcher {
	fields := append([]websource.Field{
		{Key: "tenant_id", Label: "Tenant id", Kind: websource.FieldString, Required: true},
		{Key: "client_id", Label: "App (client) id", Kind: websource.FieldString, Required: true},
		{Key: "client_secret", Label: "Client secret", Kind: websource.FieldSecret, Required: true,
			Help: "Application permission Sites.Read.All (or Files.Read.All), admin-consented"},
		{Key: "site_url", Label: "SharePoint site URL", Kind: websource.FieldString, Placeholder: "https://contoso.sharepoint.com/sites/Engineering"},
		{Key: "drive_id", Label: "Drive id (instead of site URL)", Kind: websource.FieldString},
		{Key: "user_id", Label: "OneDrive user (instead of site)", Kind: websource.FieldString, Placeholder: "user@contoso.com"},
		{Key: "folder_path", Label: "Folder path in the library", Kind: websource.FieldString, Placeholder: "Runbooks/Production"},
	}, filesrc.Fields()...)

	return &Fetcher{graph: "https://graph.microsoft.com/v1.0", client: oauthClient, Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:  Type,
		Label: "SharePoint / OneDrive",
		Description: "Index a SharePoint document library (or a OneDrive folder) through Microsoft Graph with an Entra ID app. " +
			"Word, PowerPoint, Excel, PDF and Markdown files are converted; unchanged files are skipped by eTag.",
		Fields: fields,
	}}}
}

func oauthClient(ctx context.Context, cfg Config) *http.Client {
	cc := clientcredentials.Config{
		ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		TokenURL: "https://login.microsoftonline.com/" + url.PathEscape(cfg.TenantID) + "/oauth2/v2.0/token",
		Scopes:   []string{"https://graph.microsoft.com/.default"},
	}
	c := cc.Client(ctx)
	c.Timeout = 5 * time.Minute

	return c
}

func decode(raw json.RawMessage) (Config, error) {
	cfg, err := websource.DecodeConfig[Config](raw, Type)
	cfg.SiteURL = strings.TrimRight(strings.TrimSpace(cfg.SiteURL), "/")
	cfg.FolderPath = strings.Trim(strings.TrimSpace(cfg.FolderPath), "/")

	return cfg, err
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decode(raw)
	if err != nil {
		return err
	}
	if cfg.TenantID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return fmt.Errorf("sharepoint needs tenant_id, client_id and client_secret")
	}
	set := 0
	for _, v := range []string{cfg.SiteURL, cfg.DriveID, cfg.UserID} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("set exactly one of site_url, drive_id or user_id")
	}
	if cfg.SiteURL != "" {
		u, err := url.Parse(cfg.SiteURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("site_url must be an https SharePoint site URL")
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

// driveRoot resolves the Graph path of the configured drive.
func (f *Fetcher) driveRoot(ctx context.Context, c *http.Client, cfg Config) (string, error) {
	switch {
	case cfg.DriveID != "":
		return "/drives/" + url.PathEscape(cfg.DriveID), nil
	case cfg.UserID != "":
		return "/users/" + url.PathEscape(cfg.UserID) + "/drive", nil
	}
	u, _ := url.Parse(cfg.SiteURL)
	var site struct {
		ID string `json:"id"`
	}
	if _, err := websource.GetJSON(ctx, c, f.graph+"/sites/"+u.Host+":"+u.EscapedPath(), nil, &site); err != nil {
		return "", fmt.Errorf("resolve site; %w", err)
	}

	return "/sites/" + url.PathEscape(site.ID) + "/drive", nil
}

type driveItem struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	ETag                 string    `json:"eTag"`
	CTag                 string    `json:"cTag"`
	Size                 int64     `json:"size"`
	WebURL               string    `json:"webUrl"`
	LastModifiedDateTime time.Time `json:"lastModifiedDateTime"`
	Folder               *struct{} `json:"folder"`
	File                 *struct {
		MimeType string `json:"mimeType"`
	} `json:"file"`
}

func (f *Fetcher) list(ctx context.Context, cfg Config) (*http.Client, string, []websource.ListedItem, int, error) {
	c := f.client(ctx, cfg)
	root, err := f.driveRoot(ctx, c, cfg)
	if err != nil {
		return nil, "", nil, 0, err
	}
	start := root + "/root"
	if cfg.FolderPath != "" {
		start = root + "/root:/" + escapePath(cfg.FolderPath) + ":"
	}
	type dir struct{ endpoint, path string }
	queue := []dir{{endpoint: f.graph + start + "/children?$top=200", path: ""}}
	folders := 0
	var items []websource.ListedItem
	scanned := 0
	for len(queue) > 0 {
		if folders++; folders > maxFolders {
			return nil, "", nil, 0, fmt.Errorf("library has more than %d folders; set folder_path", maxFolders)
		}
		d := queue[0]
		queue = queue[1:]
		next := d.endpoint
		for next != "" {
			var res struct {
				Value []driveItem `json:"value"`
				Next  string      `json:"@odata.nextLink"`
			}
			if _, err := websource.GetJSON(ctx, c, next, nil, &res); err != nil {
				return nil, "", nil, 0, err
			}
			for _, it := range res.Value {
				p := strings.TrimPrefix(d.path+"/"+it.Name, "/")
				if it.Folder != nil {
					queue = append(queue, dir{endpoint: f.graph + root + "/items/" + url.PathEscape(it.ID) + "/children?$top=200", path: p})
					continue
				}
				if it.File == nil {
					continue
				}
				scanned++
				if !cfg.Select(p, it.File.MimeType, it.Size) {
					continue
				}
				items = append(items, websource.ListedItem{
					Key: it.ID, Path: p, Version: firstNonEmpty(it.CTag, it.ETag), URL: it.WebURL, UpdatedAt: it.LastModifiedDateTime,
				})
			}
			next = res.Next
		}
	}

	return c, root, items, scanned, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}

	return strings.Join(parts, "/")
}

func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	if err := f.Validate(raw); err != nil {
		return websource.PreviewResult{}, err
	}
	cfg, _ := decode(raw)
	_, _, items, scanned, err := f.list(ctx, cfg)

	return filesrc.Preview(items, scanned, cfg.MaxFiles), err
}

func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, state json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decode(col.Config)
	if err != nil {
		return nil, err
	}
	c, root, items, _, err := f.list(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := cfg.Check(items); err != nil {
		return nil, err
	}

	return filesrc.Sync(ctx, state, cfg.Signature(root, cfg.FolderPath), items,
		func(ctx context.Context, item websource.ListedItem) ([]byte, string, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.graph+root+"/items/"+url.PathEscape(item.Key)+"/content", nil)
			if err != nil {
				return nil, "", err
			}
			res, err := websource.Do(c, req, cfg.MaxBytes())
			if err != nil {
				return nil, "", err
			}

			return res.Body, res.Header.Get("Content-Type"), nil
		}, cfg.MaxBytes(), emit)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}

	return ""
}
