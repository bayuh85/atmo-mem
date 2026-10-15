// Package llm calls an OpenAI-compatible chat completions endpoint.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNoAPIKey is returned when a completion is requested without a key.
var ErrNoAPIKey = errors.New("LLM API key is not set (ATMO_MEM_API_KEY)")

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Completer produces a single assistant reply.
type Completer interface {
	Complete(ctx context.Context, messages []Message) (string, error)
}

// Client is a provider-agnostic OpenAI-compatible chat client.
// BaseURL includes the API prefix, for example https://atmorouter.dev/v1.
type Client struct {
	BaseURL   string
	APIKey    string
	Model     string
	Timeout   time.Duration
	UserAgent string
	HTTP      *http.Client
}

// New builds a client. timeout is applied to each Complete call.
func New(baseURL, apiKey, model, userAgent string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{
		BaseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:    strings.TrimSpace(apiKey),
		Model:     strings.TrimSpace(model),
		Timeout:   timeout,
		UserAgent: userAgent,
		HTTP:      &http.Client{Timeout: timeout + 2*time.Second},
	}
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete posts /chat/completions and returns the assistant text.
func (c *Client) Complete(ctx context.Context, messages []Message) (string, error) {
	if c == nil {
		return "", errors.New("llm client is nil")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return "", ErrNoAPIKey
	}
	if c.BaseURL == "" {
		return "", errors.New("LLM base URL is not set (ATMO_MEM_BASE_URL)")
	}
	if c.Model == "" {
		return "", errors.New("LLM model is not set (ATMO_MEM_MODEL)")
	}
	if len(messages) == 0 {
		return "", errors.New("llm: no messages")
	}

	payload, err := json.Marshal(chatRequest{
		Model:       c.Model,
		Messages:    messages,
		Temperature: 0.2,
		MaxTokens:   1024,
	})
	if err != nil {
		return "", fmt.Errorf("llm: encode request: %w", err)
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint := c.BaseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("llm: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm: model %s: status %d: %s", c.Model, resp.StatusCode, snippet(raw))
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("llm: decode response: %w: %s", err, snippet(raw))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("llm: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("llm: response had no choices")
	}
	text, err := messageText(parsed.Choices[0].Message.Content)
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	return text, nil
}

func messageText(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
		return b.String(), nil
	}
	return "", errors.New("unsupported message content")
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}
