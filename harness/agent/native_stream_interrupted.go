package agent

import (
	"encoding/json"
	"strings"

	"go-bot/internal/modeldata"
)

type nativePartialBlock struct {
	kind string
	text string
}

// nativeInterruptedPrefix mirrors DeepSeek's BlockAssembler.interruptedBlocks:
// retain nonblank text/reasoning in first-seen order and never surface a
// partially streamed tool call after cancellation.
func nativeInterruptedPrefix(records []any) (string, []modeldata.AssistantOutputBlock, *modeldata.ModelUsage) {
	order := make([]int, 0)
	blocks := make(map[int]*nativePartialBlock)
	var usage *modeldata.ModelUsage
	ensure := func(index int, kind string) *nativePartialBlock {
		block := blocks[index]
		if block == nil {
			block = &nativePartialBlock{kind: kind}
			blocks[index] = block
			order = append(order, index)
		}
		return block
	}
	for _, raw := range records {
		record, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := record["type"].(string); kind == "text-chunks" || kind == "reasoning-chunks" {
			index, ok := record["index"].(int)
			texts, valid := record["texts"].([]string)
			if !ok || !valid {
				continue
			}
			blockType := "text"
			if kind == "reasoning-chunks" {
				blockType = "reasoning"
			}
			block := ensure(index, blockType)
			for _, text := range texts {
				block.text += text
			}
			continue
		}
		chunk, ok := record["chunk"].(map[string]any)
		if !ok {
			continue
		}
		kind, _ := chunk["type"].(string)
		index, _ := chunk["index"].(float64)
		switch kind {
		case "block-start":
			blockType, _ := chunk["blockType"].(string)
			ensure(int(index), blockType)
		case "text-delta", "reasoning-delta":
			blockType := "text"
			if kind == "reasoning-delta" {
				blockType = "reasoning"
			}
			text, _ := chunk["text"].(string)
			ensure(int(index), blockType).text += text
		case "block-end":
			full, ok := chunk["block"].(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := full["type"].(string)
			if blockType == "text" || blockType == "reasoning" {
				text, _ := full["text"].(string)
				block := ensure(int(index), blockType)
				block.kind, block.text = blockType, text
			}
		case "usage":
			if rawUsage, ok := chunk["usage"]; ok {
				encoded, err := json.Marshal(rawUsage)
				if err != nil {
					continue
				}
				var value struct {
					InputTokens      int64 `json:"inputTokens"`
					OutputTokens     int64 `json:"outputTokens"`
					TotalTokens      int64 `json:"totalTokens"`
					CacheReadTokens  int64 `json:"cacheReadTokens"`
					CacheWriteTokens int64 `json:"cacheWriteTokens"`
					ReasoningTokens  int64 `json:"reasoningTokens"`
				}
				if json.Unmarshal(encoded, &value) == nil {
					usage = &modeldata.ModelUsage{InputTokens: value.InputTokens + value.CacheReadTokens + value.CacheWriteTokens,
						OutputTokens: value.OutputTokens, TotalTokens: value.TotalTokens,
						CachedInputTokens: value.CacheReadTokens, CacheCreationInputTokens: value.CacheWriteTokens,
						ReasoningTokens: value.ReasoningTokens}
				}
			}
		}
	}
	var content strings.Builder
	result := make([]modeldata.AssistantOutputBlock, 0, len(order))
	for _, index := range order {
		block := blocks[index]
		if (block.kind != "text" && block.kind != "reasoning") || strings.TrimSpace(block.text) == "" {
			continue
		}
		result = append(result, modeldata.AssistantOutputBlock{Type: block.kind, Text: block.text})
		if block.kind == "text" {
			content.WriteString(block.text)
		}
	}
	return content.String(), result, usage
}
