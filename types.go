package meridian

import "encoding/json"

// Role is a message role. The engine forwards any role as sent; these are the ones providers accept.
type Role string

// Message roles.
const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ChatParams is a chat request. Leave Model empty to let Meridian choose (D-25).
type ChatParams struct {
	Messages            []ChatMessage `json:"messages,omitempty"`
	Model               string        `json:"model,omitempty"`
	Temperature         *float64      `json:"temperature,omitempty"`
	TopP                *float64      `json:"top_p,omitempty"`
	MaxTokens           *int          `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int          `json:"max_completion_tokens,omitempty"`
	N                   *int          `json:"n,omitempty"`
	Stop                any           `json:"stop,omitempty"`
	Tools               []Tool        `json:"tools,omitempty"`
	ToolChoice          any           `json:"tool_choice,omitempty"`
	ResponseFormat      any           `json:"response_format,omitempty"`
	User                string        `json:"user,omitempty"`

	// IdempotencyKey is sent on every retry of this call; empty means the SDK generates one.
	IdempotencyKey string `json:"-"`
	// Extra holds request fields the engine forwards to the provider. Known fields and the
	// stream flags always win over Extra.
	Extra map[string]any `json:"-"`
}

// ChatMessage is one message. Content is a string, a []ContentPart, or nil.
type ChatMessage struct {
	Role       Role       `json:"role"`
	Content    any        `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ContentPart is one part of a multi-part message.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL is an image part's source. Images need a vision model.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// TextPart is a text content part.
func TextPart(text string) ContentPart { return ContentPart{Type: "text", Text: text} }

// ImagePart is an image content part (an https or data URL).
func ImagePart(url string) ContentPart {
	return ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: url}}
}

// Tool is a function the model may call.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function. Parameters is a JSON Schema object.
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// FunctionTool is a function tool.
func FunctionTool(name, description string, parameters any) Tool {
	return Tool{Type: "function", Function: ToolFunction{Name: name, Description: description, Parameters: parameters}}
}

// ToolCall is a call the model made.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the called function and its JSON arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatCompletion is a buffered chat reply.
type ChatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`

	// RequestID is the engine's request id (the X-Request-Id header), not part of the reply body.
	RequestID string `json:"-"`
}

// Text is the first choice's content, or "".
func (c *ChatCompletion) Text() string {
	if len(c.Choices) == 0 || c.Choices[0].Message.Content == nil {
		return ""
	}

	return *c.Choices[0].Message.Content
}

// ChatChoice is one choice of a buffered reply.
type ChatChoice struct {
	Index        int              `json:"index"`
	Message      AssistantMessage `json:"message"`
	FinishReason string           `json:"finish_reason"`
}

// AssistantMessage is the model's message.
type AssistantMessage struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	Reasoning        *string    `json:"reasoning,omitempty"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	Refusal          *string    `json:"refusal,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// ChatChunk is one streamed chunk.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// Text is the first choice's new content, or "".
func (c ChatChunk) Text() string {
	if len(c.Choices) == 0 || c.Choices[0].Delta.Content == nil {
		return ""
	}

	return *c.Choices[0].Delta.Content
}

// ChunkChoice is one choice of a chunk.
type ChunkChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// ChunkDelta is what a chunk adds.
type ChunkDelta struct {
	Role             *string         `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	Reasoning        *string         `json:"reasoning,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	Refusal          *string         `json:"refusal,omitempty"`
	ToolCalls        []ToolCallDelta `json:"tool_calls,omitempty"`
}

// ToolCallDelta is part of a streamed tool call.
type ToolCallDelta struct {
	Index    int                    `json:"index"`
	ID       *string                `json:"id,omitempty"`
	Type     *string                `json:"type,omitempty"`
	Function *ToolCallDeltaFunction `json:"function,omitempty"`
}

// ToolCallDeltaFunction is part of a streamed function call.
type ToolCallDeltaFunction struct {
	Name      *string `json:"name,omitempty"`
	Arguments *string `json:"arguments,omitempty"`
}

// Usage is the reply's token counts. A count the engine did not report is nil, never 0.
// There is never a price or cost (D-27).
type Usage struct {
	PromptTokens     *int `json:"prompt_tokens,omitempty"`
	CompletionTokens *int `json:"completion_tokens,omitempty"`
	TotalTokens      *int `json:"total_tokens,omitempty"`
}

// Model is a model the key may use.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// key is the idempotency key for this call. A caller's key that a header cannot carry intact
// is refused before any request, never silently replaced.
func (p ChatParams) key() (string, *Error) {
	switch {
	case p.IdempotencyKey == "":
		return newIdempotencyKey()
	case !validIdempotencyKey(p.IdempotencyKey):
		return "", configError("IdempotencyKey must be 1 to 255 printable ASCII characters and not all spaces.")
	}

	return p.IdempotencyKey, nil
}

// body is the JSON request: Extra first, then known fields, then the SDK's stream flags.
func (p ChatParams) body(stream bool) (map[string]json.RawMessage, *Error) {
	// [1] Extra fields first, so known fields and stream flags always win.
	out := make(map[string]json.RawMessage, len(p.Extra)+8)

	for k, v := range p.Extra {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, configError("Extra field " + k + " could not be encoded as JSON: " + err.Error())
		}

		out[k] = raw
	}

	// [2] Known fields; empty ones are omitted by their tags.
	known, err := json.Marshal(p)
	if err != nil {
		return nil, configError("The chat request could not be encoded as JSON: " + err.Error())
	}

	var fields map[string]json.RawMessage
	_ = json.Unmarshal(known, &fields)

	for k, v := range fields {
		out[k] = v
	}

	// [3] The SDK owns the stream flags.
	if stream {
		out["stream"] = json.RawMessage("true")
		out["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	} else {
		out["stream"] = json.RawMessage("false")
		delete(out, "stream_options")
	}

	return out, nil
}
