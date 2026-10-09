package meridian_test

import (
	"testing"

	"meridian.local/meridian-go"
	"meridian.local/meridian-go/internal/gen/public"
)

func TestPublicTypesConformToThePinnedSpec(t *testing.T) {
	assertConforms(t, "ChatCompletionRequest", meridian.ChatParams{}, public.ChatCompletionRequest{}, "stream", "stream_options")
	assertConforms(t, "ChatMessage", meridian.ChatMessage{}, public.ChatMessage{})
	assertConforms(t, "ImageUrl", meridian.ImageURL{}, public.ImageUrl{})
	assertConforms(t, "Tool", meridian.Tool{}, public.Tool{})
	assertConforms(t, "ToolFunction", meridian.ToolFunction{}, public.ToolFunction{})
	assertConforms(t, "ToolCall", meridian.ToolCall{}, public.ToolCall{})
	assertConforms(t, "ToolCallFunction", meridian.ToolCallFunction{}, public.ToolCallFunction{})
	assertConforms(t, "ChatCompletion", meridian.ChatCompletion{}, public.ChatCompletion{})
	assertConforms(t, "ChatChoice", meridian.ChatChoice{}, public.ChatChoice{})
	assertConforms(t, "AssistantMessage", meridian.AssistantMessage{}, public.AssistantMessage{})
	assertConforms(t, "ChatCompletionChunk", meridian.ChatChunk{}, public.ChatCompletionChunk{})
	assertConforms(t, "ChunkChoice", meridian.ChunkChoice{}, public.ChunkChoice{})
	assertConforms(t, "ChunkDelta", meridian.ChunkDelta{}, public.ChunkDelta{})
	assertConforms(t, "ToolCallDelta", meridian.ToolCallDelta{}, public.ToolCallDelta{})
	assertConforms(t, "ToolCallDeltaFunction", meridian.ToolCallDeltaFunction{}, public.ToolCallDeltaFunction{})
	assertConforms(t, "Usage", meridian.Usage{}, public.Usage{}, "prompt_tokens_details", "completion_tokens_details")
	assertConforms(t, "ModelObject", meridian.Model{}, public.ModelObject{})
}
