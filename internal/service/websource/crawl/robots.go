package crawl

import (
	"regexp"
	"strings"
)

// robots is the subset of robots.txt (RFC 9309) a crawler needs: the
// Allow/Disallow rules of the group matching our user agent (or "*"), with
// longest-match precedence and "*" / "$" wildcards.
type robots struct {
	rules []robotsRule
}

type robotsRule struct {
	allow   bool
	pattern string
	re      *regexp.Regexp
}

func parseRobots(body, userAgent string) *robots {
	ua := strings.ToLower(userAgent)
	if i := strings.IndexAny(ua, "/ "); i > 0 {
		ua = ua[:i]
	}

	type group struct {
		agents []string
		rules  []robotsRule
	}
	var groups []*group
	var cur *group
	lastWasAgent := false

	for line := range strings.SplitSeq(body, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "user-agent":
			if cur == nil || !lastWasAgent {
				cur = &group{}
				groups = append(groups, cur)
			}
			cur.agents = append(cur.agents, strings.ToLower(value))
			lastWasAgent = true
		case "allow", "disallow":
			lastWasAgent = false
			if cur == nil || value == "" && key == "disallow" {
				continue
			}
			cur.rules = append(cur.rules, robotsRule{allow: key == "allow", pattern: value, re: compileRobots(value)})
		default:
			lastWasAgent = false
		}
	}

	var star, specific *group
	for _, g := range groups {
		for _, a := range g.agents {
			if a == "*" && star == nil {
				star = g
			} else if a != "*" && strings.Contains(ua, a) && specific == nil {
				specific = g
			}
		}
	}
	chosen := specific
	if chosen == nil {
		chosen = star
	}
	if chosen == nil {
		return &robots{}
	}

	return &robots{rules: chosen.rules}
}

func (r *robots) allowed(p string) bool {
	if r == nil || len(r.rules) == 0 {
		return true
	}
	if p == "" {
		p = "/"
	}
	best := -1
	allow := true
	for _, rule := range r.rules {
		if !rule.re.MatchString(p) {
			continue
		}
		l := len(rule.pattern)
		if l > best || l == best && rule.allow {
			best = l
			allow = rule.allow
		}
	}

	return allow
}

// compileRobots turns a robots path pattern with "*" (any run) and a
// trailing "$" (end anchor) into a prefix regexp.
func compileRobots(pattern string) *regexp.Regexp {
	anchored := strings.HasSuffix(pattern, "$")
	parts := strings.Split(strings.TrimSuffix(pattern, "$"), "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	expr := "^" + strings.Join(parts, ".*")
	if anchored {
		expr += "$"
	}

	return regexp.MustCompile(expr)
}
