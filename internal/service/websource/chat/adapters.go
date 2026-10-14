package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rytsh/krabby/internal/service/websource"
)

// maxMessages bounds one channel's window so a busy channel cannot exhaust
// memory; the newest messages are kept.
const maxMessages = 50_000

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// ---- Slack ------------------------------------------------------------------

type slack struct {
	client *http.Client
	cfg    Config
	users  map[string]string
	base   string // tests override the API root
}

type slackMsg struct {
	TS         string `json:"ts"`
	ThreadTS   string `json:"thread_ts"`
	User       string `json:"user"`
	Text       string `json:"text"`
	BotID      string `json:"bot_id"`
	Subtype    string `json:"subtype"`
	ReplyCount int    `json:"reply_count"`
}

type slackPage struct {
	OK       bool       `json:"ok"`
	Error    string     `json:"error"`
	Messages []slackMsg `json:"messages"`
	Meta     struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
	HasMore bool `json:"has_more"`
}

func (s *slack) api() string {
	if s.base != "" {
		return s.base
	}

	return "https://slack.com/api"
}

func slackTime(ts string) time.Time {
	sec, frac, _ := strings.Cut(ts, ".")
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Time{}
	}
	us, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)

	return time.Unix(n, us*1000)
}

func (s *slack) call(ctx context.Context, method string, q url.Values, out *slackPage) error {
	if _, err := websource.GetJSON(ctx, s.client, s.api()+"/"+method+"?"+q.Encode(), bearer(s.cfg.Token), out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("slack %s: %s", method, out.Error)
	}

	return nil
}

func (s *slack) userName(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	if name, ok := s.users[id]; ok {
		return name
	}
	var res struct {
		OK   bool `json:"ok"`
		User struct {
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	name := id
	if _, err := websource.GetJSON(ctx, s.client, s.api()+"/users.info?user="+url.QueryEscape(id), bearer(s.cfg.Token), &res); err == nil && res.OK {
		name = firstNonEmpty(res.User.Profile.DisplayName, res.User.Profile.RealName, res.User.Name, id)
	}
	s.users[id] = name

	return name
}

func (s *slack) convert(ctx context.Context, channel string, m slackMsg) Message {
	thread := m.ThreadTS
	if thread == m.TS {
		thread = ""
	}

	return Message{
		ID: m.TS, ThreadID: thread, User: s.userName(ctx, m.User), Text: m.Text, At: slackTime(m.TS),
		Bot: m.BotID != "" || m.Subtype == "bot_message" || (m.Subtype != "" && m.Subtype != "thread_broadcast"),
	}
}

func (s *slack) messages(ctx context.Context, channel string, oldest time.Time) (string, []Message, error) {
	var out []Message
	var roots []slackMsg
	cursor := ""
	for {
		q := url.Values{"channel": {channel}, "limit": {"200"}, "oldest": {strconv.FormatInt(oldest.Unix(), 10)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page slackPage
		if err := s.call(ctx, "conversations.history", q, &page); err != nil {
			return "", nil, err
		}
		for _, m := range page.Messages {
			out = append(out, s.convert(ctx, channel, m))
			if m.ReplyCount > 0 {
				roots = append(roots, m)
			}
		}
		cursor = page.Meta.NextCursor
		if cursor == "" || !page.HasMore || len(out) >= maxMessages {
			break
		}
	}
	for _, root := range roots {
		cursor := ""
		for {
			q := url.Values{"channel": {channel}, "ts": {root.TS}, "limit": {"200"}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			var page slackPage
			if err := s.call(ctx, "conversations.replies", q, &page); err != nil {
				return "", nil, err
			}
			for _, m := range page.Messages {
				if m.TS != root.TS {
					out = append(out, s.convert(ctx, channel, m))
				}
			}
			cursor = page.Meta.NextCursor
			if cursor == "" || !page.HasMore {
				break
			}
		}
	}

	var info struct {
		OK      bool `json:"ok"`
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
	}
	name := ""
	if _, err := websource.GetJSON(ctx, s.client, s.api()+"/conversations.info?channel="+url.QueryEscape(channel), bearer(s.cfg.Token), &info); err == nil && info.OK {
		name = info.Channel.Name
	}

	return name, out, nil
}

// ---- Mattermost ---------------------------------------------------------------

type mattermost struct {
	client *http.Client
	cfg    Config
}

func (m *mattermost) messages(ctx context.Context, channel string, oldest time.Time) (string, []Message, error) {
	api := m.cfg.BaseURL + "/api/v4"
	var ch struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		TeamID      string `json:"team_id"`
	}
	if _, err := websource.GetJSON(ctx, m.client, api+"/channels/"+url.PathEscape(channel), bearer(m.cfg.Token), &ch); err != nil {
		return "", nil, err
	}
	users := map[string]string{}
	var out []Message
	for page := 0; page < 1000 && len(out) < maxMessages; page++ {
		q := url.Values{"since": {strconv.FormatInt(oldest.UnixMilli(), 10)}, "page": {strconv.Itoa(page)}, "per_page": {"200"}}
		var res struct {
			Order []string `json:"order"`
			Posts map[string]struct {
				ID       string         `json:"id"`
				RootID   string         `json:"root_id"`
				UserID   string         `json:"user_id"`
				Message  string         `json:"message"`
				CreateAt int64          `json:"create_at"`
				DeleteAt int64          `json:"delete_at"`
				Type     string         `json:"type"`
				Props    map[string]any `json:"props"`
			} `json:"posts"`
		}
		if _, err := websource.GetJSON(ctx, m.client, api+"/channels/"+url.PathEscape(channel)+"/posts?"+q.Encode(), bearer(m.cfg.Token), &res); err != nil {
			return "", nil, err
		}
		for _, id := range res.Order {
			p := res.Posts[id]
			if p.DeleteAt != 0 {
				continue
			}
			name, ok := users[p.UserID]
			if !ok {
				var u struct {
					Username string `json:"username"`
				}
				name = p.UserID
				if _, err := websource.GetJSON(ctx, m.client, api+"/users/"+url.PathEscape(p.UserID), bearer(m.cfg.Token), &u); err == nil && u.Username != "" {
					name = u.Username
				}
				users[p.UserID] = name
			}
			_, fromBot := p.Props["from_bot"]
			out = append(out, Message{
				ID: p.ID, ThreadID: p.RootID, User: name, Text: p.Message, At: time.UnixMilli(p.CreateAt),
				Bot:  fromBot || p.Type != "",
				Link: m.cfg.BaseURL + "/_redirect/pl/" + p.ID,
			})
		}
		if len(res.Order) < 200 {
			break
		}
	}

	return firstNonEmpty(ch.Name, ch.DisplayName), out, nil
}

// ---- Microsoft Teams (Graph) ---------------------------------------------------

type teams struct {
	client *http.Client
	cfg    Config
	base   string
}

type graphMessage struct {
	ID              string    `json:"id"`
	ReplyToID       string    `json:"replyToId"`
	CreatedDateTime time.Time `json:"createdDateTime"`
	MessageType     string    `json:"messageType"`
	DeletedDateTime *string   `json:"deletedDateTime"`
	WebURL          string    `json:"webUrl"`
	From            *struct {
		User *struct {
			DisplayName string `json:"displayName"`
		} `json:"user"`
		Application *struct {
			DisplayName string `json:"displayName"`
		} `json:"application"`
	} `json:"from"`
	Body struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	Replies []graphMessage `json:"replies"`
}

func (t *teams) api() string {
	if t.base != "" {
		return t.base
	}

	return "https://graph.microsoft.com/v1.0"
}

func (t *teams) toMessage(g graphMessage, thread string) Message {
	m := Message{ID: g.ID, ThreadID: thread, At: g.CreatedDateTime, Link: g.WebURL, Text: g.Body.Content}
	if g.Body.ContentType == "html" {
		if md, err := websource.MarkdownFromHTML(g.Body.Content, ""); err == nil {
			m.Text = md
		}
	}
	if g.From != nil && g.From.User != nil {
		m.User = g.From.User.DisplayName
	} else if g.From != nil && g.From.Application != nil {
		m.User, m.Bot = g.From.Application.DisplayName, true
	}
	if g.MessageType != "" && g.MessageType != "message" || g.DeletedDateTime != nil {
		m.Bot = true
	}

	return m
}

func (t *teams) messages(ctx context.Context, channel string, oldest time.Time) (string, []Message, error) {
	base := t.api() + "/teams/" + url.PathEscape(t.cfg.Team) + "/channels/" + url.PathEscape(channel)
	var info struct {
		DisplayName string `json:"displayName"`
	}
	_, _ = websource.GetJSON(ctx, t.client, base, bearer(t.cfg.Token), &info)

	var out []Message
	next := base + "/messages?$top=50&$expand=replies"
	for page := 0; next != "" && page < 2000 && len(out) < maxMessages; page++ {
		var res struct {
			Value []graphMessage `json:"value"`
			Next  string         `json:"@odata.nextLink"`
		}
		if _, err := websource.GetJSON(ctx, t.client, next, bearer(t.cfg.Token), &res); err != nil {
			return "", nil, err
		}
		olderPage := true
		for _, g := range res.Value {
			lastActivity := g.CreatedDateTime
			for _, r := range g.Replies {
				lastActivity = maxTime(lastActivity, r.CreatedDateTime)
			}
			if lastActivity.Before(oldest) {
				continue
			}
			olderPage = false
			out = append(out, t.toMessage(g, ""))
			for _, r := range g.Replies {
				out = append(out, t.toMessage(r, g.ID))
			}
		}
		// Graph returns newest threads first; a page entirely outside the
		// window means every later page is too.
		if olderPage && len(res.Value) > 0 {
			break
		}
		next = res.Next
	}

	return info.DisplayName, out, nil
}
