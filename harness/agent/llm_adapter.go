package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	agentcontent "go-bot/harness/content"
	"go-bot/internal/modeldata"
	"go-bot/thirdparty/llm"
)

// LLMTransport is the low-level model call used by the ServerAgents Harness.
// Attachment resolution and image offload remain Harness responsibilities.
type LLMTransport interface {
	GenerateWithProtocol(context.Context, llm.Protocol, llm.Request) (*llm.Response, error)
}

// LLMStreamTransport exposes provider events while the response is still in
// flight. The Agent loop, not the transport, owns attempt settlement.
type LLMStreamTransport interface {
	StreamWithProtocol(context.Context, llm.Protocol, llm.Request, llm.StreamSink) (*llm.Response, error)
}

type LLMAdapter struct {
	EndpointURL         string
	Transport           LLMTransport
	Protocol            llm.Protocol
	Images              ImageAttachmentReader
	ImageAccessPath     func(agentcontent.ImageAttachment) string
	ImageInputEnabled   bool
	MaxInlineImageBytes int64
	MaxImages           int
	InlineByteQuantum   int64
	ImageCountQuantum   int
}

type requestImageLoader interface {
	LoadRequestImage(context.Context, agentcontent.ImageAttachment) ([]byte, string, error)
}

type resolvedModelImage struct {
	data      []byte
	mediaType string
}

type transportImage struct {
	attachment agentcontent.ImageAttachment
	data       []byte
	itemIndex  int
	blockIndex int
}

// imageAccessResolver is optional because a model adapter can also be used
// with a remote attachment reader that exposes bytes but no tool-world path.
// The path is resolved at request time, not persisted in the v4 Session event.
type imageAccessResolver interface {
	ImageAccessPath(agentcontent.ImageAttachment) string
}

func (a LLMAdapter) offloadedImageText(ref agentcontent.ImageAttachment) string {
	identity := ref.ID
	if ref.Name != "" {
		identity = nativeJSONQuote(ref.Name) + " (" + ref.ID + ")"
	}
	text := "[image omitted to fit request image limits; " + identity + "."
	path := ""
	if a.ImageAccessPath != nil {
		path = a.ImageAccessPath(ref)
	} else if resolver, ok := a.Images.(imageAccessResolver); ok {
		path = resolver.ImageAccessPath(ref)
	}
	if path == "" {
		return text + " No local normalized image path is available; ask the user to attach it again if needed.]"
	}
	text += " Normalized copy (read-only; may be resized or re-encoded): " + nativeJSONQuote(path) + " (" + fmt.Sprintf("%dx%dpx, %s", ref.Width, ref.Height, ref.MIMEType) + ")."
	text += " Source dimensions, format, and byte size may differ."
	suffix := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}[ref.MIMEType]
	if suffix != "" {
		text += " Copy to a writable path ending in " + suffix + " before editing."
	}
	return text + "]"
}

func (a LLMAdapter) Complete(ctx context.Context, messages []Message, specs []ToolSpec) (modeldata.AssistantMessage, error) {
	return a.complete(ctx, messages, specs, nil, false)
}

// Stream uses the same request projection as Complete, including image
// offload, but exposes actual provider events before the final response.
// It never fabricates delta events from a completed response.
func (a LLMAdapter) Stream(ctx context.Context, messages []Message, specs []ToolSpec, sink llm.StreamSink) (modeldata.AssistantMessage, error) {
	if sink == nil {
		return modeldata.AssistantMessage{}, errors.New("LLM stream sink is required")
	}
	return a.complete(ctx, messages, specs, sink, true)
}

// StreamChunks projects the provider's live protocol events into the native
// Harness chunk vocabulary. The caller owns timestamps, attempt identity and
// durable settlement; this adapter never treats chunks as committed messages.
func (a LLMAdapter) StreamChunks(ctx context.Context, messages []Message, specs []ToolSpec, sink ModelStreamSink) (modeldata.AssistantMessage, error) {
	if sink == nil {
		return modeldata.AssistantMessage{}, errors.New("model stream chunk sink is required")
	}
	protocol := a.Protocol
	if protocol == "" {
		protocol = llm.Responses
	}
	projection := newLLMStreamProjection(protocol, sink)
	return a.Stream(ctx, messages, specs, projection.push)
}

func (a LLMAdapter) complete(ctx context.Context, messages []Message, specs []ToolSpec, sink llm.StreamSink, stream bool) (modeldata.AssistantMessage, error) {
	if a.Transport == nil {
		return modeldata.AssistantMessage{}, errors.New("LLM transport is not configured")
	}
	var streamer LLMStreamTransport
	if stream {
		var ok bool
		streamer, ok = a.Transport.(LLMStreamTransport)
		if !ok {
			return modeldata.AssistantMessage{}, errors.New("LLM transport does not support streaming")
		}
	}
	input := make([]llm.Item, 0, len(messages)*2)
	var images []transportImage
	referenceIDs := slices.ContainsFunc(specs, func(spec ToolSpec) bool { return spec.Name == "generateimage" })
	resolved := make(map[string]resolvedModelImage)
	metadata := make(map[string]agentcontent.ImageAttachment)
	route, _ := ModelRouteFromContext(ctx)
	for _, message := range messages {
		if replay := message.ProviderReplay; message.Role == "assistant" && a.Protocol == llm.AnthropicMessages && replay != nil && replay.Protocol == string(a.Protocol) && replay.EndpointID == route.Provider && replay.ModelID == route.Model && replay.EndpointURL == a.EndpointURL && ModelCallOptionsFromContext(ctx).Purpose != "compaction" {
			raw, err := json.Marshal(replay.Content)
			if err != nil {
				return modeldata.AssistantMessage{}, err
			}
			input = append(input, llm.Item{Type: "anthropic_message", Role: "assistant", Raw: raw})
			continue
		}
		if message.Role == "assistant" && len(message.OrderedAssistantBlocks) > 0 {
			ordered, err := orderedAssistantRequestItems(message.OrderedAssistantBlocks)
			if err != nil {
				return modeldata.AssistantMessage{}, err
			}
			input = append(input, ordered...)
			continue
		}
		startImages := len(images)
		blocks := make([]llm.ContentBlock, 0, len(message.ContentBlocks)+1)
		if message.Content != "" && !message.ContentBlocksComplete {
			blocks = append(blocks, llm.ContentBlock{Type: "text", Text: message.Content})
		}
		imageIndex := 0
		for _, block := range message.ContentBlocks {
			switch block.Type {
			case "text":
				blocks = append(blocks, llm.ContentBlock{Type: "text", Text: block.Text})
			case "image":
				if block.ImageRefID == "" {
					return modeldata.AssistantMessage{}, errors.New("model message has invalid image attachment")
				}
				if message.IsImageOffloaded(imageIndex) {
					blocks = append(blocks, llm.ContentBlock{Type: "text", Text: a.offloadedImageText(agentcontent.ImageAttachmentFromBlock(block))})
					imageIndex++
					continue
				}
				imageIndex++
				if (message.Role != "user" && message.Role != "tool") || a.Images == nil || !a.ImageInputEnabled {
					return modeldata.AssistantMessage{}, errors.New("image-capable model adapter and attachment resolver are not configured")
				}
				ref := agentcontent.ImageAttachmentFromBlock(block)
				if ref.MIMEType == "" || ref.Bytes <= 0 || ref.Width <= 0 || ref.Height <= 0 {
					return modeldata.AssistantMessage{}, errors.New("model image attachment lacks required metadata")
				}
				loaded, exists := resolved[ref.ID]
				if !exists {
					var err error
					if variants, ok := a.Images.(requestImageLoader); ok {
						loaded.data, loaded.mediaType, err = variants.LoadRequestImage(ctx, ref)
					} else {
						loaded.data, err = a.Images.LoadImage(ctx, ref)
						loaded.mediaType = ref.MIMEType
					}
					if err != nil {
						return modeldata.AssistantMessage{}, fmt.Errorf("load model image %s: %w", ref.ID, err)
					}
					resolved[ref.ID] = loaded
					metadata[ref.ID] = ref
				} else if previous := metadata[ref.ID]; previous.MIMEType != ref.MIMEType || previous.Bytes != ref.Bytes || previous.Width != ref.Width || previous.Height != ref.Height {
					return modeldata.AssistantMessage{}, fmt.Errorf("conflicting metadata for image attachment %s", ref.ID)
				}
				if referenceIDs {
					blocks = append(blocks, llm.ContentBlock{Type: "text", Text: "Image attachment ID: " + ref.ID})
				}
				blocks = append(blocks, llm.ContentBlock{Type: "image", Detail: "high"})
				ref.MIMEType = loaded.mediaType
				images = append(images, transportImage{attachment: ref, data: loaded.data, itemIndex: -1, blockIndex: len(blocks) - 1})
			default:
				return modeldata.AssistantMessage{}, fmt.Errorf("unsupported model content block %q", block.Type)
			}
		}
		if message.Role == "tool" && a.Protocol == llm.AnthropicMessages {
			input = append(input, llm.Item{Type: "tool_result", CallID: message.ToolCallID, Content: blocks, IsError: message.IsError})
			for index := startImages; index < len(images); index++ {
				images[index].itemIndex = len(input) - 1
			}
			continue
		}
		if message.Role == "tool" {
			var resultText strings.Builder
			for index, block := range blocks {
				if block.Type == "text" {
					if resultText.Len() > 0 && index > 0 {
						resultText.WriteByte('\n')
					}
					resultText.WriteString(block.Text)
				}
			}
			input = append(input, llm.Item{Type: "tool_result", CallID: message.ToolCallID, Content: []llm.ContentBlock{{Type: "text", Text: resultText.String()}}})
			media := make([]llm.ContentBlock, 0, len(blocks))
			for _, block := range blocks {
				if block.Type == "image" {
					media = append(media, block)
				}
			}
			if len(media) > 0 {
				prefix := llm.ContentBlock{Type: "text", Text: "Images returned by tool " + message.ToolName + " (call " + message.ToolCallID + "):"}
				input = append(input, llm.Item{Type: "message", Role: "user", Content: append([]llm.ContentBlock{prefix}, media...)})
				for index := startImages; index < len(images); index++ {
					images[index].itemIndex = len(input) - 1
					images[index].blockIndex = 1 + index - startImages
				}
			}
			continue
		}
		input = append(input, llm.Item{Type: "message", Role: message.Role, Content: blocks})
		for index := startImages; index < len(images); index++ {
			images[index].itemIndex = len(input) - 1
		}
		for _, call := range message.ToolCalls {
			input = append(input, llm.Item{Type: "tool_call", CallID: call.ID, Name: call.Name, Arguments: call.Arguments})
		}
	}
	budget := make([]preparedImage, 0, len(images))
	for _, image := range images {
		budget = append(budget, preparedImage{attachment: image.attachment, data: image.data})
	}
	count, err := imagesToOffload(budget, imageBudget{
		maxBytes: a.MaxInlineImageBytes, maxImages: a.MaxImages,
		byteQuantum: a.InlineByteQuantum, countQuantum: a.ImageCountQuantum,
	})
	if err != nil {
		return modeldata.AssistantMessage{}, err
	}
	if count > 0 {
		return modeldata.AssistantMessage{}, &ImageOffloadRequiredError{Count: count}
	}
	for _, image := range images {
		input[image.itemIndex].Content[image.blockIndex].Source = &llm.ImageSource{Type: "url", URL: "data:" + image.attachment.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(image.data)}
	}
	tools := make([]llm.Tool, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, llm.Tool{Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters})
	}
	protocol := a.Protocol
	if protocol == "" {
		protocol = llm.Responses
	}
	options := ModelCallOptionsFromContext(ctx)
	request := llm.Request{
		Input: input, Tools: tools, Purpose: options.Purpose, MaxOutputTokens: options.MaxOutputTokens,
		ReasoningEffort: route.ReasoningEffort,
	}
	var response *llm.Response
	if stream {
		response, err = streamer.StreamWithProtocol(ctx, protocol, request, sink)
	} else {
		response, err = a.Transport.GenerateWithProtocol(ctx, protocol, request)
	}
	maxTokens := response != nil &&
		((response.Status == "length" && response.FinishReason == "length") ||
			(response.Status == "incomplete" && response.FinishReason == "max_output_tokens"))
	if err != nil && !maxTokens {
		return modeldata.AssistantMessage{Usage: transportUsage(response)}, normalizeTransportError(err, response)
	}
	if response == nil {
		return modeldata.AssistantMessage{}, errors.New("LLM transport returned nil response")
	}
	var output modeldata.AssistantMessage
	output.Usage = transportUsage(response)
	output.MaxTokens = maxTokens
	var content strings.Builder
	for _, item := range response.Output {
		switch item.Type {
		case "reasoning":
			if options.Purpose != "compaction" {
				for _, block := range item.Content {
					if block.Type == "text" {
						output.OutputBlocks = append(output.OutputBlocks, modeldata.AssistantOutputBlock{Type: "reasoning", Text: block.Text})
					}
				}
			}
		case "message":
			for _, block := range item.Content {
				if block.Type == "text" {
					content.WriteString(block.Text)
					if options.Purpose != "compaction" {
						output.OutputBlocks = append(output.OutputBlocks, modeldata.AssistantOutputBlock{Type: "text", Text: block.Text})
					}
					if options.Purpose == "compaction" {
						output.RawOutput = append(output.RawOutput, agentcontent.ContentBlock{Type: "text", Text: block.Text})
					}
				} else if options.Purpose == "compaction" {
					return modeldata.AssistantMessage{Usage: output.Usage}, errors.New("compaction summary contains non-text model output")
				}
			}
		case "tool_call":
			// A max-token response may contain a partially formed tool call.
			// DeepSeek keeps completed text but never dispatches or replays any
			// tool calls from that truncated assistant step.
			if maxTokens {
				continue
			}
			output.ToolCalls = append(output.ToolCalls, modeldata.ToolRequest{ID: item.CallID, Name: item.Name, Arguments: item.Arguments})
			if options.Purpose != "compaction" {
				output.OutputBlocks = append(output.OutputBlocks, modeldata.AssistantOutputBlock{Type: "tool-call", ID: item.CallID, Name: item.Name, Arguments: item.Arguments})
			}
		}
	}
	output.Content = content.String()
	if protocol == llm.AnthropicMessages && !output.MaxTokens && options.Purpose != "compaction" && len(response.NativeContent) > 0 {
		output.ProviderReplay = &modeldata.ProviderReplay{EndpointURL: a.EndpointURL, Protocol: string(protocol), EndpointID: route.Provider, ModelID: route.Model, Content: response.NativeContent}
	}
	return output, nil
}

func orderedAssistantRequestItems(blocks []modeldata.AssistantOutputBlock) ([]llm.Item, error) {
	items := make([]llm.Item, 0, len(blocks))
	currentType := ""
	current := make([]llm.ContentBlock, 0)
	flush := func() {
		if len(current) == 0 {
			return
		}
		kind := "message"
		if currentType == "reasoning" {
			kind = "reasoning"
		}
		items = append(items, llm.Item{Type: kind, Role: "assistant", Content: current})
		current = nil
	}
	for _, block := range blocks {
		switch block.Type {
		case "text", "reasoning":
			if currentType != block.Type {
				flush()
				currentType = block.Type
			}
			current = append(current, llm.ContentBlock{Type: "text", Text: block.Text})
		case "tool-call":
			if block.ID == "" || block.Name == "" {
				return nil, errors.New("assistant history has invalid tool-call identity")
			}
			flush()
			currentType = ""
			items = append(items, llm.Item{Type: "tool_call", CallID: block.ID, Name: block.Name, Arguments: block.Arguments})
		default:
			return nil, fmt.Errorf("unsupported assistant history block %q", block.Type)
		}
	}
	flush()
	return items, nil
}

func transportUsage(response *llm.Response) *modeldata.ModelUsage {
	if response == nil || response.Usage == nil {
		return nil
	}
	usage := response.Usage
	return &modeldata.ModelUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens,
		CachedInputTokens: usage.CachedTokens, CacheCreationInputTokens: usage.CacheCreationTokens, ReasoningTokens: usage.ReasoningTokens}
}

func normalizeTransportError(err error, response *llm.Response) error {
	result := llm.NormalizeModelError("endpoint", "", err)
	result.OutputObserved = response != nil && len(response.Output) > 0
	if response != nil && response.Status != "" && response.Status != "completed" && result.Code == llm.ModelErrorUnknown {
		result.Code = llm.ModelErrorResponseIncomplete
	}
	return result
}
