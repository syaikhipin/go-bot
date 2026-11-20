package mainmodel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/content"
	"go-bot/internal/modeldata"
	"go-bot/thirdparty/llm"
)

// Config holds the model routing and protocol for a standalone model or a
// single turn of a Controller-managed session.
type Config struct {
	Endpoints map[string]Endpoint `json:"endpoints"`
	Default   agent.ModelRoute    `json:"default"`
}

type Endpoint struct {
	BaseURL  string       `json:"baseUrl"`
	APIKey   string       `json:"apiKey"`
	Protocol llm.Protocol `json:"protocol"`
}

// DefaultModelID is used when a Host deployment supplies an endpoint and
// provider but leaves the model name to the Host image.
const DefaultModelID = "deepseek-flash"

func ParseConfig(raw string) (Config, error) {
	var config Config
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode Host model config: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("Host model config has trailing JSON")
	}
	if len(config.Endpoints) == 0 {
		return Config{}, errors.New("Host model endpoints are required")
	}
	for name, endpoint := range config.Endpoints {
		if endpoint.Protocol == "" {
			endpoint.Protocol = llm.Responses
			config.Endpoints[name] = endpoint
		}
		parsed, err := url.Parse(endpoint.BaseURL)
		if name == "" || strings.TrimSpace(name) != name || err != nil || parsed.Host == "" ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
			(endpoint.Protocol != llm.Responses && endpoint.Protocol != llm.ChatCompletions && endpoint.Protocol != llm.AnthropicMessages) {
			return Config{}, fmt.Errorf("invalid Host model endpoint %q", name)
		}
	}
	if config.Default.Model == "" {
		config.Default.Model = DefaultModelID
	}
	if err := config.checkRoute(config.Default); err != nil {
		return Config{}, fmt.Errorf("invalid default Host model: %w", err)
	}
	return config, nil
}

func (c Config) checkRoute(route agent.ModelRoute) error {
	if route.Provider == "" || route.Model == "" || strings.TrimSpace(route.Provider) != route.Provider || strings.TrimSpace(route.Model) != route.Model ||
		len(route.ReasoningEffort) > 64 || strings.ContainsAny(route.ReasoningEffort, " \t\r\n") {
		return errors.New("model route is unavailable")
	}
	if route.Provider != c.Default.Provider || route.Model != c.Default.Model {
		return errors.New("all agents use model.main")
	}
	if _, ok := c.Endpoints[route.Provider]; !ok {
		return errors.New("model provider is unavailable")
	}
	return nil
}

// Model owns model selection, request assembly, attachment loading and
// response decoding. Requests go directly to the configured provider.
type Model struct {
	Config          Config
	Images          agent.ImageAttachmentReader
	ImageAccessPath func(content.ImageAttachment) string
	Transport       http.RoundTripper
}

func (m Model) CheckRoute(_ context.Context, route agent.ModelRoute) error {
	return m.Config.checkRoute(route)
}

func (m Model) Complete(ctx context.Context, messages []agent.Message, specs []agent.ToolSpec) (modeldata.AssistantMessage, error) {
	adapter, requestCtx, err := m.adapter(ctx)
	if err != nil {
		return modeldata.AssistantMessage{}, err
	}
	return adapter.Complete(requestCtx, messages, specs)
}

// Stream keeps model selection and provider credentials Host-local while
// forwarding actual provider deltas to the Agent's attempt recorder.
func (m Model) Stream(ctx context.Context, messages []agent.Message, specs []agent.ToolSpec, sink llm.StreamSink) (modeldata.AssistantMessage, error) {
	adapter, requestCtx, err := m.adapter(ctx)
	if err != nil {
		return modeldata.AssistantMessage{}, err
	}
	return adapter.Stream(requestCtx, messages, specs, sink)
}

func (m Model) StreamChunks(ctx context.Context, messages []agent.Message, specs []agent.ToolSpec, sink agent.ModelStreamSink) (modeldata.AssistantMessage, error) {
	adapter, requestCtx, err := m.adapter(ctx)
	if err != nil {
		return modeldata.AssistantMessage{}, err
	}
	return adapter.StreamChunks(requestCtx, messages, specs, sink)
}

func (m Model) adapter(ctx context.Context) (agent.LLMAdapter, context.Context, error) {
	route, selected := agent.ModelRouteFromContext(ctx)
	if !selected {
		route = m.Config.Default
		ctx = agent.WithModelRoute(ctx, route)
	}
	if err := m.Config.checkRoute(route); err != nil {
		return agent.LLMAdapter{}, nil, err
	}
	if m.Transport == nil {
		return agent.LLMAdapter{}, nil, errors.New("Host model transport is unavailable")
	}
	endpoint := m.Config.Endpoints[route.Provider]
	client, err := llm.NewClientWithHTTPAndIdleTimeout(llm.Endpoint{BaseURL: endpoint.BaseURL, APIKey: endpoint.APIKey}, route.Model,
		&http.Client{Transport: m.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, 5*time.Minute)
	if err != nil {
		return agent.LLMAdapter{}, nil, err
	}
	return agent.LLMAdapter{EndpointURL: strings.TrimRight(endpoint.BaseURL, "/"), Transport: client, Protocol: endpoint.Protocol, Images: m.Images, ImageAccessPath: m.ImageAccessPath, ImageInputEnabled: true}, ctx, nil
}
