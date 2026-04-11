
Memory/RAG: search_my_documents to query a vector database of the user's uploaded files. IMPLEMENT FULLY
URL Scraper: fetch_page_content to read the full text of a specific link found during search. PLACEHOLDER ONLY DO NOT IMPLEMENT
Image Generator: generate_image via DALL-E or Stable Diffusion. ( custom endpoint )  PLACEHOLDER ONLY DO NOT IMPLEMENT

Wolfram Alpha / Calculator: For high-precision math and unit conversions (LLMs are notoriously bad at arithmetic). PLACEHOLDER ONLY DO NOT IMPLEMENT
code Interpreter (Sandboxed): For data analysis, plotting charts, or complex logic. PLACEHOLDER ONLY DO NOT IMPLEMENT




#### 1. Tool Architecture Refactoring (Confidence: 0.95)
**Logic:** A monolithic `openAiChat` with hardcoded `if tc.Function.Name == "web_search"` will become unmaintainable as you add 3-4 more tools. You should use a **Tool Registry Pattern**. 
**Verification:** Creating a central registry (a map of functions and schemas) allows the chat loop to dynamically execute *any* requested tool without modifying the chat logic again.

#### 2. Building `search_my_documents` (Confidence: 0.95)
**Logic:** You already have the underlying RAG logic in `retrieveContext` and `ragGenerate`. A tool is just an LLM-callable wrapper around that logic.
**Verification:** The tool schema needs parameters for `query` and optionally `document_names` to filter. The execution logic will call your existing `getQueryEmbedding`, `hybridSearch`, `performRRF`, `rerankChunks`, and `assembleContext`.

#### 3. Integration (Confidence: 0.90)
**Logic:** Replace the `if tcName == "web_search"` blocks in `openAiChat` with a generic `result, citations, err := executeTool(tcName, arguments)` call.
**Verification:** You currently append `finalWebResults` to the end of the message. We can generalize this so any tool can return citations to be appended.

---

### SYNTHESIZE: Step-by-Step Implementation

Here is how you should organize your code. Create a separate file (e.g., `tools.go`) to manage tools.

#### Step 1: Create a Tool Registry (`tools.go`)
Define a standard interface for all tools.

```go
package main

import (
	"encoding/json"
	"fmt"
	"github.com/openai/openai-go"
)

// Generic citation type for any tool to return
type ToolCitation struct {
	Title string
	URL   string // Can be an external URL or a local document name
}

// Tool Definition
type Tool struct {
	Definition openai.ChatCompletionToolParam
	Execute    func(args string, progressCb func(string)) (string,[]ToolCitation, error)
}

var ToolRegistry = make(map[string]Tool)

// Initialize tools (Call this from main())
func InitTools() {
	ToolRegistry["web_search"] = getWebSearchTool()
	ToolRegistry["search_my_documents"] = getSearchDocsTool()
	// ToolRegistry["another_tool"] = getAnotherTool()
}
```

#### Step 2: Implement `search_my_documents` 
In `tools.go` (or `tool_rag.go`), wrap your existing retrieval logic into the Tool format:

```go
func getSearchDocsTool() Tool {
	// 1. Define the OpenAI Schema
	schemaJSON := `{
		"type": "function",
		"function": {
			"name": "search_my_documents",
			"description": "Search the user's uploaded documents and files for specific context, knowledge, or facts.",
			"parameters": {
				"type": "object",
				"properties": {
					"query": {
						"type": "string",
						"description": "The search query to find relevant text."
					},
					"document_names": {
						"type": "array",
						"items": {"type": "string"},
						"description": "Optional list of specific document names to filter the search by."
					}
				},
				"required": ["query"]
			}
		}
	}`
	var def openai.ChatCompletionToolParam
	json.Unmarshal([]byte(schemaJSON), &def)

	// 2. Define the Execution Logic (Reusing your existing RAG functions)
	execute := func(argsJSON string, progressCb func(string)) (string,[]ToolCitation, error) {
		var args struct {
			Query         string   `json:"query"`
			DocumentNames[]string `json:"document_names"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", nil, err
		}

		if progressCb != nil {
			progressCb("[rag] Searching local documents for: " + args.Query)
		}

		// Use your existing RAG pipeline functions
		queryEmbedding, err := getQueryEmbedding(args.Query)
		if err != nil {
			return "", nil, err
		}

		dense, sparse, err := hybridSearch(args.Query, queryEmbedding, args.DocumentNames, 20)
		if err != nil {
			return "", nil, err
		}

		fusedChunks := performRRF(dense, sparse, 60, 15)
		retrievedChunks := rerankChunks(args.Query, fusedChunks, 5) // top_n = 5

		if len(retrievedChunks) == 0 {
			return "No relevant information found in user documents.", nil, nil
		}

		// Assemble the text content to feed back to the LLM
		contextText := assembleContext(retrievedChunks)

		// Create citations based on the retrieved chunks
		var citations[]ToolCitation
		docSet := make(map[string]bool)
		for _, chunk := range retrievedChunks {
			// Assuming your chunk struct has a DocumentName or similar field
			if !docSet[chunk.DocumentName] {
				docSet[chunk.DocumentName] = true
				citations = append(citations, ToolCitation{
					Title: chunk.DocumentName,
					URL:   "doc://" + chunk.DocumentName, // Custom scheme for frontend to parse
				})
			}
		}

		return contextText, citations, nil
	}

	return Tool{
		Definition: def,
		Execute:    execute,
	}
}
```
*(Note: You will also adapt your existing `web_search` into this identical `Tool` format).*

#### Step 3: Refactor `openAiChat` to Use the Registry
Now, your `openAiChat` becomes completely agnostic to *which* tools exist.

**1. Injecting schemas dynamically:**
Instead of hardcoding the web search JSON injection:
```go
	// Inject tools requested by UI
	if webSearch {
		if wsTool, ok := ToolRegistry["web_search"]; ok {
			params.Tools = append(params.Tools, wsTool.Definition)
		}
	}
	// Example: Add search_my_documents if requested
	if r.FormValue("use_rag") == "true" { 
		if ragTool, ok := ToolRegistry["search_my_documents"]; ok {
			params.Tools = append(params.Tools, ragTool.Definition)
		}
	}
```

**2. Executing dynamically (inside the loops):**
Replace your `if tcName == "web_search" { ... }` block with:

```go
	// In the stream/non-stream loop where tool execution happens:
	if tool, exists := ToolRegistry[tcName]; exists {
		contextText, citations, err := tool.Execute(tcAccumulator, progressCb)
		if err != nil {
			log.Printf("Tool %s failed: %v", tcName, err)
			contextText = "Tool execution failed."
		}

		// Collect generalized citations (merging web results and doc results)
		finalCitations = append(finalCitations, citations...)

		// Send tool result back to LLM
		toolMsgBytes, _ := json.Marshal(map[string]interface{}{
			"role": "tool",
			"tool_call_id": tcID,
			"content": contextText,
		})
		var toolUnion openai.ChatCompletionMessageParamUnion
		json.Unmarshal(toolMsgBytes, &toolUnion)
		params.Messages = append(params.Messages, toolUnion)
		
		continue // Loop again to let LLM generate final answer
	} else {
		// Tool not found in registry, break and send tool_call to client
		// (Keep your existing fallback logic here)
	}
```

---

### Final Output Summary

**Clear Answer:** 
Yes, you should extract the tool schemas and execution logic out of `openAiChat`. Use a **Tool Registry Pattern** (a Map binding tool names to their schemas and execution functions). To build `search_my_documents`, wrap your existing `hybridSearch`, `performRRF`, and `rerankChunks` pipeline into a generic tool function signature and register it. 

**Confidence Level:** 
0.95/1.0 - The Tool Registry pattern is the standard, scalable way to manage LLM tools in Go backends. It directly resolves the issue of bloated controller functions.

**Key Caveats:**
1. **Token Limits:** Adding `search_my_documents` means the LLM might decide to call it *and* `web_search` in the same turn, or consecutively. Ensure your RAG `assembleContext` truncates text strictly, or you will easily blow past maximum context windows.
2. **Citation Clashing:** Because `web_search` returns URLs and `search_my_documents` returns local files, standardize your citation format (e.g., using prefix `doc://` for internal files vs `https://` for web) so the frontend UI can format the links correctly at the end of the chat stream.