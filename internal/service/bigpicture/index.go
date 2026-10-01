package bigpicture

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rakunlabs/query"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

const ScopePrefix = vectorstore.BigPictureScopePrefix

func ScopeKey(name string) string { return ScopePrefix + name }
func Name(key string) string {
	if !strings.HasPrefix(key, ScopePrefix) {
		return ""
	}
	return strings.TrimPrefix(key, ScopePrefix)
}
func IndexKey(name, revision string) string { return ScopeKey(name) + ":" + revision }
func ParseIndexKey(key string) (string, string) {
	name, revision, ok := strings.Cut(Name(key), ":")
	if rev, epoch, found := strings.Cut(revision, ":"); found {
		if !storagePattern.MatchString(epoch) {
			return "", ""
		}
		revision = rev
	}
	if !ok || !namePattern.MatchString(name) || !storagePattern.MatchString(revision) {
		return "", ""
	}
	return name, revision
}

// All is used by internal indexing/scheduling, not exposed as an unbounded
// transport response. Namespace labels are independent of repository tags.
func (s *Store) All(ctx context.Context, namespace string) ([]*Picture, error) {
	namespace = strings.ToLower(strings.TrimSpace(namespace))
	q := query.New()
	if namespace != "*" {
		q.Where = []query.Expression{query.NewExpressionCmp(query.OperatorEq, "namespace", NormalizeNamespace(namespace)).Expression()}
	}
	q.Sort = []query.ExpressionSort{{Field: "name"}}
	q.SetLimit(256)
	var items []*Picture
	for offset := uint64(0); ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		q.SetOffset(offset)
		page, err := s.bucket.Find(ctx, q)
		if err != nil {
			return nil, err
		}
		items = append(items, page...)
		if len(page) < 256 {
			return items, nil
		}
		offset += uint64(len(page))
	}
}

// PublicationDir is internal service wiring; transports only use paged reads.
func (s *Store) PublicationDir(ctx context.Context, name, revision string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, err := s.snapshot(ctx, name, revision)
	if err != nil {
		return "", err
	}
	p, err := s.Get(ctx, name)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, p.StorageID, snapshot.ID), nil
}

// BeginIndex records key as an in-progress attempt before any data is written,
// and returns keys left behind by interrupted attempts for the caller to purge.
// The live indexes stay searchable until FinishIndex switches them.
func (s *Store) BeginIndex(ctx context.Context, name, instance, revision, key string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if p.StorageID != instance || p.CurrentRevision != revision {
		return nil, ErrConflict
	}
	if indexName, indexRevision := ParseIndexKey(key); indexName != name || indexRevision != revision {
		return nil, ErrInvalid
	}
	stale := slices.DeleteFunc(slices.Clone(p.IndexPending), func(k string) bool { return k == p.TextIndex || k == p.VectorIndex })
	p.IndexPending = []string{key}
	return stale, s.bucket.Insert(ctx, p)
}

// FinishIndex atomically makes the completed sides of an attempt live and
// returns previously live keys whose text or vector data is no longer
// referenced on that side. A conflict means the publication changed; the caller
// must purge the attempt.
func (s *Store) FinishIndex(ctx context.Context, name, instance, revision, key string, text, vector bool) (retiredText, retiredVector string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.Get(ctx, name)
	if err != nil {
		return "", "", err
	}
	if p.StorageID != instance || p.CurrentRevision != revision {
		return "", "", ErrConflict
	}
	if text {
		if p.TextIndex != key {
			retiredText = p.TextIndex
		}
		p.TextIndex, p.TextRevision = key, revision
	}
	if vector {
		if p.VectorIndex != key {
			retiredVector = p.VectorIndex
		}
		p.VectorIndex, p.VectorRevision = key, revision
	}
	p.IndexPending = slices.DeleteFunc(p.IndexPending, func(k string) bool { return k == key })
	// A key still live on the other side must keep its row in IndexKeys; only
	// its data on the replaced side is retired.
	if err := s.bucket.Insert(ctx, p); err != nil {
		return "", "", err
	}
	return retiredText, retiredVector, nil
}

// RecordRun stores the latest automatic research outcome for this instance.
func (s *Store) RecordRun(ctx context.Context, name, instance string, run Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	if p.StorageID != instance {
		return ErrConflict
	}
	if len(run.Message) > 1024 {
		run.Message = strings.ToValidUTF8(run.Message[:1024], "")
	}
	p.LastRun = &run
	return s.bucket.Insert(ctx, p)
}

// IndexKeys lists every index key that may hold data for a workspace.
func (p *Picture) IndexKeys() []string {
	keys := slices.Clone(p.IndexPending)
	for _, key := range []string{p.TextIndex, p.VectorIndex} {
		if key != "" && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}
