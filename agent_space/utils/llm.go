package utils

import (
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/tmc/langchaingo/embeddings"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
)

// OpenRouter is OpenAI-compatible, so the openai driver needs only a base URL override
const openRouterBaseURL = "https://openrouter.ai/api/v1"

const (
	defaultChatModel = "cohere/north-mini-code:free"
	// ~$0.01/Mtok with Matryoshka truncation, so 4096-dim quality at quarter-size vectors
	defaultEmbeddingModel = "qwen/qwen3-embedding-8b"
	// must match the vector column; changing it means recreating the embedding table
	DefaultEmbeddingDimensions = 1024

	// sent when a self-hosted endpoint is configured but no model is named
	placeholderModel = "local-model"
)

var errNoAPIKey = errors.New("no LLM API key: set OPENROUTER_API_KEY, or set OPENAI_BASE_URL to use a self-hosted server instead")

// keep the self-hosted advisories to one line each at boot
var warnOnce sync.Map

func warn(key, format string, args ...any) {
	if _, loaded := warnOnce.LoadOrStore(key, true); !loaded {
		log.Printf("WARNING: "+format, args...)
	}
}

// SelfHosted reports whether chat is pointed at a non-OpenRouter endpoint.
func SelfHosted() bool {
	return os.Getenv("OPENAI_BASE_URL") != ""
}

// BaseURL returns the endpoint chat completions are sent to.
func BaseURL() string {
	return EnvOr("OPENAI_BASE_URL", openRouterBaseURL)
}

// embeddings do not follow OPENAI_BASE_URL: the column is sized to the embedder, so a silent swap invalidates every vector. EMBEDDING_BASE_URL moves them on purpose.
func EmbeddingBaseURL() string {
	return EnvOr("EMBEDDING_BASE_URL", openRouterBaseURL)
}

// whether embeddings go somewhere other than OpenRouter
func EmbeddingSelfHosted() bool {
	return EmbeddingBaseURL() != openRouterBaseURL
}

// truthy env var; anything unparseable is false so a typo fails safe
func EnvBool(key string) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key)))
	return err == nil && v
}

// EnvOr returns the environment variable named by key, or fallback if unset.
func EnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// firstEnv returns the value of the first variable in keys that is set.
func firstEnv(keys ...string) string {
	for _, key := range keys {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return ""
}

// OPENROUTER_API_KEY is only ever sent to OpenRouter; self-hosted gets a placeholder rather than a leaked billable key
func apiKeyFor(baseURL string) (string, error) {
	if baseURL != openRouterBaseURL {
		if key := firstEnv("OPENAI_API_KEY", "LLM_API_KEY"); key != "" {
			return key, nil
		}
		return "sk-no-key-required", nil
	}

	if key := firstEnv("OPENROUTER_API_KEY", "OPENAI_API_KEY", "LLM_API_KEY"); key != "" {
		return key, nil
	}
	return "", errNoAPIKey
}

// one variable names the model wherever it runs, passed through unchanged to a self-hosted server
func ChatModel() string {
	if model := os.Getenv("OPENROUTER_MODEL"); model != "" {
		return model
	}

	if SelfHosted() {
		warn("chat-model", "OPENAI_BASE_URL is set but OPENROUTER_MODEL is empty; sending %q. Set OPENROUTER_MODEL if your server routes by model name.", placeholderModel)
		return placeholderModel
	}

	return defaultChatModel
}

// EmbeddingModel returns the embedding model name.
func EmbeddingModel() string {
	if model := os.Getenv("OPENROUTER_EMBEDDING_MODEL"); model != "" {
		return model
	}

	if EmbeddingSelfHosted() {
		warn("embedding-model", "EMBEDDING_BASE_URL is set but OPENROUTER_EMBEDDING_MODEL is empty; sending %q. Set OPENROUTER_EMBEDDING_MODEL if your server routes by model name.", placeholderModel)
		return placeholderModel
	}

	return defaultEmbeddingModel
}

// client for one endpoint; chat and embeddings can resolve differently so the URL is a parameter
func newOpenAICompatibleClient(baseURL string, opts ...openai.Option) (*openai.LLM, error) {
	key, err := apiKeyFor(baseURL)
	if err != nil {
		return nil, err
	}

	return openai.New(append([]openai.Option{
		openai.WithToken(key),
		openai.WithBaseURL(baseURL),
	}, opts...)...)
}

// GetLLM initializes and returns the chat model backing the agent.
func GetLLM() (llms.Model, error) {
	return newOpenAICompatibleClient(
		BaseURL(),
		openai.WithModel(ChatModel()),
	)
}

// width to request; zero is meaningful, langchaingo omits the field and self-hosted servers reject it
func EmbeddingDimensions() int {
	raw := os.Getenv("OPENROUTER_EMBEDDING_DIMENSIONS")
	if raw == "" {
		if EmbeddingSelfHosted() {
			return 0
		}
		return DefaultEmbeddingDimensions
	}

	dims, err := strconv.Atoi(raw)
	if err != nil || dims < 0 {
		log.Printf("WARNING: ignoring invalid OPENROUTER_EMBEDDING_DIMENSIONS %q", raw)
		return DefaultEmbeddingDimensions
	}
	return dims
}

// width to size the column to; follows EmbeddingDimensions unless the request field is suppressed with 0
func VectorDimensions() int {
	if dims := EmbeddingDimensions(); dims > 0 {
		return dims
	}

	raw := os.Getenv("VECTOR_DIMENSIONS")
	if raw == "" {
		warn("vector-dimensions", "embedding dimensions are suppressed but VECTOR_DIMENSIONS is unset; sizing the vector column to %d. If your embedding model is a different width, inserts will fail — set VECTOR_DIMENSIONS to its native width and re-run `go run ./cmd/nuke -mode=drop`.", DefaultEmbeddingDimensions)
		return DefaultEmbeddingDimensions
	}

	dims, err := strconv.Atoi(raw)
	if err != nil || dims <= 0 {
		log.Printf("WARNING: ignoring invalid VECTOR_DIMENSIONS %q", raw)
		return DefaultEmbeddingDimensions
	}
	return dims
}

// GetEmbedder initializes and returns the Embedder used by the vector store.
func GetEmbedder() (embeddings.Embedder, error) {
	client, err := newOpenAICompatibleClient(
		EmbeddingBaseURL(),
		// irrelevant to embedding calls but openai.New requires a chat model; use the embedding one so no placeholder leaks
		openai.WithModel(EmbeddingModel()),
		openai.WithEmbeddingModel(EmbeddingModel()),
		// Zero is passed through deliberately: langchaingo drops the field.
		openai.WithEmbeddingDimensions(EmbeddingDimensions()),
	)
	if err != nil {
		return nil, err
	}

	return embeddings.NewEmbedder(client)
}

// one-line summary of the resolved providers for the boot log, never including a key
func DescribeLLM() string {
	return "chat: " + ChatModel() + " via " + providerName(SelfHosted()) + " at " + BaseURL() +
		" | embeddings: " + EmbeddingModel() + " via " + providerName(EmbeddingSelfHosted()) + " at " + EmbeddingBaseURL() +
		" (request " + describeDims(EmbeddingDimensions()) +
		", column " + strconv.Itoa(VectorDimensions()) + ")"
}

func providerName(selfHosted bool) string {
	if selfHosted {
		return "self-hosted"
	}
	return "OpenRouter"
}

func describeDims(dims int) string {
	if dims == 0 {
		return "dimensions omitted"
	}
	return strconv.Itoa(dims) + " dims"
}
