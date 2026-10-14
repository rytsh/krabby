// Package chat implements the "chat" web-source type: channel conversations
// from Slack, Mattermost or Microsoft Teams. Each thread (a root message and
// its replies) becomes one document, so an answer is retrieved with the
// question that prompted it.
//
// Privacy is opt-in by construction: only explicitly listed channels are read,
// a look-back window bounds history, and bot/system messages and messages
// matching exclude patterns are dropped.
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/progress"
	"github.com/rytsh/krabby/internal/service/websource"
)

// Type is the collection type served by this fetcher.
const Type = "chat"

// Providers.
const (
	Slack      = "slack"
	Mattermost = "mattermost"
	Teams      = "teams"
)

const defaultDays = 90

// Config is the provider config.
type Config struct {
	Provider       string   `json:"provider"`
	BaseURL        string   `json:"base_url,omitempty"`
	Token          string   `json:"token,omitempty"`
	Channels       []string `json:"channels"`
	Team           string   `json:"team,omitempty"`
	Days           int      `json:"days,omitempty"`
	MinReplies     int      `json:"min_replies,omitempty"`
	IncludeBots    bool     `json:"include_bots,omitempty"`
	ExcludePattern string   `json:"exclude_pattern,omitempty"`
	RedactUsers    bool     `json:"redact_users,omitempty"`
}

func (c Config) normalize() Config {
	c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.Days <= 0 {
		c.Days = defaultDays
	}

	return c
}

// Message is one normalized chat message.
type Message struct {
	ID       string
	ThreadID string // empty for a root message
	User     string
	Text     string
	At       time.Time
	Bot      bool
	Link     string
}

type adapter interface {
	// messages returns the channel's messages since oldest, replies included.
	messages(ctx context.Context, channel string, oldest time.Time) (name string, msgs []Message, err error)
}

// Fetcher syncs chat channels.
type Fetcher struct {
	websource.Schema
	client *http.Client
}

// New creates the fetcher.
func New() *Fetcher {
	return &Fetcher{client: websource.DefaultHTTPClient(), Schema: websource.Schema{Descriptor: websource.Descriptor{
		Type:  Type,
		Label: "Chat channels (Slack, Mattermost, Teams)",
		Description: "Index threads of explicitly listed channels within a look-back window; each thread is one document. " +
			"Bot messages and messages matching an exclude pattern are dropped; user names can be redacted.",
		Fields: []websource.Field{
			{Key: "provider", Label: "Provider", Kind: websource.FieldSelect, Required: true, Options: []string{Slack, Mattermost, Teams}, Default: Slack},
			{Key: "base_url", Label: "Server URL (Mattermost)", Kind: websource.FieldString, Placeholder: "https://chat.example.com"},
			{Key: "token", Label: "Token", Kind: websource.FieldSecret, Required: true,
				Help: "Slack bot token (channels:history, users:read), Mattermost personal/bot token, Teams Graph token (ChannelMessage.Read.All)"},
			{Key: "team", Label: "Teams team id", Kind: websource.FieldString, Help: "Microsoft Teams only"},
			{Key: "channels", Label: "Channel ids", Kind: websource.FieldList, Required: true, Placeholder: "C0123ABC, C0456DEF"},
			{Key: "days", Label: "Look-back window (days)", Kind: websource.FieldNumber, Default: defaultDays},
			{Key: "min_replies", Label: "Only threads with at least N replies", Kind: websource.FieldNumber},
			{Key: "include_bots", Label: "Include bot messages", Kind: websource.FieldBool},
			{Key: "exclude_pattern", Label: "Drop messages matching (regexp)", Kind: websource.FieldString, Placeholder: "(?i)password|token"},
			{Key: "redact_users", Label: "Redact user names", Kind: websource.FieldBool},
		},
	}}}
}

func decode(raw json.RawMessage) (Config, error) {
	cfg, err := websource.DecodeConfig[Config](raw, Type)

	return cfg.normalize(), err
}

func (f *Fetcher) Validate(raw json.RawMessage) error {
	cfg, err := decode(raw)
	if err != nil {
		return err
	}
	switch cfg.Provider {
	case Slack:
	case Mattermost:
		if err := websource.ValidHTTPURL(cfg.BaseURL); err != nil {
			return fmt.Errorf("mattermost base_url %w", err)
		}
	case Teams:
		if cfg.Team == "" {
			return fmt.Errorf("teams needs the team id")
		}
	default:
		return fmt.Errorf("chat provider must be slack, mattermost or teams")
	}
	if len(cfg.Channels) == 0 {
		return fmt.Errorf("list at least one channel id")
	}
	if cfg.Days > 3650 {
		return fmt.Errorf("days must be at most 3650")
	}
	if cfg.ExcludePattern != "" {
		if _, err := regexp.Compile(cfg.ExcludePattern); err != nil {
			return fmt.Errorf("exclude_pattern; %w", err)
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

func (f *Fetcher) adapter(cfg Config) adapter {
	switch cfg.Provider {
	case Mattermost:
		return &mattermost{client: f.client, cfg: cfg}
	case Teams:
		return &teams{client: f.client, cfg: cfg}
	default:
		return &slack{client: f.client, cfg: cfg, users: map[string]string{}}
	}
}

// Thread is a root message with its replies.
type Thread struct {
	Channel  string
	Root     Message
	Replies  []Message
	LastSeen time.Time
}

// group builds threads from a flat message list, dropping filtered messages.
func group(cfg Config, channel string, msgs []Message) []*Thread {
	var exclude *regexp.Regexp
	if cfg.ExcludePattern != "" {
		exclude = regexp.MustCompile(cfg.ExcludePattern)
	}
	keep := func(m Message) bool {
		if m.Bot && !cfg.IncludeBots {
			return false
		}
		return strings.TrimSpace(m.Text) != "" && (exclude == nil || !exclude.MatchString(m.Text))
	}

	threads := map[string]*Thread{}
	var order []string
	for _, m := range msgs {
		if m.ThreadID == "" || m.ThreadID == m.ID {
			if !keep(m) {
				continue
			}
			t := threads[m.ID]
			if t == nil {
				t = &Thread{Channel: channel}
				threads[m.ID] = t
				order = append(order, m.ID)
			}
			t.Root = m
			t.LastSeen = maxTime(t.LastSeen, m.At)
		}
	}
	for _, m := range msgs {
		if m.ThreadID == "" || m.ThreadID == m.ID || !keep(m) {
			continue
		}
		t := threads[m.ThreadID]
		if t == nil {
			continue // reply to a root outside the window or filtered
		}
		t.Replies = append(t.Replies, m)
		t.LastSeen = maxTime(t.LastSeen, m.At)
	}

	out := make([]*Thread, 0, len(order))
	for _, id := range order {
		t := threads[id]
		if len(t.Replies) < cfg.MinReplies {
			continue
		}
		sort.Slice(t.Replies, func(i, j int) bool { return t.Replies[i].At.Before(t.Replies[j].At) })
		out = append(out, t)
	}

	return out
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}

	return a
}

func (f *Fetcher) Preview(ctx context.Context, raw json.RawMessage) (websource.PreviewResult, error) {
	var out websource.PreviewResult
	if err := f.Validate(raw); err != nil {
		return out, err
	}
	cfg, _ := decode(raw)
	ad := f.adapter(cfg)
	oldest := time.Now().AddDate(0, 0, -cfg.Days)
	for _, ch := range cfg.Channels {
		_, msgs, err := ad.messages(ctx, strings.TrimSpace(ch), oldest)
		if err != nil {
			return out, fmt.Errorf("channel %s: %w", ch, err)
		}
		out.Scanned += len(msgs)
		out.ItemCount += len(group(cfg, ch, msgs))
	}

	return out, nil
}

// Fetch re-reads the look-back window of every channel. The window is the
// source of truth, so the sweep is complete and threads that aged out (or
// channels that were removed from the config) are pruned.
func (f *Fetcher) Fetch(ctx context.Context, col *websource.Collection, _ []*websource.Page, _ json.RawMessage, emit websource.Emit) (*websource.FetchResult, error) {
	cfg, err := decode(col.Config)
	if err != nil {
		return nil, err
	}
	ad := f.adapter(cfg)
	oldest := time.Now().AddDate(0, 0, -cfg.Days)
	for i, ch := range cfg.Channels {
		ch = strings.TrimSpace(ch)
		name, msgs, err := ad.messages(ctx, ch, oldest)
		if err != nil {
			return nil, fmt.Errorf("channel %s: %w", ch, err)
		}
		for _, t := range group(cfg, ch, msgs) {
			if err := emit(render(cfg, firstNonEmpty(name, ch), t)); err != nil {
				return nil, err
			}
		}
		progress.Report(ctx, i+1, len(cfg.Channels))
	}

	return &websource.FetchResult{Complete: true}, nil
}

func render(cfg Config, channel string, t *Thread) websource.RemotePage {
	who := func(m Message) string {
		if cfg.RedactUsers {
			return "user-" + websource.Hash(m.User)[:6]
		}
		return firstNonEmpty(m.User, "unknown")
	}
	title := oneLine(t.Root.Text, 90)
	var b strings.Builder
	fmt.Fprintf(&b, "# #%s: %s\n\n", channel, title)
	fmt.Fprintf(&b, "- **Channel**: #%s\n- **Started**: %s\n- **Replies**: %d\n\n", channel, t.Root.At.UTC().Format("2006-01-02 15:04"), len(t.Replies))
	for _, m := range append([]Message{t.Root}, t.Replies...) {
		fmt.Fprintf(&b, "**%s** (%s):\n%s\n\n", who(m), m.At.UTC().Format("2006-01-02 15:04"), strings.TrimSpace(m.Text))
	}

	return websource.RemotePage{
		Slug:      websource.ListingSlug(t.Channel + "/" + t.Root.ID),
		Title:     "#" + channel + ": " + title,
		URL:       t.Root.Link,
		UpdatedAt: t.LastSeen,
		Teams:     []string{channel},
		Markdown:  b.String(),
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:n]) + "…"
	}

	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}

	return ""
}
