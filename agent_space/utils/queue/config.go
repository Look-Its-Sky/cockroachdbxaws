package queue

import (
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agent_space/utils"
)

// Defaults sized for the agent, not for a fast consumer: one run was measured
// at 66s and has been seen at 135s, so the lease has to outlive that.
const (
	DefaultRegion            = "us-east-1"
	DefaultVisibilityTimeout = 120 * time.Second
	DefaultWaitTime          = 20 * time.Second // SQS caps long polling at 20s
	DefaultRunTimeout        = 10 * time.Minute
	DefaultMaxAttempts       = 5
)

// everything needed to poll one queue, read the same way as mcp.Config: from
// the environment, with an empty required field meaning "feature off".
type Config struct {
	QueueURL string
	Region   string
	// non-empty points the SQS client somewhere other than AWS, i.e. LocalStack
	Endpoint string

	VisibilityTimeout time.Duration
	WaitTime          time.Duration
	RunTimeout        time.Duration
	// the delivery count at which a message is given up on,
	// recorded as failed and deleted rather than left to redeliver forever.
	MaxAttempts int
}

// normalised on the way out, since the worker reads these directly and a
// MaxAttempts of 0 would give up on every message before running it
func ConfigFromEnv() Config {
	return Config{
		QueueURL:          utils.EnvOr("SQS_QUEUE_URL", ""),
		Region:            utils.EnvOr("AWS_REGION", DefaultRegion),
		Endpoint:          utils.EnvOr("AWS_ENDPOINT_URL", ""),
		VisibilityTimeout: envDuration("WORKER_VISIBILITY_TIMEOUT", DefaultVisibilityTimeout),
		WaitTime:          envDuration("WORKER_WAIT_TIME", DefaultWaitTime),
		RunTimeout:        envDuration("WORKER_RUN_TIMEOUT", DefaultRunTimeout),
		MaxAttempts:       envInt("WORKER_MAX_ATTEMPTS", DefaultMaxAttempts),
	}.normalised()
}

// whether there is a queue to poll at all
func (c Config) Configured() bool { return c.QueueURL != "" }

// clamp what SQS constrains and settle the pair that can contradict, so a bad
// .env is corrected at boot rather than rejected on every receive
func (c Config) normalised() Config {
	// moving to real SQS means deleting the endpoint override too, which is
	// easy to half-do: the override wins silently and every call goes to an
	// emulator that is not listening. A URL on AWS's own domain settles it.
	if c.Endpoint != "" && isAWSQueue(c.QueueURL) {
		log.Printf("queue: SQS_QUEUE_URL is an AWS queue, so AWS_ENDPOINT_URL=%q is ignored", c.Endpoint)
		c.Endpoint = ""
	}

	if c.Region == "" {
		c.Region = DefaultRegion
	}
	if c.VisibilityTimeout <= 0 {
		c.VisibilityTimeout = DefaultVisibilityTimeout
	}
	if c.WaitTime <= 0 || c.WaitTime > 20*time.Second {
		c.WaitTime = DefaultWaitTime
	}
	if c.RunTimeout <= 0 {
		c.RunTimeout = DefaultRunTimeout
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	return c
}

// whether a queue URL addresses AWS rather than an emulator. Matched on the
// parsed host with a leading dot, so evil-amazonaws.com does not pass; China
// needs saying separately, since its hosts end .amazonaws.com.cn.
func isAWSQueue(queueURL string) bool {
	u, err := url.Parse(queueURL)
	if err != nil {
		return false
	}

	host := strings.ToLower(u.Hostname())
	return strings.HasSuffix(host, ".amazonaws.com") ||
		strings.HasSuffix(host, ".amazonaws.com.cn")
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := utils.EnvOr(key, "")
	if raw == "" {
		return fallback
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("queue: %s=%q is not a duration (e.g. 120s, 2m); using %s", key, raw, fallback)
		return fallback
	}
	return d
}

func envInt(key string, fallback int) int {
	raw := utils.EnvOr(key, "")
	if raw == "" {
		return fallback
	}

	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		log.Printf("queue: %s=%q is not a positive integer; using %d", key, raw, fallback)
		return fallback
	}
	return n
}
