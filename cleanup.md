# Implementation Guide: Refactoring `openAiChat` (Server-Side Orchestration)

## 🎯 Goal
Refactor the `openAiChat` HTTP handler to adhere strictly to the Single Responsibility Principle (SRP).
The current handler and the orchestration logic are split between `text_generation.go` and `agent_orchestrator.go`, but the tool execution and orchestration should ideally be fully consolidated into `mcp.go`.

The main goal is to strip `openAiChat` down to purely handling HTTP transport (parsing requests and SSE headers) and delegate all LLM network calls, ad-hoc tool merging, and recursive tool execution loops entirely to `mcp.go`.

## 🧠 Reasoning
1. **Consolidating Tool Orchestration:** `mcp.go` already holds the MCP Server, tool definitions, and dispatch logic (`DispatchMCPTool`). Moving the agentic loop (`RunAgentLoop`) and `AgentParams` from `agent_orchestrator.go` into `mcp.go` centralizes all tool-related logic into one domain.
2. **Separation of Concerns:** `text_generation.go` should only manage the HTTP request/response lifecycle (multipart forms, headers, file attach processing). It shouldn't care about merging ad-hoc tool arrays or managing context limits.
3. **Frontend Contract:** The UI (`chat.js`) expects a highly specific SSE payload (e.g. `reasoning_content`, `[search]` parsing, dynamic duration calculation, and custom `{"type": "citations"}`). Consolidating the orchestrator in `mcp.go` ensures we keep the streaming cleanly isolated from standard HTTP logic.

---

## 🛑 Frontend Constraints & Contract (CRITICAL)
1. **Streaming Format:** The frontend reads `data: { "choices": [ { "delta": { ... } } ] }`.
2. **Reasoning/Progress Injection:** Tool execution progress (e.g., `[search] executing optimal...`) MUST be streamed as `delta.reasoning_content`. The frontend parses this to show the "Thinking/Searching" accordion UI.
3. **Done Signal:** The stream must end with `data: [DONE]\n\n`.
4. **Citations:** Web search and RAG sources must be appended to the final `content` output as a markdown source block, or sent via a custom `{"type": "citations", ...}` JSON chunk.
5. **File Attachments:** The frontend sends files via `multipart/form-data` under the `files` key. These must still be processed into `image_url` or `text` blocks in the OpenAI format before the agentic loop starts.

---

## 🏗️ Step-by-Step Implementation Plan

### Step 1: Consolidate Orchestration into `mcp.go`
* **Task:** Move `RunAgentLoop`, `runNonStreamingLoop`, `runStreamingLoop`, and `AgentParams` from `agent_orchestrator.go` entirely into `mcp.go`.
* **Task:** Delete `agent_orchestrator.go` after migrating the code to eliminate file fragmentation.

### Step 2: Abstract Tool Parameter Merging
* **Task:** In `mcp.go`, create a helper `MergeTools(adHocTools json.RawMessage, webSearch bool) []openai.ChatCompletionToolParam`.
* **Logic:** Move the parsing of `tools` from the JSON/Multipart body out of `text_generation.go`. Let `mcp.go` handle combining explicitly requested tools with the dynamic MCP system tools.

### Step 3: Refactor `openAiChat` HTTP Handler
* **Task:** Strip the `openAiChat` handler in `text_generation.go` down to:
  1. Header normalization and CORS.
  2. Parsing multipart or JSON payloads.
  3. Running `processIncomingFile` logic for attachments.
  4. Setting up the SSE flusher.
  5. Building `AgentParams` and calling `RunAgentLoop` (now residing in `mcp.go`).
* **Result:** `openAiChat` becomes drastically smaller and completely agnostic to how the LLM functions.

### Step 4: Ensure UI Citation / Reasoning Parity
* **Task:** When moving the loop, ensure `progressCb` logic explicitly conforms to formatting `delta.reasoning_content` appropriately so the UI's `updateStreamingDOM` function continues to render "Searching..." or "Thinking..." correctly.
* **Task:** Handle the `citations` custom SSE chunk correctly inside the new `mcp.go` orchestrator if applicable.

---

## ✅ Definition of Done
- [ ] `agent_orchestrator.go` is deleted and its core functions (`RunAgentLoop`, etc.) are moved into `mcp.go`.
- [ ] `openAiChat` manages strictly HTTP data processing; no tool merging logic remains.
- [ ] Ad-hoc tool payload merging is encapsulated inside an `mcp.go` helper function.
- [ ] SSE streaming perfectly matches `chat.js` expectations (`reasoning_content` strings, file attachment preservation, and custom chunks).