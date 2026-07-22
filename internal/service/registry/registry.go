// Package registry persists the set of tracked repositories in a bw bucket.
package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rakunlabs/bw"
	"github.com/rakunlabs/query"
)

// Repo status values.
const (
	StatusPending  = "pending"
	StatusCloning  = "cloning"
	StatusBuilding = "building"
	StatusReady    = "ready"
	StatusError    = "error"
)

// Generation stage names. Each stage produces one artifact and can be run
// selectively via Manager.Generate.
const (
	StageGraph     = "graph"
	StageDocs      = "docs"
	StageDocsIndex = "docs_index"
	StageCodeIndex = "code_index"
)

// Stage status values. An empty status means the stage never ran.
const (
	StageRunning = "running"
	StageOK      = "ok"
	StageError   = "error"
)

// ValidStage reports whether name is a known generation stage.
func ValidStage(name string) bool {
	switch name {
	case StageGraph, StageDocs, StageDocsIndex, StageCodeIndex:
		return true
	}

	return false
}

// StageState records the last outcome of one generation stage.
type StageState struct {
	Status     string    `bw:"status"      json:"status,omitempty"` // "", running, ok, error
	Error      string    `bw:"error"       json:"error,omitempty"`
	Commit     string    `bw:"commit"      json:"commit,omitempty"` // commit the stage last ran against
	FinishedAt time.Time `bw:"finished_at" json:"finished_at,omitzero"`
}

// Stages groups the per-artifact generation states of a repo.
type Stages struct {
	Graph     StageState `bw:"graph"      json:"graph"`
	Docs      StageState `bw:"docs"       json:"docs"`
	DocsIndex StageState `bw:"docs_index" json:"docs_index"`
	CodeIndex StageState `bw:"code_index" json:"code_index"`
}

// Get returns a mutable pointer to the named stage, or nil for unknown names.
func (s *Stages) Get(name string) *StageState {
	switch name {
	case StageGraph:
		return &s.Graph
	case StageDocs:
		return &s.Docs
	case StageDocsIndex:
		return &s.DocsIndex
	case StageCodeIndex:
		return &s.CodeIndex
	}

	return nil
}

// Repo is a tracked repository record.
type Repo struct {
	ID          string    `bw:"id,pk"        json:"id"` // owner/name
	URL         string    `bw:"url"          json:"url"`
	Branch      string    `bw:"branch"       json:"branch,omitempty"`
	Path        string    `bw:"path"         json:"path"`
	LastCommit  string    `bw:"last_commit"  json:"last_commit,omitempty"`
	LastSyncAt  time.Time `bw:"last_sync"    json:"last_sync_at,omitzero"`
	LastBuildAt time.Time `bw:"last_build"   json:"last_build_at,omitzero"`
	Status      string    `bw:"status,index" json:"status"`
	LastError   string    `bw:"last_error"   json:"last_error,omitempty"`
	Stages      Stages    `bw:"stages"       json:"stages"`
}

// Registry stores Repo records.
type Registry struct {
	bucket *bw.Bucket[Repo]
}

// repoSchemaVersion must be bumped whenever the Repo struct changes shape so
// bw auto-migrates existing buckets instead of failing with a fingerprint
// mismatch at startup. v2: added per-stage generation states (Stages).
const repoSchemaVersion = 2

// New opens the repos bucket on the given database.
func New(db *bw.DB) (*Registry, error) {
	bucket, err := bw.RegisterBucket[Repo](db, "repos", bw.WithVersion[Repo](repoSchemaVersion))
	if err != nil {
		return nil, fmt.Errorf("register repos bucket; %w", err)
	}

	return &Registry{bucket: bucket}, nil
}

// Get returns a repo by id, or nil if it does not exist.
func (r *Registry) Get(ctx context.Context, id string) (*Repo, error) {
	repo, err := r.bucket.Get(ctx, id)
	if err != nil {
		if errors.Is(err, bw.ErrNotFound) {
			return nil, nil
		}

		return nil, fmt.Errorf("get repo %s; %w", id, err)
	}

	return repo, nil
}

// List returns all tracked repos.
func (r *Registry) List(ctx context.Context) ([]*Repo, error) {
	q, err := query.Parse("_limit=10000")
	if err != nil {
		return nil, fmt.Errorf("parse query; %w", err)
	}

	repos, err := r.bucket.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list repos; %w", err)
	}

	if repos == nil {
		repos = []*Repo{}
	}

	return repos, nil
}

// Upsert inserts or replaces a repo record.
func (r *Registry) Upsert(ctx context.Context, repo *Repo) error {
	if err := r.bucket.Insert(ctx, repo); err != nil {
		return fmt.Errorf("upsert repo %s; %w", repo.ID, err)
	}

	return nil
}

// Delete removes a repo record.
func (r *Registry) Delete(ctx context.Context, id string) error {
	if err := r.bucket.Delete(ctx, id); err != nil && !errors.Is(err, bw.ErrNotFound) {
		return fmt.Errorf("delete repo %s; %w", id, err)
	}

	return nil
}
