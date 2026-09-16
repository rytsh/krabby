package main

import (
	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/settings"
)

// readerSettings changes connections only, in memory. In particular a new
// endpoint cannot silently change the model/dimension of a replicated index.
func readerSettings(s settings.Settings, q config.QueryConnections) settings.Settings {
	if q.CodeEmbedder.BaseURL != "" && s.CodeEmbedBaseURL == "" {
		s.CodeEmbedBaseURL, s.CodeEmbedAPIKey = s.EmbedBaseURL, s.EmbedAPIKey
		s.CodeEmbedModel, s.CodeEmbedDim = s.EmbedModel, s.EmbedDim
		s.CodeEmbedBatch, s.CodeEmbedConcurrency, s.CodeEmbedTimeout = s.EmbedBatch, s.EmbedConcurrency, s.EmbedTimeout
	}
	if q.DocsEmbedder.BaseURL != "" {
		s.EmbedBaseURL = q.DocsEmbedder.BaseURL
	}
	if q.DocsEmbedder.APIKey != "" {
		s.EmbedAPIKey = q.DocsEmbedder.APIKey
	}
	if q.CodeEmbedder.BaseURL != "" {
		s.CodeEmbedBaseURL = q.CodeEmbedder.BaseURL
	}
	if q.CodeEmbedder.APIKey != "" {
		if s.CodeEmbedBaseURL == "" {
			// An inherited code connection follows the docs connection. A separate
			// code key needs an explicit endpoint to avoid changing docs auth.
			s.CodeEmbedBaseURL, s.CodeEmbedModel, s.CodeEmbedDim = s.EmbedBaseURL, s.EmbedModel, s.EmbedDim
			s.CodeEmbedBatch, s.CodeEmbedConcurrency, s.CodeEmbedTimeout = s.EmbedBatch, s.EmbedConcurrency, s.EmbedTimeout
		}
		s.CodeEmbedAPIKey = q.CodeEmbedder.APIKey
	}
	return s
}
