package manager

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"slices"

	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/docsearch"
	"github.com/rytsh/krabby/internal/service/rag"
	"github.com/rytsh/krabby/internal/service/vectorstore"
)

func (m *Manager) indexBigPicture(ctx context.Context, name string, vectors bool) error {
	defer m.lockKey(bigpicture.ScopeKey(name))()
	return m.indexBigPictureLocked(ctx, name, vectors)
}

func (m *Manager) indexBigPictureLocked(ctx context.Context, name string, vectors bool) error {
	if m.bigPictures == nil {
		return nil
	}
	p, err := m.bigPictures.Get(ctx, name)
	if err != nil {
		return err
	}
	if p.CurrentRevision == "" {
		return nil
	}
	dir, err := m.bigPictures.PublicationDir(ctx, name, p.CurrentRevision)
	if err != nil {
		return err
	}
	b, release := m.acquireDocs()
	defer release()
	if b == nil {
		return ErrDocsDisabled
	}
	indexVectors := vectors && b.rag != nil
	if m.docsText == nil && !indexVectors {
		return nil
	}
	purge := func(key string, text, vector bool) error {
		var errs []error
		cleanup := context.WithoutCancel(ctx)
		if text && m.docsText != nil {
			errs = append(errs, m.docsText.DeleteRepo(cleanup, key))
		}
		if vector && b.store != nil {
			errs = append(errs, b.store.DeleteRepo(cleanup, key))
		}
		return errors.Join(errs...)
	}
	// Every attempt writes a fresh key, so readers that resolved the live key
	// never see a partially rebuilt index. The live index keeps serving until
	// the new one is complete. The key is recorded before any data is written
	// so an interrupted attempt is reclaimed by the next one.
	key := bigpicture.IndexKey(name, p.CurrentRevision) + ":" + rand.Text()
	stale, err := m.bigPictures.BeginIndex(ctx, name, p.StorageID, p.CurrentRevision, key)
	if err != nil {
		return err
	}
	var errs []error
	for _, old := range stale {
		if err := purge(old, true, true); err != nil {
			errs = append(errs, err)
		}
	}
	textDone, vectorDone := false, false
	if m.docsText != nil {
		if err := m.docsText.IndexWithOptions(ctx, key, dir, &rag.IndexOptions{KeepMarkdownTargets: b.ragCfg.KeepMarkdownTargets}); err != nil {
			errs = append(errs, err)
		} else {
			textDone = true
		}
	}
	if indexVectors {
		if err := b.rag.Index(ctx, key, dir); err != nil {
			errs = append(errs, err)
		} else {
			vectorDone = true
		}
	}
	retiredText, retiredVector := "", ""
	if textDone || vectorDone {
		retiredText, retiredVector, err = m.bigPictures.FinishIndex(ctx, name, p.StorageID, p.CurrentRevision, key, textDone, vectorDone)
		if err != nil {
			errs = append(errs, err)
			textDone, vectorDone = false, false
		}
	}
	// Discard whatever this attempt wrote but did not make live.
	if !textDone || !vectorDone {
		if err := purge(key, !textDone, !vectorDone); err != nil {
			errs = append(errs, err)
		}
	}
	if retiredText != "" {
		if err := purge(retiredText, true, false); err != nil {
			errs = append(errs, err)
		}
	}
	if retiredVector != "" {
		if err := purge(retiredVector, false, true); err != nil {
			errs = append(errs, err)
		}
	}
	if textDone && m.docsText != nil {
		if err := m.docsText.RefreshStats(ctx); err != nil {
			slog.Warn("refresh big picture search statistics", "name", name)
		}
	}
	return errors.Join(errs...)
}

// pictureSearchFilter constrains candidates BEFORE bounded ranking, including
// wildcard searches. Partial and retired publication indexes are never eligible.
func (m *Manager) pictureSearchFilter(ctx context.Context, scope, key, namespace, mode string, filter vectorstore.Filter) (vectorstore.Filter, bool, error) {
	filter.BigPictureKeys = []string{}
	if name := bigpicture.Name(key); name != "" {
		p, err := m.BigPicture(ctx, name)
		if err != nil {
			return filter, false, err
		}
		if !pictureIndexReady(p, mode) {
			return filter, true, nil
		}
		keys := pictureSearchKeys(p, mode)
		filter.Keys, filter.BigPictureKeys = slices.Clone(keys), keys
		return filter, false, nil
	}
	if key != "" || (scope != "" && scope != ScopeAll && scope != ScopeBigPictures) {
		return filter, false, nil
	}
	if m.bigPictures == nil {
		return filter, scope == ScopeBigPictures, nil
	}
	pictures, err := m.bigPictures.All(ctx, namespace)
	if err != nil {
		return filter, false, err
	}
	for _, p := range pictures {
		if !pictureIndexReady(p, mode) {
			continue
		}
		keys := pictureSearchKeys(p, mode)
		filter.BigPictureKeys = append(filter.BigPictureKeys, keys...)
		// A repository namespace filter already resolved an explicit allowlist.
		if len(filter.Keys) > 0 || scope == ScopeBigPictures {
			filter.Keys = append(filter.Keys, keys...)
		}
	}
	if scope == ScopeBigPictures && len(filter.BigPictureKeys) == 0 {
		return filter, true, nil
	}
	return filter, false, nil
}

func pictureIndexReady(p *bigpicture.Picture, mode string) bool {
	if p.CurrentRevision == "" {
		return false
	}
	text := p.TextRevision == p.CurrentRevision && p.TextIndex != ""
	vector := p.VectorRevision == p.CurrentRevision && p.VectorIndex != ""
	switch mode {
	case DocsSearchLexical:
		return text
	case DocsSearchSemantic:
		return vector
	default:
		return text && vector
	}
}

// pictureSearchKeys returns the live keys each ranker reads. Hybrid arms may
// come from different indexing attempts of the same publication; results are
// canonicalized to the public scope key before fusion.
func pictureSearchKeys(p *bigpicture.Picture, mode string) []string {
	switch mode {
	case DocsSearchLexical:
		return []string{p.TextIndex}
	case DocsSearchSemantic:
		return []string{p.VectorIndex}
	}
	if p.TextIndex == p.VectorIndex {
		return []string{p.TextIndex}
	}
	return []string{p.TextIndex, p.VectorIndex}
}

// canonicalPictureDocs rewrites internal index keys to the public
// bigpicture:<name> scope and pins the publication revision, so both hybrid
// arms fuse the same document regardless of which attempt indexed it.
func canonicalPictureDocs(docs []rag.Doc) []rag.Doc {
	for i := range docs {
		if name, revision := bigpicture.ParseIndexKey(docs[i].Repo); name != "" {
			docs[i].Repo, docs[i].Revision = bigpicture.ScopeKey(name), revision
		}
	}
	return docs
}

type pictureLexical struct{ docsearch.LexicalIndex }

func (l pictureLexical) Search(ctx context.Context, filter vectorstore.Filter, query string, limit int) ([]rag.Doc, error) {
	docs, err := l.LexicalIndex.Search(ctx, filter, query, limit)
	return canonicalPictureDocs(docs), err
}

func (m *Manager) warmPictureText(ctx context.Context, scope, key, namespace string) error {
	if m.bigPictures == nil || m.docsText == nil {
		return nil
	}
	if name := bigpicture.Name(key); name != "" {
		p, err := m.BigPicture(ctx, name)
		if err != nil {
			return err
		}
		if p.CurrentRevision != "" && !pictureIndexReady(p, DocsSearchLexical) {
			return m.tryWarmPicture(ctx, name)
		}
		return nil
	}
	if key != "" || (scope != "" && scope != ScopeAll && scope != ScopeBigPictures) {
		return nil
	}
	pictures, err := m.bigPictures.All(ctx, namespace)
	if err != nil {
		return err
	}
	for _, p := range pictures {
		if p.CurrentRevision != "" && !pictureIndexReady(p, DocsSearchLexical) {
			if err := m.tryWarmPicture(ctx, p.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) tryWarmPicture(ctx context.Context, name string) error {
	release, ok := m.tryLockKey(bigpicture.ScopeKey(name))
	if !ok {
		return nil
	} // a queued/running index will finish; never block search
	defer release()
	p, err := m.BigPicture(ctx, name)
	if err != nil {
		return err
	}
	if pictureIndexReady(p, DocsSearchLexical) {
		return nil
	}
	return m.indexBigPictureLocked(ctx, name, false)
}

// BackfillBigPictureIndexes runs after durable task restore and queues only
// missing index work, including vectors for publications from older versions.
func (m *Manager) BackfillBigPictureIndexes(ctx context.Context) error {
	if m.bigPictures == nil || m.queue == nil {
		return nil
	}
	pictures, err := m.bigPictures.All(ctx, "*")
	if err != nil {
		return err
	}
	for _, p := range pictures {
		if err := m.repairPictureIndexes(p); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) repairPictureIndexes(p *bigpicture.Picture) error {
	if p.CurrentRevision == "" || m.queue == nil {
		return nil
	}
	b, release := m.acquireDocs()
	needsText := m.docsText != nil && (p.TextRevision != p.CurrentRevision || p.TextIndex == "")
	needsVectors := b != nil && b.rag != nil && (p.VectorRevision != p.CurrentRevision || p.VectorIndex == "")
	release()
	if needsText || needsVectors {
		return m.scheduleReindex(bigpicture.ScopeKey(p.Name))
	}
	return nil
}
