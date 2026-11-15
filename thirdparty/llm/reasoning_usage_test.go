package llm

import "testing"

func TestDecodeResponsesPreservesReasoningUsageSubset(t *testing.T) {
	response, err := decodeResponses([]byte(`{"status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]}],"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"output_tokens_details":{"reasoning_tokens":12}}}`))
	if err != nil || response.Usage == nil || response.Usage.OutputTokens != 20 || response.Usage.ReasoningTokens != 12 ||
		len(response.Output) != 1 || response.Output[0].Type != "reasoning" {
		t.Fatalf("Responses reasoning usage/output = %+v, %v", response, err)
	}
}

func TestDecodeChatCompletionPreservesReasoningUsageSubset(t *testing.T) {
	response, err := decodeChatCompletion([]byte(`{"choices":[{"message":{"role":"assistant","content":"answer","reasoning_content":"think"}}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"completion_tokens_details":{"reasoning_tokens":12}}}`))
	if err != nil || response.Usage == nil || response.Usage.OutputTokens != 20 || response.Usage.ReasoningTokens != 12 ||
		len(response.Output) != 2 || response.Output[0].Type != "reasoning" {
		t.Fatalf("Chat reasoning usage/output = %+v, %v", response, err)
	}
}
