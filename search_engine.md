implement the LLM search feature. Create a new file search_feature.go 
Place the entire feature into the file.
# 🌐 LLM Web Search & Context Retrieval Engine

A robust, lightweight web search integration designed to fetch, parse, and feed the latest internet information and documentation directly into our LLM chat.

## 📌 Overview
This feature bypasses the knowledge cutoff of our LLM by querying live data. We utilize **DuckDuckGo Lite** (`https://lite.duckduckgo.com/lite/`) as our primary search engine due to its simple HTML structure, lack of JavaScript bloat, and absence of restrictive API rate limits. 

To ensure long-term stability against DOM/CSS changes by the search engine, we use an **LLM-driven parsing strategy** rather than fragile regex or XPath selectors.

## ⚙️ Execution Pipeline

### Step 1: Query Execution
- Intercept user prompts requiring real-time knowledge.
- Formulate an optimized search query and execute an HTTP request to DuckDuckGo Lite.
- Retrieve the raw, lightweight HTML response.

### Step 2: Resilient Search Parsing (LLM-Based)
- Pass the raw (or stripped) search engine HTML into a lightweight, fast LLM (e.g., Llama-3-8B, Claude Haiku, or GPT-4o-mini).
- Prompt the LLM to extract the top `N` results, returning structured JSON containing: `{ "title": "...", "snippet": "...", "url": "..." }`.
- *Benefit:* If DuckDuckGo changes its class names or layout, the LLM will still correctly identify and parse the results.

LLM usage
```sh
curl https://alice.forest-interactive.com/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-1234567890" \
  -d '{
    "model": "Qwen3.5-35B-A3B-GGUF",
    "messages": [
      { "role": "system", "content": "You are a helpful assistant." },
      { "role": "user", "content": "Hello!" }
    ],
    "temperature": 0.7
  }'
```

### Step 3: Deep Web Crawling & Main Text Extraction (Golang)
*If the search snippet lacks sufficient context (e.g., reading API docs or full articles), the pipeline triggers a deep crawl of the extracted `url`s using a robust Go-based scraping architecture.*

- **JS-Rendered Execution (Chromedp):**
  Many modern websites and documentation portals (e.g., React/Next.js apps) require JavaScript execution to display content. We use **[chromedp](https://github.com/chromedp/chromedp)** to drive a headless Chrome instance. This is the most efficient, native Go implementation for the Chrome DevTools Protocol, optimized for high-performance scraping on Ubuntu/Linux servers.  In Go, you must ensure you call cancel() on the chromedp context, otherwise, you will leave "zombie" Chrome processes 

  # requirements for JS-Rendered Execution Chromium on Ubuntu
sudo apt update && sudo apt install -y chromium-browser

- **Main Text Extraction (Noise Reduction):**
  Feeding entire raw HTML pages to an LLM will immediately exhaust token limits. To strip away ads, footers, and navbars, we pass the rendered HTML through **[go-shiori/go-readability](https://github.com/go-shiori/go-readability)**. This Go port of Mozilla's Readability algorithm accurately isolates the core article text.

### Step 4: Post-Processing, Citation, & Streaming
- Feed the aggregated context (search snippets + crawled webpage text) into the main conversational LLM.
- Synthesize the final response.
- **Cite the sources** inline using the parsed URLs.
- **Stream** the generated response back to the user interface for a low-latency experience.

