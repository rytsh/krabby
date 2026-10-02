package manager

import (
	"strings"

	"github.com/rytsh/krabby/internal/config"
	"github.com/rytsh/krabby/internal/service/settings"
)

func docsConfig(s settings.Settings) config.Docs {
	return config.Docs{
		Enabled:      s.DocsEnabled,
		Concurrency:  s.DocsConcurrency,
		SummaryModel: s.DocsSummaryModel,
		MaxGroups:    s.DocsMaxGroups,
		Filters: config.Filters{
			Include:      s.DocsInclude,
			IncludeExtra: s.DocsIncludeExtra,
			Exclude:      s.DocsExclude,
		},
		Prompt:      s.DocsPrompt,
		PromptExtra: s.DocsPromptExtra,

		SkipIntegrationProfile: s.DocsSkipIntegrationProfile,
		Limits: config.DocsLimits{
			MaxSourceBytes:    s.DocsMaxSourceBytes,
			MaxGroupBytes:     s.DocsMaxGroupBytes,
			MaxSynthesisBytes: s.DocsMaxSynthesisBytes,
		},
	}
}

func llmConfig(s settings.Settings) config.LLM {
	return config.LLM{
		BaseURL: s.LLMBaseURL,
		APIKey:  s.LLMAPIKey,
		Model:   s.LLMModel,
		Timeout: s.LLMTimeout,
	}
}

// summaryLLMConfig returns the LLM config for the per-file summary phase. It
// reuses the main chat endpoint/credentials/timeout and only overrides the model
// with the configured (usually faster) summary model. When no summary model is
// set it falls back to the main model, so the returned client behaves like the
// synthesis client.
func summaryLLMConfig(s settings.Settings) config.LLM {
	cfg := llmConfig(s)
	if m := strings.TrimSpace(s.DocsSummaryModel); m != "" {
		cfg.Model = m
	}

	return cfg
}

func visionLLMConfig(s settings.Settings) config.LLM {
	cfg := llmConfig(s)
	if model := strings.TrimSpace(s.WebImageModel); model != "" {
		cfg.Model = model
	}
	return cfg
}

func webImageConfig(s settings.Settings) config.WebImage {
	return config.WebImage{
		AnalysisEnabled:    s.WebImageAnalysisEnabled,
		Model:              visionLLMConfig(s).Model,
		MaxPerPage:         s.EffectiveWebImageMaxPerPage(),
		MaxBytes:           s.EffectiveWebImageMaxBytes(),
		MaxPixels:          s.EffectiveWebImageMaxPixels(),
		AllowAuthenticated: s.WebImageAllowAuthenticated,
	}
}

func embedderConfig(s settings.Settings) config.Embedder {
	return config.Embedder{
		BaseURL:     s.EmbedBaseURL,
		APIKey:      s.EmbedAPIKey,
		Model:       s.EmbedModel,
		Dim:         s.EmbedDim,
		Batch:       s.EmbedBatch,
		InputMode:   s.EmbedInputMode,
		TaskMode:    s.EmbedTaskMode,
		Concurrency: s.EmbedConcurrency,
		Timeout:     s.EmbedTimeout,
	}
}

// codeEmbedderConfig returns the code embedder settings, falling back to the
// docs embedder when no dedicated code embedder base URL is configured.
func codeEmbedderConfig(s settings.Settings) config.Embedder {
	if s.CodeEmbedBaseURL == "" {
		return embedderConfig(s)
	}

	return config.Embedder{
		BaseURL:     s.CodeEmbedBaseURL,
		APIKey:      s.CodeEmbedAPIKey,
		Model:       s.CodeEmbedModel,
		Dim:         s.CodeEmbedDim,
		Batch:       s.CodeEmbedBatch,
		InputMode:   s.CodeEmbedInputMode,
		TaskMode:    s.CodeEmbedTaskMode,
		Concurrency: s.CodeEmbedConcurrency,
		Timeout:     s.CodeEmbedTimeout,
	}
}

// langfuseConfig maps the persisted observability settings onto the exporter's
// configuration carrier.
func langfuseConfig(s settings.Settings) config.Langfuse {
	return config.Langfuse{
		Enabled:         s.LangfuseEnabled,
		Host:            s.LangfuseHost,
		PublicKey:       s.LangfusePublicKey,
		SecretKey:       s.LangfuseSecretKey,
		Environment:     s.LangfuseEnvironment,
		Timeout:         s.LangfuseTimeout,
		Capture:         config.ParseCapture(s.LangfuseCapture),
		MaxContentBytes: s.LangfuseMaxContentBytes,
		TraceDocs:       s.LangfuseTraceDocs,
		TraceEmbed:      s.LangfuseTraceEmbed,
		TraceMCP:        s.LangfuseTraceMCP,
		TraceHTTP:       s.LangfuseTraceHTTP,
	}
}

func ragConfig(s settings.Settings) config.RAG {
	return config.RAG{
		Enabled:             s.RAGEnabled,
		KeepMarkdownTargets: s.RAGKeepMarkdownTargets,
		ChunkSize:           s.RAGChunkSize,
		ChunkOverlap:        s.RAGChunkOverlap,
		TopK:                s.RAGTopK,
		TopDocs:             s.RAGTopDocs,

		HybridCandidates:     s.RAGHybridCandidates,
		HybridRRFK:           s.RAGHybridRRFK,
		HybridWeightLexical:  s.RAGHybridWeightLexical,
		HybridWeightSemantic: s.RAGHybridWeightSemantic,
		LexicalStopWords:     s.RAGLexicalStopWords,
	}
}

func codeRagConfig(s settings.Settings) config.CodeRAG {
	return config.CodeRAG{
		Enabled:      s.CodeRAGEnabled,
		ChunkSize:    s.CodeRAGChunkSize,
		ChunkOverlap: s.CodeRAGChunkOverlap,
		TopK:         s.CodeRAGTopK,
		Filters: config.Filters{
			Include:      s.CodeRAGInclude,
			IncludeExtra: s.CodeRAGIncludeExtra,
			Exclude:      s.CodeRAGExclude,
		},
	}
}
