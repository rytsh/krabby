// Package s3 implements the "s3" web-source type: objects of an S3-compatible
// bucket (AWS S3, MinIO, Ceph, Cloudflare R2, GCS interoperability) under an
// optional prefix, synced incrementally by ETag.
package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rytsh/krabby/internal/service/websource"
	"github.com/rytsh/krabby/internal/service/websource/filesrc"
)

// Type is the collection type served by this fetcher.
const Type = "s3"

// Config is the provider config.
type Config struct {
	filesrc.Common
	Bucket       string `json:"bucket"`
	Prefix       string `json:"prefix,omitempty"`
	Region       string `json:"region,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	AccessKey    string `json:"access_key_id,omitempty"`
	SecretKey    string `json:"secret_access_key,omitempty"`
	SessionToken string `json:"session_token,omitempty"`
	PathStyle    bool   `json:"path_style,omitempty"`
	LinkBase     string `json:"link_base_url,omitempty"`
}

// Fetcher syncs a bucket.
type Fetcher struct {
	websource.Schema
}

// New creates the fetcher.
func New() *Fetcher {
	fields := append([]websource.Field{
		{Key: "bucket", Label: "Bucket", Kind: websource.FieldString, Required: true, Placeholder: "team-docs"},
		{Key: "prefix", Label: "Key prefix", Kind: websource.FieldString, Placeholder: "handbook/"},
		{Key: "region", Label: "Region", Kind: websource.FieldString, Placeholder: "eu-west-1"},
		{Key: "endpoint", Label: "Endpoint (MinIO, R2, GCS, …)", Kind: websource.FieldString, Placeholder: "https://minio.example.com"},
		{Key: "path_style", Label: "Path-style addressing (MinIO)", Kind: websource.FieldBool},
		{Key: "access_key_id", Label: "Access key id", Kind: websource.FieldString, Help: "Empty uses the server's AWS credential chain"},
		{Key: "secret_access_key", Label: "Secret access key", Kind: websource.FieldSecret},
		{Key: "session_token", Label: "Session token", Kind: websource.FieldSecret},
		{Key: "link_base_url", Label: "Link base URL (optional)", Kind: websource.FieldString, Placeholder: "https://cdn.example.com/"},
	}, filesrc.Fields()...)

	return &Fetcher{Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:        Type,
		Label:       "S3 / object storage",
		Description: "Index documents in an S3-compatible bucket (AWS S3, MinIO, Ceph, R2, GCS interop). Unchanged objects are skipped by ETag.",
		Fields:      fields,
	}}}
}

func decode(raw json.RawMessage) (Config, error) {
	cfg, err := websource.DecodeConfig[Config](raw, Type)
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.Prefix = strings.TrimLeft(strings.TrimSpace(cfg.Prefix), "/")
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}

	return cfg, err
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decode(raw)
	if err != nil {
		return err
	}
	if cfg.Bucket == "" {
		return fmt.Errorf("s3 bucket is required")
	}
	if cfg.Endpoint != "" {
		if err := websource.ValidHTTPURL(cfg.Endpoint); err != nil {
			return fmt.Errorf("s3 endpoint %w", err)
		}
	}
	if (cfg.AccessKey == "") != (cfg.SecretKey == "") {
		return fmt.Errorf("s3 access_key_id and secret_access_key must be set together")
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

func client(ctx context.Context, cfg Config) (*awss3.Client, error) {
	opts := awss3.Options{Region: cfg.Region, UsePathStyle: cfg.PathStyle}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(strings.TrimRight(cfg.Endpoint, "/"))
	}
	if cfg.AccessKey != "" {
		opts.Credentials = credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, cfg.SessionToken)
	} else {
		loaded, err := defaultCredentials(ctx, cfg.Region)
		if err != nil {
			return nil, err
		}
		opts.Credentials = loaded
	}

	return awss3.New(opts), nil
}

func (f *Fetcher) list(ctx context.Context, cfg Config) (*awss3.Client, []websource.ListedItem, int, error) {
	c, err := client(ctx, cfg)
	if err != nil {
		return nil, nil, 0, err
	}
	var items []websource.ListedItem
	scanned := 0
	p := awss3.NewListObjectsV2Paginator(c, &awss3.ListObjectsV2Input{Bucket: aws.String(cfg.Bucket), Prefix: aws.String(cfg.Prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("list s3://%s/%s; %w", cfg.Bucket, cfg.Prefix, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if strings.HasSuffix(key, "/") {
				continue
			}
			scanned++
			rel := strings.TrimPrefix(key, cfg.Prefix)
			if !cfg.Select(rel, "", aws.ToInt64(obj.Size)) {
				continue
			}
			item := websource.ListedItem{Key: key, Version: strings.Trim(aws.ToString(obj.ETag), `"`), UpdatedAt: aws.ToTime(obj.LastModified)}
			if cfg.LinkBase != "" {
				item.URL = strings.TrimRight(cfg.LinkBase, "/") + "/" + (&url.URL{Path: key}).EscapedPath()
			}
			items = append(items, item)
		}
	}

	return c, items, scanned, nil
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
	c, items, _, err := f.list(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := cfg.Check(items); err != nil {
		return nil, err
	}

	return filesrc.Sync(ctx, state, cfg.Signature(cfg.Bucket, cfg.Prefix, cfg.Endpoint, cfg.LinkBase), items,
		func(ctx context.Context, item websource.ListedItem) ([]byte, string, error) {
			out, err := c.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(cfg.Bucket), Key: aws.String(item.Key)})
			if err != nil {
				return nil, "", fmt.Errorf("get %s; %w", item.Key, err)
			}
			defer func() { _ = out.Body.Close() }()
			data, err := io.ReadAll(io.LimitReader(out.Body, cfg.MaxBytes()+1))

			return data, aws.ToString(out.ContentType), err
		}, cfg.MaxBytes(), emit)
}
