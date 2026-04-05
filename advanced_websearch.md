# Advanced Web Search Implementation

This document outlines the technical implementation of the **Query Expansion (Multi-Query) RAG pattern** for the Enterprise Chat application. This architecture empowers the chat application with advanced search capabilities similar to Perplexity or ChatGPT's Web Search.

## Background

A naive direct search approach using only the user's latest message (e.g., *"What about his second book?"*) fails because search engines lack chat history context.

To solve this, a pipeline is embedded inside the chat route:
1. **Query Generation**: Analyze chat history with an LLM and generate 1-3 standalone, context-rich search queries.
2. **Parallel Search Execution**: Fire off concurrent DuckDuckGo searches and crawls.
3. **Context Injection**: Merge and deduplicate the search results into the chat history.
4. **Final Generation**: Stream the answer back to the user via the final LLM call.

---

## Technical Details: Files to Modify

Implementing this pipeline requires modifying both the Go backend (`search_engine.go`, `text_generation.go`) and the frontend (`public/js/chat.js`) logic.


### 1. `search_engine.go` (Backend Search Logic)

We must introduce the query generation and concurrent execution functions to the existing search engine logic.

**Additions:**
- **`generateSearchQueries`**: Initiates a fast, non-streaming LLM call with a low temperature (e.g., 0.1) that forces strict JSON array output based on the user's conversation history.
- **`executeParallelSearches`**: Utilizes `sync.WaitGroup` and `sync.Mutex` to concurrently execute multiple `fetchDDGHTML` calls, parse the results via `parseSearchResultsWithLLM`, and perform `crawlURL` deep crawls while actively deduping duplicate URLs across concurrent queries.

*Dependencies Required*:
```go
import "sync"
```


### 2. `text_generation.go` (Backend Chat Route)

We must intercept the standard chat completion pipeline to silently inject the newly scraped web context.

**Modifications to `llamaChat`:**
1. **Trigger Condition**: Detect if Web Search is requested (e.g., the frontend passes `model="web-search-agent"` or a custom `web_search=true` parameter in the payload).
2. **Pipeline Interception**: 
   - Execute `generateSearchQueries` using the unmarshalled `messages` array.
   - Run `executeParallelSearches` with the generated queries string array.
3. **Context Injection**:
   - Prepend/Append the aggregated context text into the *last user message* in the `openaiMessages` slice.
4. **Execution**: The normal `llmClient.Chat.Completions.NewStreaming` routine continues executing normally, but the user's prompt is now loaded invisibly with recent, deduped web search context.

*Key considerations during manipulation:*
The `llamaChat` method parses both `multipart/form-data` and generic JSON bodies. The interception logic must be placed right before calling `llmClient.Chat.Completions.New(...)` (or `NewStreaming(...)`), ensuring attachments and previous message states are fully parsed.


### 3. `public/js/chat.js` (Frontend Routing)

Currently (around lines 342-347), the frontend forcefully branches off to a separate `/v1/search` endpoint when the Web Search toggle is active: Remvoe /v1/search from the chat logic

```javascript
const endpoint = webSearchEnabled
  ? '/v1/search'
  : activeDocumentNames.size > 0
    ? '/v1/rag'
    : '/v1/chat/completions';
const response = await fetch(endpoint, requestConfig);
```

**Modifications to `ChatApp` logic:**
To leverage the Query Expansion RAG pipeline integrated into `llamaChat`, we either:
- Continue pointing `/v1/search` to an isolated handler that executes this new pipeline and returns a stream.
- **(Recommended)** Unify the frontend to point towards `/v1/chat/completions` and denote intent using a custom flag in `formData.append('web_search', 'true')` 

The frontend should also ideally display real-time streamed partial updates (e.g., "Searching for Query 1...", "Crawled 3 Pages") using the SSE Reasoning block (`reasoningBuffer`) before the actual text generation begins.

---

## Architectural Edge Cases to Manage

1. **Rate Limiting / WAF**: Firing 3 simultaneous queries to DuckDuckGo domains from the same IP will vastly increase 429 errors. To avoid getting shadow-banned by `lite.duckduckgo.com`, it may be necessary to implement randomized staggered delays (e.g., ~500ms jitter) within the `sync.WaitGroup` goroutines.
2. **UX Delay**: The parallel crawling stage inherently blocks the final LLM text streaming response pipeline. For 3–8 seconds, the UI will wait for context aggregation. The frontend must reliably utilize `streaming-reasoning-body` to communicate progress instead of looking frozen.
