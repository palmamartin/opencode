package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sst/opencode/internal/llm/tools"
	"github.com/sst/opencode/internal/message"
)

const (
	CopilotCompletionURL = "https://api.githubcopilot.com/chat/completions"
	CopilotAuthURL       = "https://api.github.com/copilot_internal/v2/token"
	CopilotModelsURL     = "https://api.githubcopilot.com/models"
	DefaultModelID       = "gpt-4.1"
)

type copilotOptions struct {
	oauthToken string
}

type CopilotOption func(*copilotOptions)

type copilotClient struct {
	providerOptions providerClientOptions
	options         copilotOptions
	httpClient      *http.Client
	apiToken        *apiToken
}

type CopilotClient ProviderClient

type apiToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type apiTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

type chatRequest struct {
	Intent      bool          `json:"intent"`
	N           int           `json:"n"`
	Stream      bool          `json:"stream"`
	Temperature float32       `json:"temperature"`
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatTool    `json:"tools,omitempty"`
	ToolChoice  *string       `json:"tool_choice,omitempty"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    chatContent    `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatContent interface{}

type chatMessagePart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// chatToolChoice is a string type for tool choice options

type chatToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Function chatFunctionContent `json:"function"`
}

type chatFunctionContent struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responseEvent struct {
	ID      string           `json:"id"`
	Choices []responseChoice `json:"choices"`
}

type responseChoice struct {
	Index        int              `json:"index"`
	FinishReason *string          `json:"finish_reason"`
	Delta        *responseDelta   `json:"delta,omitempty"`
	Message      *responseMessage `json:"message,omitempty"`
}

type responseDelta struct {
	Content   *string         `json:"content"`
	Role      *string         `json:"role"`
	ToolCalls []toolCallChunk `json:"tool_calls,omitempty"`
}

type responseMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type toolCallChunk struct {
	Index    int            `json:"index"`
	ID       *string        `json:"id"`
	Function *functionChunk `json:"function"`
}

type functionChunk struct {
	Name      *string `json:"name"`
	Arguments *string `json:"arguments"`
}

func newCopilotClient(opts providerClientOptions) CopilotClient {
	copilotOpts := copilotOptions{}

	client := &copilotClient{
		providerOptions: opts,
		options:         copilotOpts,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}

	// Try to load OAuth token from GitHub config using auth helper
	auth := NewCopilotAuth()
	if token, err := auth.GetOAuthToken(); err == nil {
		client.options.oauthToken = token
	}

	return client
}

func (c *copilotClient) loadOAuthToken() string {
	auth := NewCopilotAuth()
	token, _ := auth.GetOAuthToken()
	return token
}

func (c *copilotClient) authenticate(ctx context.Context) error {
	if c.options.oauthToken == "" {
		auth := NewCopilotAuth()
		return fmt.Errorf("no GitHub OAuth token found. %s", auth.GetAuthenticationInstructions())
	}

	// Check if current API token is still valid
	if c.apiToken != nil && time.Until(c.apiToken.ExpiresAt) > 5*time.Minute {
		return nil
	}

	// Request new API token
	req, err := http.NewRequestWithContext(ctx, "GET", CopilotAuthURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create auth request: %w", err)
	}

	req.Header.Set("Authorization", "token "+c.options.oauthToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Zed)")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to authenticate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("authentication failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp apiTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("failed to decode auth response: %w", err)
	}

	c.apiToken = &apiToken{
		Token:     tokenResp.Token,
		ExpiresAt: time.Unix(tokenResp.ExpiresAt, 0),
	}

	return nil
}

func (c *copilotClient) send(ctx context.Context, messages []message.Message, tools []tools.BaseTool) (*ProviderResponse, error) {
	if err := c.authenticate(ctx); err != nil {
		return nil, err
	}

	request := c.buildChatRequest(messages, tools, false)

	reqBody, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", CopilotCompletionURL, strings.NewReader(string(reqBody)))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setRequestHeaders(req, false)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var response responseEvent
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return c.parseResponse(response), nil
}

func (c *copilotClient) stream(ctx context.Context, messages []message.Message, tools []tools.BaseTool) <-chan ProviderEvent {
	eventChan := make(chan ProviderEvent)

	go func() {
		defer close(eventChan)

		if err := c.authenticate(ctx); err != nil {
			eventChan <- ProviderEvent{Type: EventError, Error: err}
			return
		}

		request := c.buildChatRequest(messages, tools, true)

		reqBody, err := json.Marshal(request)
		if err != nil {
			eventChan <- ProviderEvent{Type: EventError, Error: fmt.Errorf("failed to marshal request: %w", err)}
			return
		}

		req, err := http.NewRequestWithContext(ctx, "POST", CopilotCompletionURL, strings.NewReader(string(reqBody)))
		if err != nil {
			eventChan <- ProviderEvent{Type: EventError, Error: fmt.Errorf("failed to create request: %w", err)}
			return
		}

		c.setRequestHeaders(req, true)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			eventChan <- ProviderEvent{Type: EventError, Error: fmt.Errorf("failed to send request: %w", err)}
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			eventChan <- ProviderEvent{Type: EventError, Error: fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))}
			return
		}

		c.processStreamingResponse(resp.Body, eventChan)
	}()

	return eventChan
}

func (c *copilotClient) setRequestHeaders(req *http.Request, isStreaming bool) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Zed)")
	req.Header.Set("Editor-Version", "Zed/0.189.5")
	req.Header.Set("Authorization", "Bearer "+c.apiToken.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")
	req.Header.Set("Copilot-Vision-Request", "false")
}

func (c *copilotClient) buildChatRequest(messages []message.Message, tools []tools.BaseTool, stream bool) chatRequest {
	modelName := string(c.providerOptions.model.APIModel)
	if modelName == "" {
		modelName = DefaultModelID
	}

	chatMessages := make([]chatMessage, 0, len(messages))

	for _, msg := range messages {
		chatMsg := c.convertMessage(msg)
		if chatMsg != nil {
			chatMessages = append(chatMessages, *chatMsg)
		}
	}

	request := chatRequest{
		Intent:      true,
		N:           1,
		Stream:      stream,
		Temperature: 0.7,
		Model:       modelName,
		Messages:    chatMessages,
	}

	// Temporarily disable tools to test basic functionality
	// if len(tools) > 0 {
	// 	request.Tools = c.convertTools(tools)
	// 	toolChoice := "auto"
	// 	request.ToolChoice = &toolChoice
	// }

	return request
}

func (c *copilotClient) convertMessage(msg message.Message) *chatMessage {
	switch msg.Role {
	case message.System:
		if content := msg.Content(); content != nil {
			return &chatMessage{
				Role:    "system",
				Content: content.Text,
			}
		}
	case message.User:
		return c.convertUserMessage(msg)
	case message.Assistant:
		return c.convertAssistantMessage(msg)
	case message.Tool:
		return c.convertToolMessage(msg)
	}
	return nil
}

func (c *copilotClient) convertUserMessage(msg message.Message) *chatMessage {
	content := msg.Content()
	images := msg.ImageURLContent()
	binaries := msg.BinaryContent()

	if len(images) == 0 && len(binaries) == 0 {
		// Simple text message
		if content != nil {
			return &chatMessage{
				Role:    "user",
				Content: content.Text,
			}
		}
		return nil
	}

	// Multipart message with images
	var parts []chatMessagePart
	if content != nil && content.Text != "" {
		parts = append(parts, chatMessagePart{
			Type: "text",
			Text: content.Text,
		})
	}

	for _, img := range images {
		parts = append(parts, chatMessagePart{
			Type: "image_url",
			ImageURL: &imageURL{
				URL: img.URL,
			},
		})
	}

	for _, binary := range binaries {
		parts = append(parts, chatMessagePart{
			Type: "image_url",
			ImageURL: &imageURL{
				URL: binary.String(c.providerOptions.model.Provider),
			},
		})
	}

	return &chatMessage{
		Role:    "user",
		Content: parts,
	}
}

func (c *copilotClient) convertAssistantMessage(msg message.Message) *chatMessage {
	chatMsg := &chatMessage{
		Role: "assistant",
	}

	if content := msg.Content(); content != nil {
		chatMsg.Content = content.Text
	}

	toolCalls := msg.ToolCalls()
	if len(toolCalls) > 0 {
		chatMsg.ToolCalls = make([]chatToolCall, len(toolCalls))
		for i, tc := range toolCalls {
			chatMsg.ToolCalls[i] = chatToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: chatFunctionContent{
					Name:      tc.Name,
					Arguments: tc.Input,
				},
			}
		}
	}

	return chatMsg
}

func (c *copilotClient) convertToolMessage(msg message.Message) *chatMessage {
	toolResults := msg.ToolResults()
	if len(toolResults) > 0 {
		// Use the first tool result
		tr := toolResults[0]
		return &chatMessage{
			Role:       "tool",
			Content:    tr.Content,
			ToolCallID: tr.ToolCallID,
		}
	}
	return nil
}

func (c *copilotClient) convertTools(tools []tools.BaseTool) []chatTool {
	chatTools := make([]chatTool, len(tools))
	for i, tool := range tools {
		info := tool.Info()
		chatTools[i] = chatTool{
			Type: "function",
			Function: chatFunction{
				Name:        info.Name,
				Description: info.Description,
				Parameters:  info.Parameters,
			},
		}
	}
	return chatTools
}

func (c *copilotClient) processStreamingResponse(body io.Reader, eventChan chan<- ProviderEvent) {
	scanner := bufio.NewScanner(body)
	var contentStarted bool
	var currentToolCalls = make(map[int]*message.ToolCall)

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if strings.HasPrefix(data, "[DONE]") {
			break
		}

		var event responseEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			slog.Warn("Failed to parse streaming response", "error", err, "data", data)
			continue
		}

		if len(event.Choices) == 0 {
			continue
		}

		choice := event.Choices[0]

		if choice.Delta != nil {
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				if !contentStarted {
					eventChan <- ProviderEvent{Type: EventContentStart}
					contentStarted = true
				}
				eventChan <- ProviderEvent{
					Type:    EventContentDelta,
					Content: *choice.Delta.Content,
				}
			}

			// Handle tool calls
			for _, toolCallChunk := range choice.Delta.ToolCalls {
				if toolCallChunk.ID != nil {
					// New tool call
					if _, exists := currentToolCalls[toolCallChunk.Index]; !exists {
						currentToolCalls[toolCallChunk.Index] = &message.ToolCall{
							ID:   *toolCallChunk.ID,
							Type: "function",
						}
						eventChan <- ProviderEvent{
							Type:     EventToolUseStart,
							ToolCall: currentToolCalls[toolCallChunk.Index],
						}
					}
				}

				if toolCall, exists := currentToolCalls[toolCallChunk.Index]; exists {
					if toolCallChunk.Function != nil {
						if toolCallChunk.Function.Name != nil {
							toolCall.Name = *toolCallChunk.Function.Name
						}
						if toolCallChunk.Function.Arguments != nil {
							toolCall.Input += *toolCallChunk.Function.Arguments
							eventChan <- ProviderEvent{
								Type:     EventToolUseDelta,
								ToolCall: toolCall,
							}
						}
					}
				}
			}
		}

		if choice.FinishReason != nil {
			// Finish any remaining tool calls
			for _, toolCall := range currentToolCalls {
				toolCall.Finished = true
				eventChan <- ProviderEvent{
					Type:     EventToolUseStop,
					ToolCall: toolCall,
				}
			}

			if contentStarted {
				eventChan <- ProviderEvent{Type: EventContentStop}
			}

			// Collect all tool calls for the final response
			var allToolCalls []message.ToolCall
			for _, tc := range currentToolCalls {
				allToolCalls = append(allToolCalls, *tc)
			}

			eventChan <- ProviderEvent{
				Type: EventComplete,
				Response: &ProviderResponse{
					ToolCalls:    allToolCalls,
					FinishReason: c.finishReason(*choice.FinishReason),
					Usage:        TokenUsage{}, // Copilot doesn't provide token usage in streaming
				},
			}
			break
		}
	}

	if err := scanner.Err(); err != nil {
		eventChan <- ProviderEvent{Type: EventError, Error: fmt.Errorf("error reading stream: %w", err)}
	}
}

func (c *copilotClient) parseResponse(response responseEvent) *ProviderResponse {
	if len(response.Choices) == 0 {
		return &ProviderResponse{
			FinishReason: message.FinishReasonError,
		}
	}

	choice := response.Choices[0]
	result := &ProviderResponse{
		Usage: TokenUsage{}, // Copilot doesn't provide token usage
	}

	if choice.Message != nil {
		result.Content = choice.Message.Content

		if len(choice.Message.ToolCalls) > 0 {
			result.ToolCalls = make([]message.ToolCall, len(choice.Message.ToolCalls))
			for i, tc := range choice.Message.ToolCalls {
				result.ToolCalls[i] = message.ToolCall{
					ID:       tc.ID,
					Name:     tc.Function.Name,
					Input:    tc.Function.Arguments,
					Type:     tc.Type,
					Finished: true,
				}
			}
		}
	}

	if choice.FinishReason != nil {
		result.FinishReason = c.finishReason(*choice.FinishReason)
	} else {
		result.FinishReason = message.FinishReasonEndTurn
	}

	return result
}

func (c *copilotClient) finishReason(reason string) message.FinishReason {
	switch reason {
	case "stop":
		return message.FinishReasonEndTurn
	case "length":
		return message.FinishReasonMaxTokens
	case "tool_calls":
		return message.FinishReasonToolUse
	default:
		return message.FinishReasonUnknown
	}
}

func WithCopilotOAuthToken(token string) CopilotOption {
	return func(options *copilotOptions) {
		options.oauthToken = token
	}
}
