package utils

import (
	"strings"
	"testing"
)

// clearLLMEnv blanks every variable the resolvers read, plus the retired ones,
// so each case starts from a known state regardless of the developer's real
// .env.
func clearLLMEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OPENAI_BASE_URL", "EMBEDDING_BASE_URL",
		"OPENROUTER_API_KEY", "OPENAI_API_KEY", "LLM_API_KEY",
		"OPENROUTER_MODEL", "OPENROUTER_EMBEDDING_MODEL",
		"OPENROUTER_EMBEDDING_DIMENSIONS", "VECTOR_DIMENSIONS",
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

// One variable names the chat model wherever it runs. Previously OPENROUTER_MODEL
// was ignored the moment OPENAI_BASE_URL was set, which meant two names for one
// setting and a silent fallback when only one of them was filled in.
func TestChatModelUsesOneVariableForBothProviders(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("OPENROUTER_MODEL", "qwen2.5-coder:32b")

	if got := ChatModel(); got != "qwen2.5-coder:32b" {
		t.Errorf("ChatModel() = %q against OpenRouter", got)
	}

	t.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")
	if got := ChatModel(); got != "qwen2.5-coder:32b" {
		t.Errorf("ChatModel() = %q when self-hosted, want the same variable to win", got)
	}
}

// The reason embeddings no longer follow OPENAI_BASE_URL: the vector column is
// sized to the embedder at CREATE TABLE, so moving chat to a local model must
// not quietly move the embedder too and invalidate every stored vector.
func TestEmbeddingsStayOnOpenRouterWhenChatIsSelfHosted(t *testing.T) {
	openRouterEnv(t)
	t.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")

	if EmbeddingSelfHosted() {
		t.Error("EmbeddingSelfHosted() = true; OPENAI_BASE_URL must not move embeddings")
	}
	if got := EmbeddingBaseURL(); got != openRouterBaseURL {
		t.Errorf("EmbeddingBaseURL() = %q, want OpenRouter", got)
	}
	if got := EmbeddingModel(); got != "qwen/qwen3-embedding-8b" {
		t.Errorf("EmbeddingModel() = %q, want the OpenRouter embedder", got)
	}
	if got := EmbeddingDimensions(); got != 1024 {
		t.Errorf("EmbeddingDimensions() = %d, want 1024 to survive a local chat endpoint", got)
	}

	// The billable key reaches OpenRouter for embeddings...
	key, err := apiKeyFor(EmbeddingBaseURL())
	if err != nil {
		t.Fatalf("apiKeyFor(embeddings): %v", err)
	}
	if key != "sk-or-secret" {
		t.Errorf("apiKeyFor(embeddings) = %q, want the OpenRouter key", key)
	}

	// ...and never reaches the local chat endpoint.
	key, err = apiKeyFor(BaseURL())
	if err != nil {
		t.Fatalf("apiKeyFor(chat): %v", err)
	}
	if key == "sk-or-secret" {
		t.Error("the OpenRouter key was sent to a self-hosted endpoint")
	}
}

func TestSelfHostedDefaultsWithoutModelName(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("OPENAI_BASE_URL", "http://localhost:8080/v1")

	// No OPENROUTER_MODEL: fall back to a neutral placeholder rather than an
	// OpenRouter name, so servers that ignore the field work and servers that
	// do not produce a legible error.
	if got := ChatModel(); got != placeholderModel {
		t.Errorf("ChatModel() = %q, want %q", got, placeholderModel)
	}
}

func TestEmbeddingSelfHostedViaEmbeddingBaseURL(t *testing.T) {
	openRouterEnv(t)
	t.Setenv("EMBEDDING_BASE_URL", "http://localhost:11434/v1")
	t.Setenv("OPENROUTER_EMBEDDING_DIMENSIONS", "")

	if !EmbeddingSelfHosted() {
		t.Error("EmbeddingSelfHosted() = false with EMBEDDING_BASE_URL set")
	}
	// Suppressed by default, because most local embedding servers reject the
	// `dimensions` field.
	if got := EmbeddingDimensions(); got != 0 {
		t.Errorf("EmbeddingDimensions() = %d, want 0 so the field is omitted", got)
	}
	// The column still has to be sized.
	t.Setenv("VECTOR_DIMENSIONS", "768")
	if got := VectorDimensions(); got != 768 {
		t.Errorf("VectorDimensions() = %d, want 768", got)
	}

	// A server that does accept the field can opt back in explicitly.
	t.Setenv("OPENROUTER_EMBEDDING_DIMENSIONS", "512")
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

	key, err := apiKeyFor(BaseURL())
	if err != nil {
		t.Fatalf("apiKeyFor: %v", err)
	}
	if key != "local-secret" {
		t.Errorf("apiKeyFor() = %q, want the local key", key)
	}
}

func TestOpenRouterPathUnchanged(t *testing.T) {
	openRouterEnv(t)

	key, err := apiKeyFor(BaseURL())
	if err != nil {
		t.Fatalf("apiKeyFor: %v", err)
	}
	if key != "sk-or-secret" {
		t.Errorf("apiKeyFor() = %q, want the OpenRouter key", key)
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
}

func TestAPIKeyRequiredOnlyForOpenRouter(t *testing.T) {
	clearLLMEnv(t)

	// Against OpenRouter a missing key is a real error — failing at boot beats
	// failing on the first request.
	if _, err := apiKeyFor(openRouterBaseURL); err == nil {
		t.Error("apiKeyFor(OpenRouter) with no key succeeded, want an error")
	}

	// Against a self-hosted server it is not: most ignore the header entirely,
	// and demanding a meaningless secret would be pure friction.
	got, err := apiKeyFor("http://localhost:8000/v1")
	if err != nil {
		t.Fatalf("apiKeyFor() against a self-hosted endpoint: %v", err)
	}
	if got == "" {
		t.Error("apiKeyFor() returned an empty key; the OpenAI protocol still needs the header")
	}
}

func TestEmbeddingDimensionsInvalidValues(t *testing.T) {
	for _, raw := range []string{"-8", "big", "1.5"} {
		t.Run(raw, func(t *testing.T) {
			clearLLMEnv(t)
			t.Setenv("OPENROUTER_EMBEDDING_DIMENSIONS", raw)
			if got := EmbeddingDimensions(); got != DefaultEmbeddingDimensions {
				t.Errorf("EmbeddingDimensions(%q) = %d, want the default", raw, got)
			}
		})
	}
}

func TestVectorDimensionsFallsBackWhenSuppressed(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("OPENROUTER_EMBEDDING_DIMENSIONS", "0")

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

	// Split providers: the line has to show both, or a local chat endpoint that
	// is not being picked up looks identical to one that is.
	t.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")
	got = DescribeLLM()
	for _, want := range []string{"self-hosted", "localhost:11434", "z-ai/glm-5.2", "OpenRouter", "qwen/qwen3-embedding-8b"} {
		if !strings.Contains(got, want) {
			t.Errorf("DescribeLLM() = %q, missing %q", got, want)
		}
	}
}
