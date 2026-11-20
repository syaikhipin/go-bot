package agent

import (
	"encoding/json"
	"fmt"
	"go-bot/harness/content"
	"go-bot/internal/modeldata"
	"strings"
)

func pipelineCodecFailure(code, message string) error { return fmt.Errorf("%s: %s", code, message) }
func EncodeToolPipelineResult(result modeldata.ToolResult) (ToolPipelineResult, error) {
	output := ToolPipelineResult{IsError: result.IsError, Meta: result.Meta, AdditionalContexts: result.AdditionalContexts, ConcludesTurn: result.ConcludeTurn, Content: []json.RawMessage{}}
	if result.IsError {
		output.Error = &ToolPipelineError{Message: strings.TrimPrefix(result.Content, "Error: ")}
		if output.Error.Message == "" {
			var text []string
			for _, block := range result.ContentBlocks {
				if block.Type == "text" {
					text = append(text, block.Text)
				}
			}
			output.Error.Message = strings.TrimPrefix(strings.Join(text, "\n"), "Error: ")
		}
		if result.Code != "" {
			output.Error.Info = &ToolPipelineErrorInfo{Name: result.ErrorName, Code: result.Code, Reason: result.ErrorReason}
			if output.Error.Info.Name == "" {
				output.Error.Info.Name = "Error"
				if result.Code == "TOOL_TIMEOUT" {
					output.Error.Info.Name = "ToolTimeoutError"
				}
			}
		}
	} else {
		output.Value, _ = json.Marshal(result.Content)
		var meta struct {
			CanonicalValue json.RawMessage `json:"canonicalValue"`
		}
		if json.Unmarshal(result.Meta, &meta) == nil && json.Valid(meta.CanonicalValue) {
			output.Value = meta.CanonicalValue
		}
	}
	blocks := append([]content.ContentBlock(nil), result.ContentBlocks...)
	if result.Content != "" {
		blocks = append([]content.ContentBlock{{Type: "text", Text: result.Content}}, blocks...)
	}
	for _, block := range blocks {
		var value any
		switch block.Type {
		case "text":
			value = map[string]any{"type": "text", "text": block.Text}
		case "image":
			attachment := map[string]any{"attachmentId": block.ImageRefID, "mediaType": block.MIMEType, "bytes": block.Bytes, "width": block.Width, "height": block.Height}
			if block.Name != "" {
				attachment["name"] = block.Name
			}
			if block.OriginalDimensions != nil {
				attachment["originalDimensions"] = block.OriginalDimensions
			}
			value = map[string]any{"type": "image", "attachment": attachment}
		default:
			return ToolPipelineResult{}, pipelineCodecFailure("INVALID_RESULT", "Unsupported tool policy content type")
		}
		data, err := json.Marshal(value)
		if err != nil {
			return ToolPipelineResult{}, err
		}
		output.Content = append(output.Content, data)
	}
	return output, nil
}

func DecodeToolPipelineResult(output ToolPipelineResult) (modeldata.ToolResult, error) {
	result := modeldata.ToolResult{IsError: output.IsError, Meta: output.Meta, AdditionalContexts: output.AdditionalContexts, ConcludeTurn: output.ConcludesTurn}
	if output.IsError {
		if output.Error == nil {
			return result, pipelineCodecFailure("INVALID_RESULT", "Failed tool policy result lacks an error")
		}
		if output.Error.Info != nil {
			result.ErrorName, result.Code = output.Error.Info.Name, output.Error.Info.Code
			result.ErrorReason = output.Error.Info.Reason
		}
	} else if !json.Valid(output.Value) {
		return result, pipelineCodecFailure("INVALID_RESULT", "Successful tool policy result lacks a value")
	}
	for _, raw := range output.Content {
		var block struct {
			Type       string `json:"type"`
			Text       string `json:"text"`
			Attachment struct {
				ID                 string                   `json:"attachmentId"`
				MIMEType           string                   `json:"mediaType"`
				Bytes              int64                    `json:"bytes"`
				Width              int                      `json:"width"`
				Height             int                      `json:"height"`
				Name               string                   `json:"name"`
				OriginalDimensions *content.ImageDimensions `json:"originalDimensions"`
			} `json:"attachment"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			return result, err
		}
		switch block.Type {
		case "text":
			if block.Text != "" {
				result.ContentBlocks = append(result.ContentBlocks, content.ContentBlock{Type: "text", Text: block.Text})
			}
		case "image":
			a := block.Attachment
			result.ContentBlocks = append(result.ContentBlocks, content.ImageBlock(content.ImageAttachment{ID: a.ID, MIMEType: a.MIMEType, Bytes: a.Bytes, Width: a.Width, Height: a.Height, Name: a.Name, OriginalDimensions: a.OriginalDimensions}))
		default:
			return result, pipelineCodecFailure("INVALID_RESULT", "Unsupported tool policy content type: "+block.Type)
		}
	}
	if err := content.ValidateDurableContentBlocks(result.ContentBlocks); err != nil {
		return result, err
	}
	// Retain the single-text legacy projection without duplicating model text.
	if len(result.ContentBlocks) == 1 && result.ContentBlocks[0].Type == "text" {
		result.Content = result.ContentBlocks[0].Text
		result.ContentBlocks = nil
	}
	return result, nil
}
