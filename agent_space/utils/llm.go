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

// SelfHosted reports whether chat is pointed at a non-OpenRouter endpoint.
func SelfHosted() bool {
	return os.Getenv("OPENAI_BASE_URL") != ""
}

// BaseURL returns the endpoint chat completions are sent to.
func BaseURL() string {
	return EnvOr("OPENAI_BASE_URL", openRouterBaseURL)
}

// EmbeddingBaseURL returns the endpoint embeddings are sent to.
//
// Embeddings deliberately do not follow OPENAI_BASE_URL. The vector column is
// sized to the embedder's width at CREATE TABLE, so pointing chat at a local
// model for cheap iteration must not silently move the embedder too — that
// swaps a 1024-wide model for a 768-wide one and every stored vector has to be
// rebuilt. Chat is the thing worth running locally; embeddings are ~$0.01 per
// million tokens, so there is little to gain and a migration to lose.
//
// Set EMBEDDING_BASE_URL to move them anyway, on purpose.
func EmbeddingBaseURL() string {
	return EnvOr("EMBEDDING_BASE_URL", openRouterBaseURL)
}

// EmbeddingSelfHosted reports whether embeddings go somewhere other than
// OpenRouter.
func EmbeddingSelfHosted() bool {
	return EmbeddingBaseURL() != openRouterBaseURL
}

// EnvBool reports whether an environment variable is set to a truthy value.
// Anything unparseable is false, so a typo fails safe rather than silently
// enabling something.
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

// apiKeyFor resolves the credential for one endpoint.
//
// OPENROUTER_API_KEY is only ever sent to OpenRouter. A .env that still carries
// one is the normal case when chat is pointed at localhost, and forwarding a
// live billable credential to an arbitrary local process would leak it
// silently. Self-hosted servers usually ignore the key but the OpenAI protocol
// still requires the header, so a placeholder is substituted rather than
// failing — otherwise every local setup would need a meaningless secret.
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

// ChatModel returns the chat model name.
//
// One variable names the model wherever it runs: OPENROUTER_MODEL is passed
// through unchanged to a self-hosted server. Servers that serve a single loaded
// model ignore the field, and ones that route by name need whatever name you
// gave them — neither case is helped by a second variable.
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

// newOpenAICompatibleClient builds a client against one endpoint. Chat and
// embeddings can now resolve to different endpoints, so the URL is a parameter
// rather than read from the environment here.
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

// EmbeddingDimensions returns the width to request from the embedder.
//
// Zero is meaningful, not an error: langchaingo omits the `dimensions` request
// field when it is zero, and self-hosted embedding servers generally reject
// that field outright. That is why zero is the default when EMBEDDING_BASE_URL
// points somewhere else — sending the field is the likelier way to break.
// VectorDimensions then supplies the column width.
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
		EmbeddingBaseURL(),
		// Irrelevant to embedding calls, but openai.New requires a chat model.
		// The embedding model is used so a self-hosted chat placeholder cannot
		// leak into a client that only ever talks to the embedding endpoint.
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

// DescribeLLM returns a one-line summary of the resolved providers for the boot
// log. Chat and embeddings are reported separately because they can now point
// at different endpoints. It never includes a key.
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
