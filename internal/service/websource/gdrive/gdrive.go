// Package gdrive implements the "gdrive" web-source type: files of a Google
// Drive folder tree or shared drive, read with a service account. Google
// Docs/Sheets/Slides are exported (HTML, CSV, plain text); uploaded files go
// through the shared converter. Unchanged files are skipped by version.
package gdrive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/convert"
	"github.com/rytsh/krabby/internal/service/websource/filesrc"
)

// Type is the collection type served by this fetcher.
const Type = "gdrive"

const (
	folderMime = "application/vnd.google-apps.folder"
	maxFolders = 5000
	scope      = "https://www.googleapis.com/auth/drive.readonly"
)

// exports maps Google-native types to the export format we convert.
var exports = map[string]string{
	"application/vnd.google-apps.document":     "text/html",
	"application/vnd.google-apps.spreadsheet":  "text/csv",
	"application/vnd.google-apps.presentation": "text/plain",
}

// Config is the provider config.
type Config struct {
	filesrc.Common
	ServiceAccount string `json:"service_account_json"`
	Subject        string `json:"impersonate,omitempty"`
	FolderID       string `json:"folder_id,omitempty"`
	DriveID        string `json:"drive_id,omitempty"`
}

// Fetcher syncs Drive files.
type Fetcher struct {
	websource.Schema
	api    string
	client func(ctx context.Context, cfg Config) (*http.Client, error)
}

// New creates the fetcher.
func New() *Fetcher {
	fields := append([]websource.Field{
		{Key: "service_account_json", Label: "Service account key (JSON)", Kind: websource.FieldSecret, Required: true,
			Help: "Share the folder with the service account email, or use domain-wide delegation with impersonate"},
		{Key: "impersonate", Label: "Impersonate user (domain-wide delegation)", Kind: websource.FieldString, Placeholder: "docs-bot@example.com"},
		{Key: "folder_id", Label: "Folder id", Kind: websource.FieldString, Placeholder: "1AbC…"},
		{Key: "drive_id", Label: "Shared drive id", Kind: websource.FieldString},
	}, filesrc.Fields()...)

	return &Fetcher{api: "https://www.googleapis.com/drive/v3", client: oauthClient, Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:  Type,
		Label: "Google Drive",
		Description: "Index a Drive folder tree or shared drive with a service account. Docs, Sheets and Slides are exported; " +
			"uploaded PDF/Office/Markdown files are converted.",
		Fields: fields,
	}}}
}

func oauthClient(ctx context.Context, cfg Config) (*http.Client, error) {
	creds, err := google.CredentialsFromJSONWithParams(ctx, []byte(cfg.ServiceAccount), google.CredentialsParams{
		Scopes: []string{scope}, Subject: cfg.Subject,
	})
	if err != nil {
		return nil, fmt.Errorf("parse service account key; %w", err)
	}
	c := &http.Client{Timeout: 5 * time.Minute}
	c.Transport = &oauth2.Transport{Source: creds.TokenSource}

	return c, nil
}

func decode(raw json.RawMessage) (Config, error) {
	cfg, err := websource.DecodeConfig[Config](raw, Type)
	cfg.FolderID = strings.TrimSpace(cfg.FolderID)
	cfg.DriveID = strings.TrimSpace(cfg.DriveID)

	return cfg, err
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decode(raw)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.ServiceAccount) == "" {
		return fmt.Errorf("gdrive service_account_json is required")
	}
	var key struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal([]byte(cfg.ServiceAccount), &key); err != nil || key.Type != "service_account" || key.ClientEmail == "" {
		return fmt.Errorf("service_account_json must be a Google service account key file")
	}
	if cfg.FolderID == "" && cfg.DriveID == "" {
		return fmt.Errorf("set folder_id or drive_id")
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

type driveFile struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	MimeType     string    `json:"mimeType"`
	Version      string    `json:"version"`
	MD5          string    `json:"md5Checksum"`
	Size         string    `json:"size"`
	ModifiedTime time.Time `json:"modifiedTime"`
	WebViewLink  string    `json:"webViewLink"`
	Trashed      bool      `json:"trashed"`
}

func (f *Fetcher) listFolder(ctx context.Context, c *http.Client, cfg Config, query string, fn func(driveFile)) error {
	token := ""
	for {
		q := url.Values{
			"q":                         {query},
			"pageSize":                  {"1000"},
			"fields":                    {"nextPageToken, files(id,name,mimeType,version,md5Checksum,size,modifiedTime,webViewLink,trashed)"},
			"supportsAllDrives":         {"true"},
			"includeItemsFromAllDrives": {"true"},
		}
		if cfg.DriveID != "" {
			q.Set("corpora", "drive")
			q.Set("driveId", cfg.DriveID)
		}
		if token != "" {
			q.Set("pageToken", token)
		}
		var res struct {
			Files         []driveFile `json:"files"`
			NextPageToken string      `json:"nextPageToken"`
		}
		if _, err := websource.GetJSON(ctx, c, f.api+"/files?"+q.Encode(), nil, &res); err != nil {
			return err
		}
		for _, file := range res.Files {
			fn(file)
		}
		if res.NextPageToken == "" {
			return nil
		}
		token = res.NextPageToken
	}
}

func (f *Fetcher) list(ctx context.Context, cfg Config) (*http.Client, []websource.ListedItem, map[string]driveFile, int, error) {
	c, err := f.client(ctx, cfg)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	root := firstNonEmpty(cfg.FolderID, cfg.DriveID)
	type dir struct{ id, path string }
	queue := []dir{{id: root}}
	seen := map[string]bool{root: true}
	files := map[string]driveFile{}
	var items []websource.ListedItem
	scanned := 0

	for len(queue) > 0 {
		if len(seen) > maxFolders {
			return nil, nil, nil, 0, fmt.Errorf("drive tree has more than %d folders; pick a narrower folder", maxFolders)
		}
		d := queue[0]
		queue = queue[1:]
		err := f.listFolder(ctx, c, cfg, fmt.Sprintf("'%s' in parents and trashed = false", strings.ReplaceAll(d.id, "'", "")), func(file driveFile) {
			p := strings.TrimPrefix(d.path+"/"+file.Name, "/")
			if file.MimeType == folderMime {
				if !seen[file.ID] {
					seen[file.ID] = true
					queue = append(queue, dir{id: file.ID, path: p})
				}
				return
			}
			scanned++
			var size int64
			_, _ = fmt.Sscan(file.Size, &size)
			key := p
			ct := file.MimeType
			if exp, ok := exports[file.MimeType]; ok {
				ct = exp
				key = p + extFor(exp)
			} else if strings.HasPrefix(file.MimeType, "application/vnd.google-apps.") {
				return // forms, drawings, shortcuts: nothing to index
			}
			if !cfg.Select(key, ct, size) {
				return
			}
			files[file.ID] = file
			items = append(items, websource.ListedItem{
				Key: file.ID, Path: key, Version: firstNonEmpty(file.Version, file.MD5, file.ModifiedTime.String()),
				Title: strings.TrimSuffix(file.Name, extOf(file.Name)), URL: file.WebViewLink, UpdatedAt: file.ModifiedTime,
			})
		})
		if err != nil {
			return nil, nil, nil, 0, err
		}
	}

	return c, items, files, scanned, nil
}

func extFor(mime string) string {
	switch mime {
	case "text/html":
		return ".html"
	case "text/csv":
		return ".csv"
	}

	return ".txt"
}

func extOf(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 && convert.Supported(name) {
		return name[i:]
	}

	return ""
}

func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	if err := f.Validate(raw); err != nil {
		return websource.PreviewResult{}, err
	}
	cfg, _ := decode(raw)
	_, items, _, scanned, err := f.list(ctx, cfg)

	return filesrc.Preview(items, scanned, cfg.MaxFiles), err
}

func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, state json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decode(col.Config)
	if err != nil {
		return nil, err
	}
	c, items, files, _, err := f.list(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := cfg.Check(items); err != nil {
		return nil, err
	}

	return filesrc.Sync(ctx, state, cfg.Signature(cfg.FolderID, cfg.DriveID, cfg.Subject), items,
		func(ctx context.Context, item websource.ListedItem) ([]byte, string, error) {
			file := files[item.Key]
			endpoint := f.api + "/files/" + url.PathEscape(file.ID) + "?alt=media&supportsAllDrives=true"
			ct := file.MimeType
			if exp, ok := exports[file.MimeType]; ok {
				endpoint = f.api + "/files/" + url.PathEscape(file.ID) + "/export?mimeType=" + url.QueryEscape(exp)
				ct = exp
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if err != nil {
				return nil, "", err
			}
			res, err := websource.Do(c, req, cfg.MaxBytes())
			if err != nil {
				return nil, "", err
			}

			return res.Body, ct, nil
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
