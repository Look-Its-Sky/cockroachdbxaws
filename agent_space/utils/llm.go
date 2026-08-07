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

// OpenRouter exposes an OpenAI-compatible API, so the openai driver drives it
// with nothing more than a base URL override. Any other OpenAI-compatible
// server — Ollama, llama.cpp, vLLM, LM Studio — works the same way.
const openRouterBaseURL = "https://openrouter.ai/api/v1"

const (
	defaultChatModel = "cohere/north-mini-code:free"
	// Qwen3-Embedding-8B costs ~$0.01 per million tokens and supports
	// Matryoshka truncation, so we take the retrieval quality of a 4096-dim
	// model while storing quarter-size vectors.
	defaultEmbeddingModel = "qwen/qwen3-embedding-8b"
	// DefaultEmbeddingDimensions must match the vector column in CockroachDB;
	// changing it means recreating the embedding table.
	DefaultEmbeddingDimensions = 1024

	// placeholderModel is sent when a self-hosted endpoint is configured but no
	// model is named. Servers that serve one loaded model (llama.cpp, LM Studio)
	// ignore the field entirely; servers that route by name (Ollama, vLLM) fail
	// with this string in the message, which points at the missing setting far
	// better than an OpenRouter model name would.
	placeholderModel = "local-model"
)

var errNoAPIKey = errors.New("no LLM API key: set OPENROUTER_API_KEY, or set OPENAI_BASE_URL to use a self-hosted server instead")

// warnOnce keeps the self-hosted advisories to one line each at boot, since the
// resolvers are called from both GetLLM and GetEmbedder.
var warnOnce sync.Map

func warn(key, format string, args ...any) {
	if _, loaded := warnOnce.LoadOrStore(key, true); !loaded {
		log.Printf("WARNING: "+format, args...)
	}
}

// SelfHosted reports whether a non-OpenRouter endpoint is configured.
//
// OPENAI_BASE_URL is the single switch for the whole provider: when it is set,
// the LLM_* / OPENAI_* settings take precedence over the OPENROUTER_* ones, so
// an existing OpenRouter configuration left in .env cannot leak a model name or
// key into requests aimed at a local server.
func SelfHosted() bool {
	return os.Getenv("OPENAI_BASE_URL") != ""
}

// BaseURL returns the OpenAI-compatible endpoint to talk to.
func BaseURL() string {
	return EnvOr("OPENAI_BASE_URL", openRouterBaseURL)
}

// EnvOr returns the environment variable named by key, or fallback if unset.
// EnvBool reports whether an environment variable is set to a truthy value.
// Anything unparseable is false, so a typo fails safe rather than silently
// enabling something.
func EnvBool(key string) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key)))
	return err == nil && v
}

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

// apiKey resolves the credential for the active provider.
//
// Self-hosted servers usually ignore the key but the OpenAI protocol still
// requires the header, so a placeholder is substituted rather than failing —
// otherwise every local setup would need a meaningless secret in .env.
func apiKey() (string, error) {
	if SelfHosted() {
		// OPENROUTER_API_KEY is deliberately not consulted here. It is a live
		// billable credential, and a .env that still carries one is the normal
		// case when switching to a local endpoint — forwarding it to an
		// arbitrary process on localhost would leak it silently.
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

// ChatModel returns the chat model name for the active provider.
func ChatModel() string {
	if SelfHosted() {
		if model := firstEnv("LLM_MODEL", "OPENAI_MODEL"); model != "" {
			return model
		}
		warn("chat-model", "OPENAI_BASE_URL is set but no chat model is configured; sending %q. Set LLM_MODEL if your server routes by model name.", placeholderModel)
		return placeholderModel
	}

	return EnvOr("OPENROUTER_MODEL", EnvOr("LLM_MODEL", defaultChatModel))
}

// EmbeddingModel returns the embedding model name for the active provider.
func EmbeddingModel() string {
	if SelfHosted() {
		if model := firstEnv("LLM_EMBEDDING_MODEL", "OPENAI_EMBEDDING_MODEL"); model != "" {
			return model
		}
		warn("embedding-model", "OPENAI_BASE_URL is set but no embedding model is configured; sending %q. Set LLM_EMBEDDING_MODEL if your server routes by model name.", placeholderModel)
		return placeholderModel
	}

	return EnvOr("OPENROUTER_EMBEDDING_MODEL", EnvOr("LLM_EMBEDDING_MODEL", defaultEmbeddingModel))
}

// newOpenAICompatibleClient builds a client against the configured endpoint.
// Chat and embeddings use different models, so each caller gets its own client
// rather than sharing one instance carrying both.
func newOpenAICompatibleClient(opts ...openai.Option) (*openai.LLM, error) {
	key, err := apiKey()
	if err != nil {
		return nil, err
	}

	return openai.New(append([]openai.Option{
		openai.WithToken(key),
		openai.WithBaseURL(BaseURL()),
	}, opts...)...)
}

// GetLLM initializes and returns the chat model backing the agent.
func GetLLM() (llms.Model, error) {
	return newOpenAICompatibleClient(
		openai.WithModel(ChatModel()),
	)
}

// EmbeddingDimensions returns the width to request from the embedder.
//
// Zero is meaningful, not an error: langchaingo omits the `dimensions` request
// field when it is zero, and self-hosted embedding servers generally reject
// that field outright. That is also why zero is the default when a self-hosted
// endpoint is configured — sending the field is the likelier way to break.
// VectorDimensions then supplies the column width.
func EmbeddingDimensions() int {
	raw := firstEnv("OPENROUTER_EMBEDDING_DIMENSIONS", "LLM_EMBEDDING_DIMENSIONS")
	if SelfHosted() {
		// The OpenRouter-named variable describes the OpenRouter provider, so
		// it must not decide what a local server receives.
		raw = firstEnv("LLM_EMBEDDING_DIMENSIONS", "OPENAI_EMBEDDING_DIMENSIONS")
		if raw == "" {
			return 0
		}
	}
	if raw == "" {
		return DefaultEmbeddingDimensions
	}

	dims, err := strconv.Atoi(raw)
	if err != nil || dims < 0 {
		log.Printf("WARNING: ignoring invalid embedding dimensions %q", raw)
		return DefaultEmbeddingDimensions
	}
	return dims
}

// VectorDimensions returns the width to size the vector column to.
//
// It follows EmbeddingDimensions unless the request field is being suppressed
// with 0, in which case VECTOR_DIMENSIONS must state the model's native width —
// the column has to be sized even when the request does not carry the field.
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
		// Irrelevant to embedding calls, but openai.New requires a chat model.
		openai.WithModel(ChatModel()),
		openai.WithEmbeddingModel(EmbeddingModel()),
		// Zero is passed through deliberately: langchaingo drops the field.
		openai.WithEmbeddingDimensions(EmbeddingDimensions()),
	)
	if err != nil {
		return nil, err
	}

	return embeddings.NewEmbedder(client)
}

// DescribeLLM returns a one-line summary of the resolved provider for the boot
// log. It never includes the key.
func DescribeLLM() string {
	provider := "OpenRouter"
	if SelfHosted() {
		provider = "self-hosted"
	}

	return provider + " at " + BaseURL() +
		" | chat: " + ChatModel() +
		" | embeddings: " + EmbeddingModel() +
		" (request " + describeDims(EmbeddingDimensions()) +
		", column " + strconv.Itoa(VectorDimensions()) + ")"
}

func describeDims(dims int) string {
	if dims == 0 {
		return "dimensions omitted"
	}
	return strconv.Itoa(dims) + " dims"
}
