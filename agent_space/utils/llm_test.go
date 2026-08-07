package utils

import (
	"strings"
	"testing"
)

// clearLLMEnv blanks every variable the resolvers read, so each case starts
// from a known state regardless of the developer's real .env.
func clearLLMEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OPENAI_BASE_URL",
		"OPENROUTER_API_KEY", "OPENAI_API_KEY", "LLM_API_KEY",
		"OPENROUTER_MODEL", "LLM_MODEL", "OPENAI_MODEL",
		"OPENROUTER_EMBEDDING_MODEL", "LLM_EMBEDDING_MODEL", "OPENAI_EMBEDDING_MODEL",
		"OPENROUTER_EMBEDDING_DIMENSIONS", "LLM_EMBEDDING_DIMENSIONS", "OPENAI_EMBEDDING_DIMENSIONS",
		"VECTOR_DIMENSIONS",
	} {
		t.Setenv(key, "")
	}
	// The warn-once state is package-global; reset it so warnings are not
	// suppressed across cases.
	warnOnce.Clear()
}

// openRouterEnv mimics the committed .env: a fully configured OpenRouter setup.
// Every self-hosted case starts from this, because the whole point is that a
// leftover OpenRouter configuration must not leak into local requests.
func openRouterEnv(t *testing.T) {
	t.Helper()
	clearLLMEnv(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-secret")
	t.Setenv("OPENROUTER_MODEL", "z-ai/glm-5.2")
	t.Setenv("OPENROUTER_EMBEDDING_MODEL", "qwen/qwen3-embedding-8b")
	t.Setenv("OPENROUTER_EMBEDDING_DIMENSIONS", "1024")
}

func TestSelfHostedDetection(t *testing.T) {
	clearLLMEnv(t)
	if SelfHosted() {
		t.Error("SelfHosted() = true with no OPENAI_BASE_URL")
	}
	if got := BaseURL(); got != openRouterBaseURL {
		t.Errorf("BaseURL() = %q, want %q", got, openRouterBaseURL)
	}

	t.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")
	if !SelfHosted() {
		t.Error("SelfHosted() = false with OPENAI_BASE_URL set")
	}
	if got := BaseURL(); got != "http://localhost:11434/v1" {
		t.Errorf("BaseURL() = %q, want the self-hosted override", got)
	}
}

// This is the whole point of the change: a leftover OpenRouter configuration
// must not reach a self-hosted endpoint. Sending "z-ai/glm-5.2" to Ollama is a
// confusing 404, and sending the real OpenRouter key to an arbitrary local
// process leaks a live credential.
func TestSelfHostedOverridesOpenRouterSettings(t *testing.T) {
	openRouterEnv(t)
	t.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")
	t.Setenv("LLM_MODEL", "qwen2.5-coder:32b")
	t.Setenv("LLM_EMBEDDING_MODEL", "nomic-embed-text")

	if got := ChatModel(); got != "qwen2.5-coder:32b" {
		t.Errorf("ChatModel() = %q, want the local model to win", got)
	}
	if got := EmbeddingModel(); got != "nomic-embed-text" {
		t.Errorf("EmbeddingModel() = %q, want the local model to win", got)
	}

	key, err := apiKey()
	if err != nil {
		t.Fatalf("apiKey: %v", err)
	}
	if key == "sk-or-secret" {
		t.Error("the OpenRouter key was sent to a self-hosted endpoint")
	}

	// OPENROUTER_EMBEDDING_DIMENSIONS describes OpenRouter, so it must not
	// decide what a local server receives.
	if got := EmbeddingDimensions(); got != 0 {
		t.Errorf("EmbeddingDimensions() = %d, want 0 so the field is omitted", got)
	}
}

func TestSelfHostedDefaultsWithoutModelNames(t *testing.T) {
	openRouterEnv(t)
	t.Setenv("OPENAI_BASE_URL", "http://localhost:8080/v1")

	// No LLM_MODEL: fall back to a neutral placeholder rather than an
	// OpenRouter name, so servers that ignore the field work and servers that
	// do not produce a legible error.
	if got := ChatModel(); got != placeholderModel {
		t.Errorf("ChatModel() = %q, want %q", got, placeholderModel)
	}
	if got := EmbeddingModel(); got != placeholderModel {
		t.Errorf("EmbeddingModel() = %q, want %q", got, placeholderModel)
	}
}

func TestSelfHostedEmbeddingDimensions(t *testing.T) {
	openRouterEnv(t)
	t.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")

	// Suppressed by default, because most local embedding servers reject the
	// `dimensions` field.
	if got := EmbeddingDimensions(); got != 0 {
		t.Errorf("EmbeddingDimensions() = %d, want 0 by default when self-hosted", got)
	}
	// The column still has to be sized.
	t.Setenv("VECTOR_DIMENSIONS", "768")
	if got := VectorDimensions(); got != 768 {
		t.Errorf("VectorDimensions() = %d, want 768", got)
	}

	// A server that does accept the field can opt back in explicitly.
	t.Setenv("LLM_EMBEDDING_DIMENSIONS", "512")
	if got := EmbeddingDimensions(); got != 512 {
		t.Errorf("EmbeddingDimensions() = %d, want the explicit override", got)
	}
	if got := VectorDimensions(); got != 512 {
		t.Errorf("VectorDimensions() = %d, want it to follow the request width", got)
	}
}

func TestSelfHostedUsesLocalKeyWhenProvided(t *testing.T) {
	openRouterEnv(t)
	t.Setenv("OPENAI_BASE_URL", "http://localhost:8000/v1")
	t.Setenv("LLM_API_KEY", "local-secret")

	key, err := apiKey()
	if err != nil {
		t.Fatalf("apiKey: %v", err)
	}
	if key != "local-secret" {
		t.Errorf("apiKey() = %q, want the local key", key)
	}
}

func TestOpenRouterPathUnchanged(t *testing.T) {
	openRouterEnv(t)

	// The existing configuration must behave exactly as before.
	key, err := apiKey()
	if err != nil {
		t.Fatalf("apiKey: %v", err)
	}
	if key != "sk-or-secret" {
		t.Errorf("apiKey() = %q, want the OpenRouter key", key)
	}
	if got := ChatModel(); got != "z-ai/glm-5.2" {
		t.Errorf("ChatModel() = %q, want the OpenRouter model", got)
	}
	if got := EmbeddingModel(); got != "qwen/qwen3-embedding-8b" {
		t.Errorf("EmbeddingModel() = %q", got)
	}
	if got := EmbeddingDimensions(); got != 1024 {
		t.Errorf("EmbeddingDimensions() = %d, want 1024", got)
	}
}

func TestOpenRouterDefaults(t *testing.T) {
	clearLLMEnv(t)

	if got := ChatModel(); got != defaultChatModel {
		t.Errorf("ChatModel() = %q, want the default", got)
	}
	if got := EmbeddingModel(); got != defaultEmbeddingModel {
		t.Errorf("EmbeddingModel() = %q, want the default", got)
	}
	if got := EmbeddingDimensions(); got != DefaultEmbeddingDimensions {
		t.Errorf("EmbeddingDimensions() = %d, want the default", got)
	}

	// The generic names still work when the OpenRouter ones are absent.
	t.Setenv("LLM_MODEL", "some/model")
	t.Setenv("LLM_EMBEDDING_MODEL", "some/embedder")
	if got := ChatModel(); got != "some/model" {
		t.Errorf("ChatModel() = %q, want the LLM_MODEL fallback", got)
	}
	if got := EmbeddingModel(); got != "some/embedder" {
		t.Errorf("EmbeddingModel() = %q, want the LLM_EMBEDDING_MODEL fallback", got)
	}
}

func TestAPIKeyRequiredOnlyForOpenRouter(t *testing.T) {
	clearLLMEnv(t)

	// Against OpenRouter a missing key is a real error — failing at boot beats
	// failing on the first request.
	if _, err := apiKey(); err == nil {
		t.Error("apiKey() with no key against OpenRouter succeeded, want an error")
	}

	// Against a self-hosted server it is not: most ignore the header entirely,
	// and demanding a meaningless secret would be pure friction.
	t.Setenv("OPENAI_BASE_URL", "http://localhost:8000/v1")
	got, err := apiKey()
	if err != nil {
		t.Fatalf("apiKey() against a self-hosted endpoint: %v", err)
	}
	if got == "" {
		t.Error("apiKey() returned an empty key; the OpenAI protocol still needs the header")
	}
}

func TestEmbeddingDimensionsInvalidValues(t *testing.T) {
	for _, raw := range []string{"-8", "big", "1.5"} {
		t.Run(raw, func(t *testing.T) {
			clearLLMEnv(t)
			t.Setenv("LLM_EMBEDDING_DIMENSIONS", raw)
			if got := EmbeddingDimensions(); got != DefaultEmbeddingDimensions {
				t.Errorf("EmbeddingDimensions(%q) = %d, want the default", raw, got)
			}
		})
	}
}

func TestVectorDimensionsFallsBackWhenSuppressed(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("LLM_EMBEDDING_DIMENSIONS", "0")

	// Suppressed and unstated: fall back rather than create a 0-width column.
	if got := VectorDimensions(); got != DefaultEmbeddingDimensions {
		t.Errorf("VectorDimensions() = %d, want the default fallback", got)
	}

	t.Setenv("VECTOR_DIMENSIONS", "1536")
	if got := VectorDimensions(); got != 1536 {
		t.Errorf("VectorDimensions() = %d, want 1536", got)
	}
}

func TestDescribeLLM(t *testing.T) {
	openRouterEnv(t)
	got := DescribeLLM()
	for _, want := range []string{"OpenRouter", "z-ai/glm-5.2", "1024"} {
		if !strings.Contains(got, want) {
			t.Errorf("DescribeLLM() = %q, missing %q", got, want)
		}
	}
	// The boot log must never carry the credential.
	if strings.Contains(got, "sk-or-secret") {
		t.Errorf("DescribeLLM() leaks the API key: %q", got)
	}

	t.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")
	t.Setenv("LLM_MODEL", "qwen2.5-coder:32b")
	t.Setenv("VECTOR_DIMENSIONS", "768")
	got = DescribeLLM()
	for _, want := range []string{"self-hosted", "localhost:11434", "qwen2.5-coder:32b", "dimensions omitted", "768"} {
		if !strings.Contains(got, want) {
			t.Errorf("DescribeLLM() = %q, missing %q", got, want)
		}
	}
}
