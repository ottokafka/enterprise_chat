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
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

	// ── Create the Streamable HTTP handler (MCP 2025-03-26 compliant) ─────────
	// NewStreamableHTTPHandler wraps the server so every incoming HTTP request
	// is routed to the appropriate MCP session or creates a new one.
	MCPHandler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return MCPServer
	}, nil)
}
