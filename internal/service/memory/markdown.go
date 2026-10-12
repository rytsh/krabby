package memory

import (
	"fmt"
	"strings"
	"time"
)

// Markdown renders a memory as a self-contained document: metadata, the
// Markdown body, pinned evidence and the event history.
func Markdown(m *Memory) string { return markdown(m, "#") }

func markdown(m *Memory, h string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n\n", h, m.Title)
	fmt.Fprintf(&b, "| | |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| ID | `%s` |\n", m.ID)
	fmt.Fprintf(&b, "| Space | `%s` |\n", m.Space)
	fmt.Fprintf(&b, "| Kind | %s |\n", m.Kind)
	fmt.Fprintf(&b, "| Status | %s |\n", m.Status)
	if len(m.Tags) > 0 {
		fmt.Fprintf(&b, "| Tags | %s |\n", strings.Join(m.Tags, ", "))
	}
	if m.Author != "" {
		fmt.Fprintf(&b, "| Author | %s |\n", escapeCell(m.Author))
	}
	fmt.Fprintf(&b, "| Created | %s |\n", stamp(m.CreatedAt))
	fmt.Fprintf(&b, "| Updated | %s (v%d) |\n", stamp(m.UpdatedAt), m.Version)
	if !m.ExpiresAt.IsZero() {
		fmt.Fprintf(&b, "| Expires | %s |\n", stamp(m.ExpiresAt))
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(m.Content))
	b.WriteString("\n")
	if len(m.Evidence) > 0 {
		fmt.Fprintf(&b, "\n%s# Evidence\n\n", h)
		for _, e := range m.Evidence {
			fmt.Fprintf(&b, "- `%s` — `%s`", e.Repo, e.Path)
			if e.StartLine > 0 {
				fmt.Fprintf(&b, " L%d-%d", e.StartLine, e.EndLine)
			}
			if e.Symbol != "" {
				fmt.Fprintf(&b, " (`%s`)", e.Symbol)
			}
			if e.Commit != "" {
				fmt.Fprintf(&b, " @ `%s`", shortSHA(e.Commit))
			}
			if e.Status != "" {
				fmt.Fprintf(&b, " — %s", e.Status)
			}
			b.WriteString("\n")
		}
	}
	if len(m.History) > 0 {
		fmt.Fprintf(&b, "\n%s# History\n\n", h)
		for _, ev := range m.History {
			fmt.Fprintf(&b, "- %s — **%s**", stamp(ev.At), ev.Action)
			if ev.Actor != "" {
				fmt.Fprintf(&b, " by %s", ev.Actor)
			}
			if ev.Note != "" {
				fmt.Fprintf(&b, ": %s", strings.ReplaceAll(ev.Note, "\n", " "))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// MarkdownSpace renders several memories as one document, e.g. an export of a
// whole space.
func MarkdownSpace(title string, memories []*Memory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\nExported %s · %d memories\n", title, stamp(time.Now()), len(memories))
	for _, m := range memories {
		b.WriteString("\n---\n\n")
		b.WriteString(markdown(m, "##"))
	}
	return b.String()
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

func escapeCell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
