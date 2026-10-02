package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rytsh/krabby/internal/service/bigpicture"
	"github.com/rytsh/krabby/internal/service/mcpclient"
)

const (
	pictureLookupBytes    = 16 << 10
	pictureLookupMaterial = 384 << 10
)

// pictureLookups follows identifiers from the collected repository material
// into the selected MCP sources' granted tools, as the research prompt asks
// (for example "look these config paths up in Consul and Vault"). Results are
// appended as evidence of the MCP source. Failures only add notes.
func (m *Manager) pictureLookups(ctx context.Context, client bigpicture.Completer, cache bigpicture.NoteCache, p *bigpicture.Picture, research *bigpicture.Research, used map[string]bool) {
	if m.externalMCPs == nil {
		return
	}
	servers := map[string]bool{}
	for _, source := range p.Sources {
		if source.Kind == "mcp" {
			servers[source.Ref] = true
		}
	}
	if len(servers) == 0 {
		return
	}
	var tools []bigpicture.LookupTool
	granted := map[string]map[string]bool{}
	catalog := map[string][]string{}
	for server := range servers {
		c, err := m.externalMCPs.Get(ctx, server)
		if err != nil || c.Disabled || len(c.AllowedTools) == 0 {
			continue
		}
		discovery := mcpclient.Discover(ctx, c)
		if !discovery.OK {
			research.Notes = append(research.Notes, fmt.Sprintf("mcp:%s: catalog unavailable; lookups skipped.", server))
			continue
		}
		granted[server] = map[string]bool{}
		for _, tool := range discovery.Tools {
			if !containsString(c.AllowedTools, tool.Name) {
				continue
			}
			granted[server][tool.Name] = true
			schema, _ := json.Marshal(tool.InputSchema)
			description := tool.Description
			if len(description) > 1024 {
				description = description[:1024]
			}
			tools = append(tools, bigpicture.LookupTool{Server: server, Name: tool.Name, Description: description, InputSchema: schema})
		}
		for _, resource := range discovery.Resources {
			if len(catalog[server]) >= 100 {
				break
			}
			for _, label := range []string{resource.Name, resource.Title} {
				if label = strings.TrimSpace(label); label != "" && len(label) <= 128 {
					catalog[server] = append(catalog[server], label)
				}
			}
		}
	}
	if len(tools) == 0 {
		return
	}

	var material strings.Builder
	for _, item := range research.Items {
		if item.Evidence.Source.Kind == "mcp" {
			continue
		}
		fmt.Fprintf(&material, "### %s:%s %s\n%s\n\n", item.Evidence.Source.Kind, item.Evidence.Source.Ref, item.Evidence.Locator, item.Content)
		if material.Len() >= pictureLookupMaterial {
			break
		}
	}
	text := material.String()
	if len(text) > pictureLookupMaterial {
		text = text[:pictureLookupMaterial]
	}

	calls, key, err := bigpicture.PlanLookups(ctx, client, cache, p, tools, catalog, text, bigpicture.MaxLookups)
	if key != "" {
		used[key] = true
	}
	if err != nil {
		research.Notes = append(research.Notes, "MCP lookups skipped: "+err.Error())
		return
	}
	byServer := map[string][]bigpicture.LookupCall{}
	skipped := 0
	for _, call := range calls {
		if reason := bigpicture.ValidateLookup(call, granted, catalog, text); reason != "" {
			skipped++
			research.Notes = append(research.Notes, fmt.Sprintf("mcp:%s: lookup %s rejected: %s.", call.Server, call.Tool, reason))
			continue
		}
		byServer[call.Server] = append(byServer[call.Server], call)
	}
	ran := 0
	for server, planned := range byServer {
		requests := make([]mcpclient.ToolCall, len(planned))
		for i, call := range planned {
			requests[i] = mcpclient.ToolCall{Tool: call.Tool, Arguments: call.Arguments}
		}
		results, err := m.externalMCPs.CallTools(ctx, server, requests, pictureLookupBytes)
		if err != nil {
			research.Notes = append(research.Notes, fmt.Sprintf("mcp:%s: lookups failed: %s.", server, err))
			continue
		}
		for i, result := range results {
			locator := lookupLocator(planned[i])
			if result.Err != nil {
				research.Notes = append(research.Notes, fmt.Sprintf("mcp:%s: %s: %s.", server, locator, result.Err))
				continue
			}
			content := bigpicture.RedactSecrets(result.Text)
			if strings.TrimSpace(content) == "" {
				continue
			}
			if planned[i].Reason != "" {
				content = "Lookup reason: " + planned[i].Reason + "\n\n" + content
			}
			research.Items = append(research.Items, bigpicture.ResearchItem{
				ID:       fmt.Sprintf("e%d", len(research.Items)+1),
				Evidence: bigpicture.Evidence{Source: bigpicture.Source{Kind: "mcp", Ref: server}, Locator: locator},
				Content:  content, Truncated: result.Truncated,
			})
			ran++
		}
	}
	research.Notes = append(research.Notes, fmt.Sprintf("MCP lookups: the model planned %d tool calls from the research prompt and repository material; %d returned evidence, %d were rejected because a tool was not granted or an argument did not appear in the repository material. Credential-looking values are masked (best effort).", len(calls), ran, skipped))
	slog.Info("big picture mcp lookups", "picture", p.Name, "planned", len(calls), "evidence", ran, "rejected", skipped)
}

func lookupLocator(call bigpicture.LookupCall) string {
	args, _ := json.Marshal(call.Arguments)
	locator := "tool:" + call.Tool + string(args)
	if len(locator) > 512 {
		locator = locator[:512]
	}
	return locator
}

func containsString(values []string, v string) bool {
	for _, value := range values {
		if value == v {
			return true
		}
	}
	return false
}
