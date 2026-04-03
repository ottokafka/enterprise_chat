package main

// search_handler.go — HTTP handler for POST /v1/search
//
// Wraps the Search() pipeline in search_engine.go and re-encodes its
// streamed plain-text output as OpenAI-compatible SSE chunks so the
// existing chat.js streaming loop handles the response transparently.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
)

// ─────────────────────────────────────────────────────────────────────────────
// sseChunkWriter
//
// Wraps an http.ResponseWriter. Every Write() call is treated as a text token
// and re-emitted as an OpenAI-style SSE data chunk:
//
//	data: {"choices":[{"delta":{"content":"<text>"},"finish_reason":null}]}
//
// A final [DONE] frame is sent by calling Close().
// It is safe to call from a single goroutine only.
// ─────────────────────────────────────────────────────────────────────────────

type sseChunkWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
}

func newSSEChunkWriter(w http.ResponseWriter) *sseChunkWriter {
	f, _ := w.(http.Flusher)
	return &sseChunkWriter{w: w, flusher: f}
}

// Write implements io.Writer. Each call emits one SSE data chunk.
func (s *sseChunkWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	type delta struct {
		Content string `json:"content"`
	}
	type choice struct {
		Delta        delta   `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}
	type chunk struct {
		Choices []choice `json:"choices"`
	}

	payload, _ := json.Marshal(chunk{
		Choices: []choice{{Delta: delta{Content: string(p)}}},
	})

	_, err := fmt.Fprintf(s.w, "data: %s\n\n", payload)
	if err != nil {
		return 0, err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return len(p), nil
}

// WriteReasoning emits reasoning content (search logs) to the SSE stream.
func (s *sseChunkWriter) WriteReasoning(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	type delta struct {
		ReasoningContent string `json:"reasoning_content,omitempty"`
	}
	type choice struct {
		Delta delta `json:"delta"`
	}
	type chunk struct {
		Choices []choice `json:"choices"`
	}

	payload, _ := json.Marshal(chunk{
		Choices: []choice{{Delta: delta{ReasoningContent: string(p)}}},
	})

	_, err := fmt.Fprintf(s.w, "data: %s\n\n", payload)
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return err
}

// Close sends the terminal [DONE] frame.
func (s *sseChunkWriter) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// webSearch — POST /v1/search
//
// Request body (JSON):
//
//	{ "query": "...", "deep_crawl": false }
//
// Response: SSE stream of OpenAI-compatible data chunks (text/event-stream)
// ─────────────────────────────────────────────────────────────────────────────

func webSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string `json:"query"`
		DeepCrawl bool   `json:"deep_crawl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.Query == "" {
		http.Error(w, `{"error":"query is required"}`, http.StatusBadRequest)
		return
	}

	// ── SSE headers ───────────────────────────────────────────────────────────
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	} else {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}

	cw := newSSEChunkWriter(w)

	progressCb := func(msg string) {
		_ = cw.WriteReasoning([]byte(msg + "\n"))
	}

	log.Printf("[search] handling query=%q deepCrawl=%v", req.Query, req.DeepCrawl)
	if err := Search(req.Query, req.DeepCrawl, cw, progressCb); err != nil {
		log.Printf("[search] pipeline error: %v", err)
		// Best-effort: emit error as a content chunk so UI shows it
		errMsg := fmt.Sprintf("\n\n⚠️  Search error: %v", err)
		cw.Write([]byte(errMsg)) //nolint:errcheck
	}

	cw.Close()
}
