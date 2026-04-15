package main

// mcp.go — Model Context Protocol server embedded in the enterprise_chat HTTP server.
//
// This file sets up an MCP server using the official go-sdk and exposes it
// over the Streamable HTTP transport at POST /v1/mcp.
//
// The MCP server is a singleton (built once at startup) that all HTTP requests
// share — the StreamableHTTPHandler manages per-session state internally.
//
// Tools registered here:
//   - calculator: add, subtract, multiply, divide two numbers (smoke-test tool)
//
// To add more tools, call mcp.AddTool(MCPServer, ...) in InitMCPServer()
// before InitRoutes() mounts the handler.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
)

// MCPServer is the singleton MCP server instance shared across all sessions.
var MCPServer *mcp.Server

// MCPHandler is the HTTP handler returned by NewStreamableHTTPHandler.
// It implements http.Handler so it can be mounted directly into the ServeMux.
var MCPHandler *mcp.StreamableHTTPHandler

// ─────────────────────────────────────────────────────────────────────────────
// Tool input/output types
// ─────────────────────────────────────────────────────────────────────────────

// CalculatorInput holds the two operands and the operation for the calculator tool.
type CalculatorInput struct {
	Operation string  `json:"operation" jsonschema:"The arithmetic operation to perform (add, subtract, multiply, divide)"`
	A         float64 `json:"a"         jsonschema:"The first operand"`
	B         float64 `json:"b"         jsonschema:"The second operand"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Calculate is the shared arithmetic implementation used by both:
//   - The MCP handler (for external MCP clients via /v1/mcp)
//   - The OpenAI tool-call loop in text_generation.go (for the chat UI)
//
// Returns a human-readable result string, or an error string with ok=false.
// ─────────────────────────────────────────────────────────────────────────────

func Calculate(operation string, a, b float64) (result string, ok bool) {
	switch operation {
	case "add":
		return fmt.Sprintf("%.6g + %.6g = %.6g", a, b, a+b), true
	case "subtract":
		return fmt.Sprintf("%.6g - %.6g = %.6g", a, b, a-b), true
	case "multiply":
		return fmt.Sprintf("%.6g × %.6g = %.6g", a, b, a*b), true
	case "divide":
		if b == 0 {
			return "Error: division by zero", false
		}
		return fmt.Sprintf("%.6g ÷ %.6g = %.6g", a, b, a/b), true
	default:
		return fmt.Sprintf("Error: unknown operation %q; valid values: add, subtract, multiply, divide", operation), false
	}
}

// calculatorTool is the MCP-protocol wrapper around Calculate().
// It satisfies the mcp.AddTool handler signature.
func calculatorTool(
	_ context.Context,
	_ *mcp.CallToolRequest,
	input CalculatorInput,
) (*mcp.CallToolResult, mcp.TextContent, error) {
	text, ok := Calculate(input.Operation, input.A, input.B)
	if !ok {
		return &mcp.CallToolResult{IsError: true}, mcp.TextContent{Text: text}, nil
	}
	return nil, mcp.TextContent{Text: text}, nil
}

// WebSearchInput holds the search queries for the web_search MCP tool.
type WebSearchInput struct {
	Queries []string `json:"queries" jsonschema:"A list of search queries to find relevant information"`
}

// webSearchMCPTool is the MCP-protocol wrapper around executeParallelSearches.
// It is called by external MCP clients (Claude Desktop, Cursor, agents) via /v1/mcp.
// The chat UI tool-calling loop in text_generation.go handles the same tool
// autonomously through the OpenAI-compatible path.
func webSearchMCPTool(
	_ context.Context,
	_ *mcp.CallToolRequest,
	input WebSearchInput,
) (*mcp.CallToolResult, mcp.TextContent, error) {
	if len(input.Queries) == 0 {
		return &mcp.CallToolResult{IsError: true}, mcp.TextContent{Text: "no queries provided"}, nil
	}

	// Use a simple log-only progress callback — MCP tool calls are synchronous
	// request/response so we can't stream progress updates to the client.
	progressCb := func(msg string) { log.Printf("[mcp:web_search] %s", msg) }

	contextText, results := executeParallelSearches(input.Queries, true, progressCb)

	// Append numbered source list so the LLM / caller can cite correctly.
	var sb strings.Builder
	sb.WriteString(contextText)
	if len(results) > 0 {
		sb.WriteString("\n\n---\n**Sources:**\n")
		for i, r := range results {
			sb.WriteString(fmt.Sprintf("[%d] [%s](%s)\n", i+1, r.Title, r.URL))
		}
	}

	return nil, mcp.TextContent{Text: sb.String()}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// search_my_documents — RAG tool for MCP clients
// ─────────────────────────────────────────────────────────────────────────────

// SearchMyDocsInput is the input for the search_my_documents MCP tool.
type SearchMyDocsInput struct {
	Query         string   `json:"query"          jsonschema:"The question or topic to search for in the user's uploaded documents"`
	DocumentNames []string `json:"document_names" jsonschema:"Optional list of specific document names to restrict search to. Omit to search all accessible documents."`
	TopN          int      `json:"top_n"          jsonschema:"Maximum number of chunks to return (default 5)"`
}

// searchMyDocumentsMCPTool runs hybrid search → RRF → rerank scoped to the
// authenticated user's documents (own + global + shared).
// The userId is read from context, which ApiAuthMiddleware populates.
func searchMyDocumentsMCPTool(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input SearchMyDocsInput,
) (*mcp.CallToolResult, mcp.TextContent, error) {
	userId := userIdFromContext(ctx)
	if userId == 0 {
		return &mcp.CallToolResult{IsError: true},
			mcp.TextContent{Text: "Error: could not resolve user identity from request context"},
			nil
	}
	if strings.TrimSpace(input.Query) == "" {
		return &mcp.CallToolResult{IsError: true},
			mcp.TextContent{Text: "Error: query must not be empty"},
			nil
	}
	topN := input.TopN
	if topN <= 0 {
		topN = 5
	}

	log.Printf("[mcp:search_my_documents] user=%d query=%q docs=%v", userId, input.Query, input.DocumentNames)

	// If no explicit doc list, fetch all document names this user can access.
	targetDocs := input.DocumentNames
	if len(targetDocs) == 0 {
		rows, err := ClickhouseQuery(
			`SELECT document_name FROM user_documents
			 WHERE user_id = ? OR is_global = true OR has(shared_with_user_ids, toUInt32(?))`,
			userId, userId,
		)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var name string
				if rows.Scan(&name) == nil {
					targetDocs = append(targetDocs, name)
				}
			}
		}
	}

	if len(targetDocs) == 0 {
		return nil, mcp.TextContent{Text: "No documents found for this user. Upload documents first via the chat interface."}, nil
	}

	// Embed the query
	queryEmbedding, err := getQueryEmbedding(input.Query)
	if err != nil {
		return &mcp.CallToolResult{IsError: true},
			mcp.TextContent{Text: fmt.Sprintf("Error embedding query: %v", err)},
			nil
	}

	// Hybrid search → RRF → rerank
	denseResults, sparseResults, err := hybridSearch(input.Query, queryEmbedding, targetDocs, 20)
	if err != nil {
		return &mcp.CallToolResult{IsError: true},
			mcp.TextContent{Text: fmt.Sprintf("Error searching documents: %v", err)},
			nil
	}

	fusedChunks := performRRF(denseResults, sparseResults, 60, 15)
	finalChunks := rerankChunks(input.Query, fusedChunks, topN)

	if len(finalChunks) == 0 {
		return nil, mcp.TextContent{Text: "No relevant content found in your documents for this query."}, nil
	}

	// Format output with source citations
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d relevant passage(s) from your documents:\n\n", len(finalChunks)))
	for i, chunk := range finalChunks {
		sb.WriteString(fmt.Sprintf("[Source %d] (Document: %s, Type: %s)\n%s\n\n",
			i+1, chunk.DocumentName, chunk.ChunkType, chunk.Content))
	}
	return nil, mcp.TextContent{Text: strings.TrimSpace(sb.String())}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// image_generation — MCP Tool for generating images
// ─────────────────────────────────────────────────────────────────────────────

type ImageGenerationInput struct {
	Prompt string `json:"prompt" jsonschema:"The text description of the image to generate"`
	Size   string `json:"size"   jsonschema:"The size of the image, e.g., '1024x1024' or '512x512' (default 1024x1024)"`
	Steps  int    `json:"steps"  jsonschema:"The number of inference steps for quality (default 20)"`
}

func imageGenerationMCPTool(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ImageGenerationInput,
) (*mcp.CallToolResult, mcp.TextContent, error) {
	if input.Prompt == "" {
		return &mcp.CallToolResult{IsError: true}, mcp.TextContent{Text: "Error: prompt must not be empty"}, nil
	}
	if input.Size == "" {
		input.Size = "1024x1024"
	}
	if input.Steps <= 0 {
		input.Steps = 20
	}

	baseURL := ""
	if v, ok := ctx.Value("baseURL").(string); ok {
		baseURL = v
	}

	url, err := GenerateImageFromTool(input.Prompt, input.Size, input.Steps, baseURL)
	if err != nil {
		return &mcp.CallToolResult{IsError: true}, mcp.TextContent{Text: fmt.Sprintf("Error generating image: %v", err)}, nil
	}

	// Make sure we return JSON with url format as required by the LLM
	resBytes, _ := json.Marshal(map[string]string{"url": url})
	return nil, mcp.TextContent{Text: string(resBytes)}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// InitMCPServer builds the server and HTTP handler.
// Call this once from main() before InitRoutes().
// ─────────────────────────────────────────────────────────────────────────────

func InitMCPServer() {
	// Create the MCP server instance
	MCPServer = mcp.NewServer(&mcp.Implementation{
		Name:    "enterprise-chat-mcp",
		Version: "v1.0.0",
	}, nil)

	// ── Register tools ────────────────────────────────────────────────────────
	mcp.AddTool(MCPServer, &mcp.Tool{
		Name:        "calculator",
		Description: "Perform basic arithmetic (add, subtract, multiply, divide) on two numbers.",
	}, calculatorTool)

	mcp.AddTool(MCPServer, &mcp.Tool{
		Name:        "web_search",
		Description: "Search the internet for real-time information, current events, latest news, or specific facts the model doesn't know. Returns crawled page content and source URLs.",
	}, webSearchMCPTool)

	mcp.AddTool(MCPServer, &mcp.Tool{
		Name:        "search_my_documents",
		Description: "Search the user's uploaded documents using semantic + keyword hybrid search with reranking. Use this when the user asks about something that might be in their uploaded files, reports, or shared documents.",
	}, searchMyDocumentsMCPTool)

	mcp.AddTool(MCPServer, &mcp.Tool{
		Name:        "image_generation",
		Description: "Generate an image based on a text prompt. Returns a JSON containing the URL of the generated image. You should reply to the user with a markdown image using the returned URL: ![Generated Image](<url>).",
	}, imageGenerationMCPTool)

	// ── Create the Streamable HTTP handler (MCP 2025-03-26 compliant) ─────────
	// NewStreamableHTTPHandler wraps the server so every incoming HTTP request
	// is routed to the appropriate MCP session or creates a new one.
	MCPHandler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return MCPServer
	}, nil)
}

// ─────────────────────────────────────────────────────────────────────────────
// MCP Tool Registry Adapter
// ─────────────────────────────────────────────────────────────────────────────

// GetOpenAIToolsFromMCP extracts the tools defined in the MCP server and maps
// them into the OpenAI ChatGPT completion format so the LLM can use them.
// The webSearch toggle dictates whether the web_search tool's description
// actively encourages searching (true) or defaults to passive (false).
func GetOpenAIToolsFromMCP(webSearch bool) []openai.ChatCompletionToolParam {
	if MCPServer == nil {
		return nil
	}

	// Because we can't directly inspect MCPServer.tools, we know the exact tools
	// we've registered in InitMCPServer. We maintain this static list of definitions
	// while still using MCP to execute them.

	wsDescription := "Search the internet when the user asks for current events, latest news, real-time data, or information the model may not know. Use your judgment — only call this tool when fresh web data is needed."
	if webSearch {
		wsDescription = "The user has enabled web search. Actively search the internet to answer this query with the most up-to-date information. Prefer real web results over your training data."
	}

	rawTools := []string{
		fmt.Sprintf(`[{
			"type": "function",
			"function": {
				"name": "web_search",
				"description": %q,
				"parameters": {
					"type": "object",
					"properties": {
						"queries": {
							"type": "array",
							"items": {"type": "string"},
							"description": "A list of specific search queries to find relevant information."
						}
					},
					"required": ["queries"]
				}
			}
		}]`, wsDescription),
		`[{
			"type": "function",
			"function": {
				"name": "calculator",
				"description": "Perform precise arithmetic: add, subtract, multiply, or divide two numbers. Use this whenever the user asks to calculate, compute, or evaluate a math expression.",
				"parameters": {
					"type": "object",
					"properties": {
						"operation": {
							"type": "string",
							"enum": ["add", "subtract", "multiply", "divide"],
							"description": "The arithmetic operation to perform."
						},
						"a": { "type": "number", "description": "The first operand." },
						"b": { "type": "number", "description": "The second operand." }
					},
					"required": ["operation", "a", "b"]
				}
			}
		}]`,
		`[{
			"type": "function",
			"function": {
				"name": "search_my_documents",
				"description": "Search the user's uploaded documents using semantic + keyword hybrid search. Use this when the user asks about something that might be in their uploaded files, reports, or shared documents.",
				"parameters": {
					"type": "object",
					"properties": {
						"query": {
							"type": "string",
							"description": "The exact question to search for in the documents."
						},
						"document_names": {
							"type": "array",
							"items": {"type": "string"},
							"description": "Optional list of document names to restrict the search to. Omit to search all accessible documents."
						},
						"top_n": {
							"type": "integer",
							"description": "Maximum number of chunks to return (default 5)"
						}
					},
					"required": ["query"]
				}
			}
		}]`,
		`[{
			"type": "function",
			"function": {
				"name": "image_generation",
				"description": "Generate an image based on a prompt. Returns the generated image URL as JSON. You MUST reply to the user using markdown to render the image: ![Generated Image](<url>)",
				"parameters": {
					"type": "object",
					"properties": {
						"prompt": {
							"type": "string",
							"description": "A detailed text description of the image to generate."
						},
						"size": {
							"type": "string",
							"description": "The dimensions of the image, e.g. '1024x1024'. Default is '1024x1024'."
						},
						"steps": {
							"type": "integer",
							"description": "Number of inference steps (quality). Default is 20."
						}
					},
					"required": ["prompt"]
				}
			}
		}]`,
	}

	var results []openai.ChatCompletionToolParam
	for _, raw := range rawTools {
		var toolList []openai.ChatCompletionToolParam
		if err := json.Unmarshal([]byte(raw), &toolList); err == nil && len(toolList) > 0 {
			results = append(results, toolList[0])
		}
	}
	return results
}

// DispatchMCPTool calls the underlying tool handler that was registered on the MCP server.
// Since MCPServer.callTool is private, we route manually to the known functions.
func DispatchMCPTool(ctx context.Context, req *mcp.CallToolRequest, progressCb func(string)) (*mcp.CallToolResult, error) {
	name := req.Params.Name
	switch name {
	case "calculator":
		var in CalculatorInput
		b, _ := json.Marshal(req.Params.Arguments)
		json.Unmarshal(b, &in)
		if progressCb != nil {
			progressCb(fmt.Sprintf("[calculator] Evaluating: %g %s %g", in.A, in.Operation, in.B))
		}
		res, txt, err := calculatorTool(ctx, req, in)
		if err != nil {
			return nil, err
		}
		if res != nil && res.IsError {
			return res, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&txt}}, nil

	case "web_search":
		var in WebSearchInput
		b, _ := json.Marshal(req.Params.Arguments)
		json.Unmarshal(b, &in)
		if progressCb != nil {
			progressCb("[search] executing optimal search queries...")
		}
		res, txt, err := webSearchMCPTool(ctx, req, in)
		if err != nil {
			return nil, err
		}
		if res != nil && res.IsError {
			return res, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&txt}}, nil

	case "search_my_documents":
		var in SearchMyDocsInput
		b, _ := json.Marshal(req.Params.Arguments)
		json.Unmarshal(b, &in)
		if progressCb != nil {
			progressCb(fmt.Sprintf("[search_my_documents] Searching user documents for: %s", in.Query))
		}
		res, txt, err := searchMyDocumentsMCPTool(ctx, req, in)
		if err != nil {
			return nil, err
		}
		if res != nil && res.IsError {
			return res, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&txt}}, nil

	case "image_generation":
		var in ImageGenerationInput
		b, _ := json.Marshal(req.Params.Arguments)
		json.Unmarshal(b, &in)
		if progressCb != nil {
			progressCb(fmt.Sprintf("[image_generation] Generating image for: %s", in.Prompt))
		}
		res, txt, err := imageGenerationMCPTool(ctx, req, in)
		if err != nil {
			return nil, err
		}
		if res != nil && res.IsError {
			return res, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&txt}}, nil

	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}


// MergeTools combines ad-hoc tools from a request with the dynamic tools from the MCP server.
func MergeTools(adHocTools json.RawMessage, webSearch bool) []openai.ChatCompletionToolParam {
	dynamicTools := GetOpenAIToolsFromMCP(webSearch)
	var mergedTools []openai.ChatCompletionToolParam
	if len(adHocTools) > 0 && string(adHocTools) != "null" {
		var toolList []openai.ChatCompletionToolParam
		if err := json.Unmarshal(adHocTools, &toolList); err == nil && len(toolList) > 0 {
			mergedTools = append(mergedTools, toolList...)
		}
	}
	mergedTools = append(mergedTools, dynamicTools...)
	return mergedTools
}

type AgentParams struct {
	Messages              []openai.ChatCompletionMessageParamUnion
	Model                 string
	MaxTokens             int64
	Stream                bool
	ResponseWriter        http.ResponseWriter
	Flusher               http.Flusher
	EstimatedPayloadBytes int
	Tools                 []openai.ChatCompletionToolParam
	Client                *openai.Client
	RequestCtx            context.Context
}

// RunAgentLoop executes the tool-calling orchestration loop
func RunAgentLoop(ctx context.Context, p AgentParams) {
	// Timeout context: prevents the handler from hanging indefinitely when llama_cpp stalls.
	llmCtx, llmCancel := context.WithTimeout(p.RequestCtx, 5*time.Minute)
	defer llmCancel()

	params := openai.ChatCompletionNewParams{
		Model:               openai.ChatModel(p.Model),
		Messages:            p.Messages,
		MaxCompletionTokens: param.NewOpt(p.MaxTokens),
	}
	if len(p.Tools) > 0 {
		params.Tools = p.Tools
	}

	canFlush := p.Flusher != nil

	progressCb := func(msg string) {
		log.Println(msg)
		if p.Stream {
			chunk := map[string]interface{}{
				"choices": []map[string]interface{}{
					{
						"delta": map[string]interface{}{
							"reasoning_content": msg + "\n",
						},
					},
				},
			}
			rawChunk, _ := json.Marshal(chunk)
			fmt.Fprintf(p.ResponseWriter, "data: %s\n\n", rawChunk)
			if canFlush {
				p.Flusher.Flush()
			}
		}
	}

	if !p.Stream {
		runNonStreamingLoop(llmCtx, p, params, progressCb)
	} else {
		runStreamingLoop(llmCtx, p, params, progressCb)
	}
}

func runNonStreamingLoop(ctx context.Context, p AgentParams, params openai.ChatCompletionNewParams, progressCb func(string)) {
	for i := 0; i < 5; i++ {
		resp, err := p.Client.Chat.Completions.New(ctx, params)
		if err != nil {
			log.Printf("[chat] Error: %v\n", err)
			http.Error(p.ResponseWriter, `{"error":"Chat completion failed"}`, http.StatusInternalServerError)
			return
		}

		if len(resp.Choices) == 0 {
			log.Printf("[chat] WARNING: LLM returned 0 choices — likely context overflow or token limit. model=%s estimatedPayloadBytes=%d", p.Model, p.EstimatedPayloadBytes)
			p.ResponseWriter.Header().Set("Content-Type", "application/json")
			json.NewEncoder(p.ResponseWriter).Encode(resp)
			return
		}
		log.Printf("[chat] ← LLM response: finish_reason=%s usage=%+v", resp.Choices[0].FinishReason, resp.Usage)

		msg := resp.Choices[0].Message

		if resp.Choices[0].FinishReason == "tool_calls" && len(msg.ToolCalls) > 0 {
			msgBytes, _ := json.Marshal(msg)
			var unionMsg openai.ChatCompletionMessageParamUnion
			json.Unmarshal(msgBytes, &unionMsg)
			params.Messages = append(params.Messages, unionMsg)

			handledAny := false
			for _, tc := range msg.ToolCalls {
				toolResult := ""
				
				var rawArgs interface{}
				json.Unmarshal([]byte(tc.Function.Arguments), &rawArgs)

				callReqJSON, _ := json.Marshal(map[string]interface{}{
					"method": "tools/call",
					"params": map[string]interface{}{
						"name":      tc.Function.Name,
						"arguments": rawArgs,
					},
				})
				var callReq *mcp.CallToolRequest
				json.Unmarshal(callReqJSON, &callReq)
				
				res, err := DispatchMCPTool(p.RequestCtx, callReq, progressCb)
				if err != nil {
					toolResult = fmt.Sprintf("Error calling tool: %v", err)
				} else if res != nil && len(res.Content) > 0 {
					if txt, ok := res.Content[0].(*mcp.TextContent); ok {
						toolResult = txt.Text
					}
				}

				toolMsgBytes, _ := json.Marshal(map[string]interface{}{
					"role":         "tool",
					"tool_call_id": tc.ID,
					"content":      toolResult,
				})
				var toolUnion openai.ChatCompletionMessageParamUnion
				json.Unmarshal(toolMsgBytes, &toolUnion)
				params.Messages = append(params.Messages, toolUnion)
				handledAny = true
			}

			if !handledAny {
				p.ResponseWriter.Header().Set("Content-Type", "application/json")
				json.NewEncoder(p.ResponseWriter).Encode(resp)
				return
			}
			continue
		}

		p.ResponseWriter.Header().Set("Content-Type", "application/json")
		json.NewEncoder(p.ResponseWriter).Encode(resp)
		return
	}

	http.Error(p.ResponseWriter, `{"error":"Max tool call depth exceeded"}`, http.StatusInternalServerError)
}

func runStreamingLoop(ctx context.Context, p AgentParams, params openai.ChatCompletionNewParams, progressCb func(string)) {
	for i := 0; i < 5; i++ {
		streamResp := p.Client.Chat.Completions.NewStreaming(ctx, params)

		var tcID string
		var tcName string
		var tcAccumulator string
		var finishReason string
		var receivedAnyContent bool

		for streamResp.Next() {
			chunk := streamResp.Current()

			if len(chunk.Choices) > 0 {
				delta := chunk.Choices[0].Delta

				if chunk.Choices[0].FinishReason != "" {
					finishReason = string(chunk.Choices[0].FinishReason)
				}

				if len(delta.ToolCalls) > 0 {
					tcDelta := delta.ToolCalls[0]
					if tcDelta.ID != "" {
						tcID = tcDelta.ID
					}
					if tcDelta.Function.Name != "" {
						tcName = tcDelta.Function.Name
					}
					if tcDelta.Function.Arguments != "" {
						tcAccumulator += tcDelta.Function.Arguments
					}
				} else {
					if tcName == "" {
						raw, _ := json.Marshal(chunk)
						fmt.Fprintf(p.ResponseWriter, "data: %s\n\n", raw)
						if p.Flusher != nil {
							p.Flusher.Flush()
						}
						receivedAnyContent = true
					}
				}
			}
		}

		if err := streamResp.Err(); err != nil {
			log.Printf("[chat] Stream error: %v\n", err)
			errChunk, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": fmt.Sprintf("LLM stream error: %v", err),
					"type":    "stream_error",
				},
			})
			fmt.Fprintf(p.ResponseWriter, "data: %s\n\n", errChunk)
			if p.Flusher != nil {
				p.Flusher.Flush()
			}
			break
		}

		if !receivedAnyContent && finishReason == "" && tcName == "" {
			approxTokens := p.EstimatedPayloadBytes / 4
			log.Printf("[chat] WARNING: streaming LLM returned no content... approxTokens≈%d", approxTokens)

			go func() {
				probeURL := os.Getenv("MAIN_GPU_URL") + "/v1/models"
				probeReq, _ := http.NewRequest("GET", probeURL, nil)
				probeReq.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
				probeClient := &http.Client{Timeout: 5 * time.Second}
				if probeResp, err := probeClient.Do(probeReq); err == nil {
					defer probeResp.Body.Close()
					body, _ := io.ReadAll(probeResp.Body)
					log.Printf("[chat] llama_cpp /v1/models → HTTP %d: %s", probeResp.StatusCode, string(body))
				}
			}()

			errChunk, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": fmt.Sprintf("The model returned no response. Input is ~%d tokens — it may exceed the context window. Check server logs for details.", approxTokens),
					"type":    "context_length_exceeded",
				},
			})
			fmt.Fprintf(p.ResponseWriter, "data: %s\n\n", errChunk)
			if p.Flusher != nil {
				p.Flusher.Flush()
			}
			break
		}

		if finishReason == "tool_calls" && tcName != "" {
			astMsgBytes, _ := json.Marshal(map[string]interface{}{
				"role": "assistant",
				"tool_calls": []map[string]interface{}{{
					"id":   tcID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      tcName,
						"arguments": tcAccumulator,
					},
				}},
			})
			var astMsg openai.ChatCompletionMessageParamUnion
			json.Unmarshal(astMsgBytes, &astMsg)
			params.Messages = append(params.Messages, astMsg)

			toolResult := ""
			
			var rawArgs interface{}
			json.Unmarshal([]byte(tcAccumulator), &rawArgs)

			callReqJSON, _ := json.Marshal(map[string]interface{}{
				"method": "tools/call",
				"params": map[string]interface{}{
					"name":      tcName,
					"arguments": rawArgs,
				},
			})
			var callReq *mcp.CallToolRequest
			json.Unmarshal(callReqJSON, &callReq)

			res, err := DispatchMCPTool(p.RequestCtx, callReq, progressCb)
			if err != nil {
				toolResult = err.Error()
			} else if res != nil && len(res.Content) > 0 {
				if txt, ok := res.Content[0].(*mcp.TextContent); ok {
					toolResult = txt.Text
				}
			}

			if toolResult != "" {
				toolMsgBytes, _ := json.Marshal(map[string]interface{}{
					"role":         "tool",
					"tool_call_id": tcID,
					"content":      toolResult,
				})
				var toolUnion openai.ChatCompletionMessageParamUnion
				json.Unmarshal(toolMsgBytes, &toolUnion)
				params.Messages = append(params.Messages, toolUnion)
				continue
			} else {
				chunk := map[string]interface{}{
					"choices": []map[string]interface{}{
						{
							"delta": map[string]interface{}{
								"content": nil,
								"tool_calls": []map[string]interface{}{
									{
										"id":   tcID,
										"type": "function",
										"function": map[string]interface{}{
											"name":      tcName,
											"arguments": tcAccumulator,
										},
									},
								},
							},
							"finish_reason": "tool_calls",
						},
					},
				}
				raw, _ := json.Marshal(chunk)
				fmt.Fprintf(p.ResponseWriter, "data: %s\n\n", raw)
				if p.Flusher != nil {
					p.Flusher.Flush()
				}
				break
			}
		}

		fmt.Fprintf(p.ResponseWriter, "data: [DONE]\n\n")
		if p.Flusher != nil {
			p.Flusher.Flush()
		}
		break
	}
}
