package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/queue"
	"github.com/rytsh/krabby/internal/service/repofs"
)

const pictureTaskKind = "bigpicture_generate"

// TriggerBigPictureGeneration captures publication preconditions at submission,
// including instance identity so queued jobs cannot target a recreated name.
func (m *Manager) TriggerBigPictureGeneration(ctx context.Context, name string) error {
	return m.triggerPictureGeneration(ctx, name, "manual")
}

func (m *Manager) triggerPictureGeneration(ctx context.Context, name, trigger string) error {
	p, err := m.BigPicture(ctx, name)
	if err != nil {
		return err
	}
	b, release := m.acquireDocs()
	configured := b != nil && b.pictureChat != nil
	release()
	if !configured {
		return bigpicture.ErrGenerationUnavailable
	}
	if m.queue == nil {
		return errors.New("task queue unavailable")
	}
	spec := queue.Spec{Kind: pictureTaskKind, ID: "bigpicture:" + name, Params: map[string]string{"instance": p.StorageID, "version": strconv.FormatUint(p.Version, 10), "revision": p.CurrentRevision, "trigger": trigger}}
	return m.queue.Submit(m.pictureGenerateTask(spec)).Err()
}

func (m *Manager) pictureGenerateTask(spec queue.Spec) queue.Task {
	return queue.Task{ID: spec.ID, Kind: pictureTaskKind, Title: "Research and generate " + spec.ID, Key: spec.ID + ":" + pictureTaskKind, Spec: spec, Run: func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		name := strings.TrimPrefix(spec.ID, "bigpicture:")
		trigger := spec.Params["trigger"]
		if trigger == "" {
			trigger = "manual"
		}
		p, err := m.BigPicture(ctx, name)
		if err != nil {
			return err
		}
		if p.StorageID != spec.Params["instance"] || strconv.FormatUint(p.Version, 10) != spec.Params["version"] || p.CurrentRevision != spec.Params["revision"] {
			return bigpicture.ErrConflict
		}
		run := bigpicture.Run{Trigger: trigger, ConfigVersion: p.Version, Revision: p.CurrentRevision}
		status, err := m.runPictureGeneration(ctx, name, p, &run)
		run.Status, run.At = status, time.Now().UTC()
		if err != nil {
			run.Status, run.Message = bigpicture.RunFailed, err.Error()
		}
		// A conflict means newer work owns the workspace; do not overwrite its
		// outcome. Recording is best effort and never masks the task result.
		if !errors.Is(err, bigpicture.ErrConflict) && !errors.Is(err, bigpicture.ErrNotFound) {
			if recordErr := m.bigPictures.RecordRun(context.WithoutCancel(ctx), name, p.StorageID, run); recordErr != nil && !errors.Is(recordErr, bigpicture.ErrNotFound) {
				slog.Warn("record big picture run", "name", name, "error", recordErr)
			}
		}
		return err
	}}
}

// runPictureGeneration returns the run status. Unchanged evidence and patches
// with no document changes are recorded without publishing a new revision.
func (m *Manager) runPictureGeneration(ctx context.Context, name string, p *bigpicture.Picture, run *bigpicture.Run) (string, error) {
	pub, status, err := func() (*bigpicture.Publication, string, error) {
		b, release := m.acquireDocs()
		defer release()
		if b == nil || b.pictureChat == nil {
			return nil, "", bigpicture.ErrGenerationUnavailable
		}
		if err := m.validatePictureSources(ctx, p.Sources); err != nil {
			return nil, "", err
		}
		research, err := m.collectPictureResearch(ctx, p)
		if err != nil {
			return nil, "", err
		}
		run.ResearchHash = bigpicture.Fingerprint(p, research)
		if p.CurrentRevision == "" {
			generated, err := bigpicture.Generate(ctx, b.pictureChat, p, research)
			if err != nil {
				return nil, "", err
			}
			return &generated, bigpicture.RunPublished, nil
		}
		previous, err := m.BigPictureSnapshot(ctx, name, p.CurrentRevision)
		if err != nil {
			return nil, "", err
		}
		if bigpicture.Unchanged(p, previous, run.ResearchHash) {
			run.Message = "Collected evidence is unchanged; model call skipped."
			return nil, bigpicture.RunUnchanged, nil
		}
		documents, err := m.pictureDocuments(ctx, name, previous)
		if err != nil {
			return nil, "", err
		}
		pub, err := bigpicture.Update(ctx, b.pictureChat, p, research, previous, documents)
		if errors.Is(err, bigpicture.ErrNoChanges) {
			run.Message = "Model found no document changes for the new evidence."
			return nil, bigpicture.RunNoChanges, nil
		}
		if err != nil {
			return nil, "", err
		}
		return pub, bigpicture.RunPublished, nil
	}()
	if err != nil {
		return "", err
	}
	// Publication remains guarded if config/publication changed during research.
	current, err := m.BigPicture(ctx, name)
	if err != nil {
		return "", err
	}
	if current.StorageID != p.StorageID || current.Version != p.Version || current.CurrentRevision != p.CurrentRevision {
		return "", bigpicture.ErrConflict
	}
	if pub == nil {
		return status, m.repairPictureIndexes(current)
	}
	snapshot, err := m.PublishBigPicture(ctx, name, *pub)
	if err != nil {
		return "", err
	}
	run.Revision, run.Message = snapshot.ID, pub.ChangeSummary
	return status, nil
}

// pictureGenerationLive reports queued or running research for a workspace.
// Index tasks share the scope ID and must not suppress scheduled research.
func (m *Manager) pictureGenerationLive(name string) bool {
	if m.queue == nil {
		return false
	}
	id := bigpicture.ScopeKey(name)
	for _, item := range m.TaskSnapshot().Tasks {
		if item.ID == id && item.Kind == pictureTaskKind && (item.State == queue.StateQueued || item.State == queue.StateRunning) {
			return true
		}
	}
	return false
}

func (m *Manager) collectPictureResearch(ctx context.Context, p *bigpicture.Picture) (bigpicture.Research, error) {
	out := bigpicture.Research{Items: []bigpicture.ResearchItem{}, Notes: []string{"Deterministic bounded collection: 256 KiB total source text divided among sources, 16 KiB per raw item. Repositories contribute Krabby-generated documentation, communication signals from the code index and a dependency-graph summary first, then up to 6–8 raw files prioritizing documentation, deployment/configuration and entrypoints. This is not exhaustive research.", "External MCP tools and resource templates are not executed. Only saved exact resource URI grants are read. Secret values may exist in sources: no automatic redaction guarantee; use a trusted model endpoint."}}
	quota := (256 << 10) / max(1, len(p.Sources))
	for _, source := range p.Sources {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		before := len(out.Items)
		remaining := quota
		add := func(locator, revision, text string, truncated bool) {
			text = strings.ToValidUTF8(text, "")
			if len(text) > remaining {
				text = text[:remaining]
				for !utf8.ValidString(text) {
					text = text[:len(text)-1]
				}
				truncated = true
			}
			if strings.TrimSpace(text) == "" {
				return
			}
			remaining -= len(text)
			out.Items = append(out.Items, bigpicture.ResearchItem{ID: fmt.Sprintf("e%d", len(out.Items)+1), Evidence: bigpicture.Evidence{Source: source, Locator: locator, Revision: revision}, Content: text, Truncated: truncated})
		}
		if source.Kind == "mcp" {
			if m.externalMCPs == nil {
				return out, errors.New("external MCP store unavailable")
			}
			c, err := m.externalMCPs.Get(ctx, source.Ref)
			if err != nil || c.Disabled {
				return out, errors.New("selected external MCP is missing or disabled; existing publication kept")
			}
			for i, uri := range c.AllowedResources {
				if i >= 8 || remaining <= 0 {
					break
				}
				if strings.ContainsAny(uri, "{}") {
					continue
				}
				text, truncated, err := m.externalMCPs.ReadResource(ctx, source.Ref, uri, min(16<<10, remaining))
				if err != nil {
					return out, errors.New("selected MCP resource could not be read; existing publication kept")
				}
				add(uri, "", text, truncated)
			}
		} else {
			root, revision := "", ""
			rawLimit := 8
			switch source.Kind {
			case "repo":
				if m.reg == nil {
					return out, errors.New("repository store unavailable")
				}
				repo, err := m.reg.Get(ctx, source.Ref)
				if err != nil || repo == nil || repo.Path == "" {
					return out, errors.New("selected repository is not cloned; existing publication kept")
				}
				root, revision = repo.Path, repo.LastCommit
				// Derived evidence covers the whole repository; raw files then
				// fill the remaining budget with deployment/config detail.
				insights, notes := m.pictureRepoInsights(ctx, repo)
				for _, insight := range insights {
					if remaining <= 0 {
						break
					}
					add(insight.locator, revision, insight.text, insight.truncated)
				}
				out.Notes = append(out.Notes, notes...)
				if len(insights) > 0 {
					rawLimit = 6
				}
			case "web":
				if m.sourcesRootDir == "" {
					return out, errors.New("web source directory unavailable")
				}
				root = m.sourcesDir(source.Ref)
			case "api":
				if m.apisRootDir == "" {
					return out, errors.New("API source directory unavailable")
				}
				root = m.apisDir(source.Ref)
			default:
				return out, bigpicture.ErrInvalid
			}
			// The sandbox never follows symlinks outside this source. Limit the
			// inventory too; a partial listing is explicitly noted, not exhaustive.
			entries, err := repofs.ListFiles(root, "", true)
			if err != nil {
				return out, errors.New("selected source files unavailable; existing publication kept")
			}
			files := []string{}
			for _, entry := range entries {
				if !entry.IsDir && pictureFileRank(entry.Path, source.Kind) > 0 {
					files = append(files, entry.Path)
				}
			}
			slices.SortFunc(files, func(a, b string) int {
				if rank := pictureFileRank(b, source.Kind) - pictureFileRank(a, source.Kind); rank != 0 {
					return rank
				}
				return strings.Compare(a, b)
			})
			count := 0
			for _, file := range files {
				if count >= rawLimit || remaining <= 0 {
					break
				}
				if err := ctx.Err(); err != nil {
					return out, err
				}
				content, err := repofs.ReadFile(root, file, 0, min(16<<10, remaining))
				if err != nil {
					return out, errors.New("selected source file read failed; existing publication kept")
				}
				add(file, revision, content.Content, content.Truncated)
				count++
			}
			out.Notes = append(out.Notes, fmt.Sprintf("%s:%s: inspected a bounded inventory of %d entries, selected %d of %d eligible files. Web/API documents are cached snapshots, not live upstream reads.", source.Kind, source.Ref, len(entries), count, len(files)))
		}
		out.Notes = append(out.Notes, fmt.Sprintf("%s:%s: %d evidence items collected; uncollected content and runtime state remain unknown.", source.Kind, source.Ref, len(out.Items)-before))
	}
	if p.CurrentRevision != "" {
		snapshot, err := m.BigPictureSnapshot(ctx, p.Name, p.CurrentRevision)
		if err != nil {
			return out, err
		}
		budget := 64 << 10
		for _, meta := range snapshot.Documents {
			if budget <= 0 {
				break
			}
			if meta.Path == "research.md" {
				continue
			}
			doc, err := m.ReadBigPictureDocument(ctx, p.Name, p.CurrentRevision, meta.Path, 0, min(16<<10, budget))
			if err != nil {
				return out, err
			}
			out.Previous = append(out.Previous, bigpicture.Document{Path: meta.Path, Title: meta.Title, Markdown: doc.Content})
			budget -= doc.Bytes
		}
		out.Notes = append(out.Notes, "Previous text is bounded context (64 KiB total, 16 KiB/page), not current evidence. Untouched published pages are preserved in full; omitted/truncated context never implies deletion.")
	}
	return out, nil
}

func (m *Manager) pictureDocuments(ctx context.Context, name string, snapshot *bigpicture.Snapshot) ([]bigpicture.Document, error) {
	documents := make([]bigpicture.Document, 0, len(snapshot.Documents))
	for _, meta := range snapshot.Documents {
		doc := bigpicture.Document{Path: meta.Path, Title: meta.Title, Evidence: meta.Evidence}
		var offset int64
		for {
			part, err := m.ReadBigPictureDocument(ctx, name, snapshot.ID, meta.Path, offset, 128<<10)
			if err != nil {
				return nil, err
			}
			doc.Markdown += part.Content
			offset += int64(part.Bytes)
			if !part.Truncated {
				break
			}
			if part.Bytes == 0 {
				return nil, errors.New("previous document read made no progress")
			}
		}
		documents = append(documents, doc)
	}
	return documents, nil
}

func pictureFileRank(file, kind string) int {
	lower := strings.ToLower(filepath.ToSlash(file))
	base := filepath.Base(lower)
	if isPictureSecretPath(file) {
		return 0
	}
	if kind != "repo" {
		if strings.HasSuffix(lower, ".md") {
			return 100
		}
		return 0
	}
	if strings.HasPrefix(base, "readme") || base == "architecture.md" {
		return 100
	}
	if strings.Contains(lower, "deploy") || strings.Contains(lower, "helm") || strings.Contains(lower, "k8s") || strings.Contains(lower, "terraform") || strings.HasPrefix(base, "dockerfile") || strings.Contains(base, "compose") {
		return 90
	}
	switch filepath.Ext(lower) {
	case ".yaml", ".yml", ".toml", ".tf", ".proto":
		return 80
	case ".md":
		return 70
	case ".go", ".ts", ".js", ".py", ".java", ".rs", ".cs":
		if strings.Contains(lower, "main") || strings.Contains(lower, "config") || strings.Contains(lower, "kafka") || strings.Contains(lower, "event") || strings.Contains(lower, "route") || strings.Contains(lower, "client") {
			return 60
		}
		return 10
	}
	return 0
}
