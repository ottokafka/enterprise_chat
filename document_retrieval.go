package main

// document_retrieval.go — Port of controllers/document_retrieval.js
//
// Implements:
//   - Hybrid Search (dense cosine + sparse keyword) against ClickHouse rag_embeddings
//   - Reciprocal Rank Fusion (RRF)
//   - Cross-Encoder Reranking via local HTTP reranker service
//   - Context assembly & citation post-processing
//   - POST /v1/retrieve  — returns raw chunks
//   - POST /v1/rag       — full pipeline with optional SSE streaming

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
)

// ─────────────────────────────────────────────────────────────────────────────
// Embedding — uses same local vLLM endpoint as document_ingestion.go
// ─────────────────────────────────────────────────────────────────────────────

func getQueryEmbedding(text string) ([]float64, error) {
	client := openai.NewClient(
		option.WithBaseURL(os.Getenv("EMBEDDING_URL")),
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	)
	res, err := client.Embeddings.New(context.Background(), openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel("Qwen3-Embedding-8B"),
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: []string{text}},
	})
	if err != nil {
		return nil, err
	}
	if len(res.Data) == 0 {
		return nil, fmt.Errorf("empty embedding response")
	}
	out := make([]float64, len(res.Data[0].Embedding))
	copy(out, res.Data[0].Embedding)
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// RAGChunk matches the shape of rows from ClickHouse rag_embeddings
// ─────────────────────────────────────────────────────────────────────────────

type RAGChunk struct {
	ID             string   `json:"id"              ch:"id"`
	DocumentName   string   `json:"document_name"   ch:"document_name"`
	ChunkType      string   `json:"chunk_type"      ch:"chunk_type"`
	ChunkIndex     *uint32  `json:"chunk_index"     ch:"chunk_index"`
	Content        string   `json:"content"         ch:"content"`
	Distance       float64  `json:"distance,omitempty"`
	RRFScore       float64  `json:"rrf_score,omitempty"`
	RelevanceScore *float64 `json:"relevance_score"`
}

// ─────────────────────────────────────────────────────────────────────────────
// PHASE 1 — Hybrid Search
// Dense:  cosineDistance on embedding column
// Sparse: hasTokenCaseInsensitive on content
// Both run via goroutines for maximum concurrency (mirrors Promise.all)
// ─────────────────────────────────────────────────────────────────────────────

func hybridSearch(userQuery string, queryEmbedding []float64, documentNames []string, limit int) ([]RAGChunk, []RAGChunk, error) {
	ch := getClickhouseClient()
	if ch == nil {
		return nil, nil, fmt.Errorf("clickhouse not initialized")
	}

	// Build embedding literal: [0.1,0.2,...]
	embParts := make([]string, len(queryEmbedding))
	for i, v := range queryEmbedding {
		embParts[i] = strconv.FormatFloat(v, 'f', -1, 64)
	}
	embLiteral := "[" + strings.Join(embParts, ",") + "]"

	// Build doc filter clause
	var docFilter string
	if len(documentNames) > 0 {
		quoted := make([]string, len(documentNames))
		for i, d := range documentNames {
			quoted[i] = "'" + strings.ReplaceAll(d, "'", "''") + "'"
		}
		docFilter = "document_name IN (" + strings.Join(quoted, ",") + ")"
	}

	// Keywords for sparse search — strip non-alphanumeric, keep words > 3 chars
	re := regexp.MustCompile(`[^a-zA-Z0-9 ]`)
	cleanQuery := re.ReplaceAllString(userQuery, "")
	var keywords []string
	for _, w := range strings.Fields(cleanQuery) {
		if len(w) > 3 {
			keywords = append(keywords, w)
		}
	}

	type result struct {
		chunks []RAGChunk
		err    error
	}
	denseCh := make(chan result, 1)
	sparseCh := make(chan result, 1)

	// Dense query goroutine
	go func() {
		whereClause := ""
		if docFilter != "" {
			whereClause = "WHERE " + docFilter
		}
		q := fmt.Sprintf(`
			SELECT id, document_name, chunk_type, chunk_index, content,
			       cosineDistance(embedding, %s) AS distance
			FROM rag_embeddings
			%s
			ORDER BY distance ASC
			LIMIT %d
		`, embLiteral, whereClause, limit)

		rows, err := ch.Query(context.Background(), q)
		if err != nil {
			denseCh <- result{err: err}
			return
		}
		defer rows.Close()
		var chunks []RAGChunk
		for rows.Next() {
			var c RAGChunk
			if err := rows.Scan(&c.ID, &c.DocumentName, &c.ChunkType, &c.ChunkIndex, &c.Content, &c.Distance); err != nil {
				continue
			}
			chunks = append(chunks, c)
		}
		denseCh <- result{chunks: chunks}
	}()

	// Sparse query goroutine
	go func() {
		var conditions []string
		if docFilter != "" {
			conditions = append(conditions, docFilter)
		}
		if len(keywords) > 0 {
			kwConds := make([]string, len(keywords))
			for i, kw := range keywords {
				kwConds[i] = fmt.Sprintf("hasTokenCaseInsensitive(content, '%s')", strings.ReplaceAll(kw, "'", "''"))
			}
			conditions = append(conditions, "("+strings.Join(kwConds, " OR ")+")")
		}

		whereClause := ""
		if len(conditions) > 0 {
			whereClause = "WHERE " + strings.Join(conditions, " AND ")
		}
		q := fmt.Sprintf(`
			SELECT id, document_name, chunk_type, chunk_index, content
			FROM rag_embeddings
			%s
			LIMIT %d
		`, whereClause, limit)

		rows, err := ch.Query(context.Background(), q)
		if err != nil {
			sparseCh <- result{err: err}
			return
		}
		defer rows.Close()
		var chunks []RAGChunk
		for rows.Next() {
			var c RAGChunk
			if err := rows.Scan(&c.ID, &c.DocumentName, &c.ChunkType, &c.ChunkIndex, &c.Content); err != nil {
				continue
			}
			chunks = append(chunks, c)
		}
		sparseCh <- result{chunks: chunks}
	}()

	dr := <-denseCh
	sr := <-sparseCh

	if dr.err != nil {
		return nil, nil, fmt.Errorf("dense search error: %w", dr.err)
	}
	if sr.err != nil {
		return nil, nil, fmt.Errorf("sparse search error: %w", sr.err)
	}

	log.Printf("[retrieve] Dense: %d hits, Sparse: %d hits\n", len(dr.chunks), len(sr.chunks))
	return dr.chunks, sr.chunks, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// PHASE 2 — Reciprocal Rank Fusion (RRF)
// score = Σ 1/(k + rank), k=60
// ─────────────────────────────────────────────────────────────────────────────

func performRRF(denseResults, sparseResults []RAGChunk, k, topN int) []RAGChunk {
	scores := make(map[string]float64)
	data := make(map[string]RAGChunk)

	accumulate := func(chunks []RAGChunk) {
		for i, row := range chunks {
			scores[row.ID] += 1.0 / float64(k+i+1)
			if _, exists := data[row.ID]; !exists {
				data[row.ID] = row
			}
		}
	}

	accumulate(denseResults)
	accumulate(sparseResults)

	type scored struct {
		id    string
		score float64
	}
	var sorted []scored
	for id, s := range scores {
		sorted = append(sorted, scored{id, s})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].score > sorted[j].score })

	if topN > len(sorted) {
		topN = len(sorted)
	}
	result := make([]RAGChunk, topN)
	for i := 0; i < topN; i++ {
		chunk := data[sorted[i].id]
		chunk.RRFScore = sorted[i].score
		result[i] = chunk
	}
	return result
}

// ─────────────────────────────────────────────────────────────────────────────
// PHASE 3 — Cross-Encoder Reranking via HTTP reranker
// Graceful fallback: if reranker is unavailable, returns top-N RRF results
// ─────────────────────────────────────────────────────────────────────────────

type rerankerRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
}

type rerankerResult struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
}

type rerankerResponse struct {
	Model   string           `json:"model"`
	Object  string           `json:"object"`
	Usage   map[string]int   `json:"usage"`
	Results []rerankerResult `json:"results"`
}

// ─────────────────────────────────────────────────────────────────────────────
// @route  POST /v1/rerank
// @desc   Proxy rerank request to upstream reranker service
// @access Private (apiAuth)
// ─────────────────────────────────────────────────────────────────────────────
func rerankHandler(w http.ResponseWriter, r *http.Request) {
	var body rerankerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"Invalid request body"}`, http.StatusBadRequest)
		return
	}

	if body.Query == "" || len(body.Documents) == 0 {
		http.Error(w, `{"error":"'query' and 'documents' are required"}`, http.StatusBadRequest)
		return
	}

	reqBody, _ := json.Marshal(body)

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Post(
		os.Getenv("RERANKER_URL"),
		"application/json",
		bytes.NewReader(reqBody),
	)
	if err != nil {
		log.Printf("[reranker] Handler error: %v\n", err)
		http.Error(w, `{"error":"Upstream reranker unavailable"}`, http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	// Forward the upstream response status
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)

	// Since we might want to ensure the response matches the exactly expected format
	// and potentially provide defaults if the upstream is slightly different,
	// we'll decode and re-encode. But for efficiency, if we trust the upstream,
	// we could just io.Copy. Given the user's request, let's decode to be safe
	// and ensure fields like "object": "list" are present if missing.
	var reranked rerankerResponse
	if err := json.NewDecoder(resp.Body).Decode(&reranked); err != nil {
		log.Printf("[reranker] Decode error: %v\n", err)
		http.Error(w, `{"error":"Failed to decode upstream reranker response"}`, http.StatusInternalServerError)
		return
	}

	// Ensure essential fields for the user's expected format
	if reranked.Object == "" {
		reranked.Object = "list"
	}
	if reranked.Model == "" {
		reranked.Model = body.Model
	}

	json.NewEncoder(w).Encode(reranked)
}

func rerankChunks(userQuery string, fusedChunks []RAGChunk, topN int) []RAGChunk {
	if len(fusedChunks) == 0 {
		return []RAGChunk{}
	}

	docs := make([]string, len(fusedChunks))
	for i, c := range fusedChunks {
		docs[i] = c.Content
	}

	reqBody, _ := json.Marshal(rerankerRequest{
		Model:     "Qwen3-Reranker-8B",
		Query:     userQuery,
		Documents: docs,
		TopN:      topN,
	})

	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Post(
		os.Getenv("RERANKER_URL"),
		"application/json",
		bytes.NewReader(reqBody),
	)
	if err != nil {
		log.Printf("[reranker] Unavailable (%v). Falling back to RRF top-%d.\n", err, topN)
		end := topN
		if end > len(fusedChunks) {
			end = len(fusedChunks)
		}
		result := make([]RAGChunk, end)
		for i := range result {
			chunk := fusedChunks[i]
			chunk.RelevanceScore = nil
			result[i] = chunk
		}
		return result
	}
	defer resp.Body.Close()

	var reranked rerankerResponse
	if err := json.NewDecoder(resp.Body).Decode(&reranked); err != nil {
		log.Printf("[reranker] Decode error: %v. Falling back.\n", err)
		end := topN
		if end > len(fusedChunks) {
			end = len(fusedChunks)
		}
		return fusedChunks[:end]
	}

	results := make([]RAGChunk, len(reranked.Results))
	for i, r := range reranked.Results {
		chunk := fusedChunks[r.Index]
		score := r.RelevanceScore
		chunk.RelevanceScore = &score
		results[i] = chunk
	}
	return results
}

// ─────────────────────────────────────────────────────────────────────────────
// PHASE 4 — Context assembly & citation post-processing
// ─────────────────────────────────────────────────────────────────────────────

const ragSystemPrompt = `You are an expert analytical assistant. You will be provided with a user question and several Context Documents.
Answer the user's question USING ONLY the information provided in the Context Documents.

RULES:
1. If the answer is not contained in the context, explicitly say: "I do not have enough information to answer this based on the provided documents."
2. When you use information from a context document, you MUST append its source tag exactly as provided (e.g., [Source 1], [Source 2]) at the end of the relevant sentence.
3. Do not invent URLs or markdown links. Only use the literal text "[Source X]".`

func assembleContext(chunks []RAGChunk) string {
	var sb strings.Builder
	sb.WriteString("### Context Documents:\n")
	for i, chunk := range chunks {
		fmt.Fprintf(&sb, "\n[Source %d] (Document: %s, Type: %s)\n%s\n", i+1, chunk.DocumentName, chunk.ChunkType, chunk.Content)
	}
	return sb.String()
}

type Citation struct {
	CitationTag  string  `json:"citation_tag"`
	DocumentName string  `json:"document_name"`
	ChunkType    string  `json:"chunk_type"`
	ChunkIndex   *uint32 `json:"chunk_index"`
	ExactText    string  `json:"exact_text"`
}

func postProcessOutput(llmResponse string, chunks []RAGChunk) (string, []Citation) {
	citationRegex := regexp.MustCompile(`\[Source (\d+)\]`)
	matches := citationRegex.FindAllStringSubmatch(llmResponse, -1)

	seen := make(map[int]bool)
	var citations []Citation
	for _, m := range matches {
		idx, _ := strconv.Atoi(m[1])
		idx-- // 1-based → 0-based
		if idx >= 0 && idx < len(chunks) && !seen[idx] {
			seen[idx] = true
			citations = append(citations, Citation{
				CitationTag:  fmt.Sprintf("[Source %d]", idx+1),
				DocumentName: chunks[idx].DocumentName,
				ChunkType:    chunks[idx].ChunkType,
				ChunkIndex:   chunks[idx].ChunkIndex,
				ExactText:    chunks[idx].Content,
			})
		}
	}
	if citations == nil {
		citations = []Citation{}
	}
	return llmResponse, citations
}

// ─────────────────────────────────────────────────────────────────────────────
// @route  POST /v1/retrieve
// @desc   Hybrid search → RRF → rerank. Returns raw chunks.
// @access Private (apiAuth)
// ─────────────────────────────────────────────────────────────────────────────

func retrieveContext(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query         string   `json:"query"`
		DocumentName  string   `json:"document_name"`
		DocumentNames []string `json:"document_names"`
		TopN          int      `json:"top_n"`
	}
	body.TopN = 5 // default

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"Invalid request body"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Query) == "" {
		http.Error(w, "{\"`query` is required.\"}", http.StatusBadRequest)
		return
	}

	// Resolve target documents (mirrors the JS spread logic)
	var targetDocs []string
	if len(body.DocumentNames) > 0 {
		targetDocs = body.DocumentNames
	} else if body.DocumentName != "" {
		targetDocs = []string{body.DocumentName}
	}

	queryEmbedding, err := getQueryEmbedding(body.Query)
	if err != nil {
		log.Printf("[retrieve] Embedding error: %v\n", err)
		http.Error(w, `{"error":"Retrieval failed."}`, http.StatusInternalServerError)
		return
	}

	denseResults, sparseResults, err := hybridSearch(body.Query, queryEmbedding, targetDocs, 20)
	if err != nil {
		log.Printf("[retrieve] Hybrid search error: %v\n", err)
		http.Error(w, `{"error":"Retrieval failed."}`, http.StatusInternalServerError)
		return
	}

	fusedChunks := performRRF(denseResults, sparseResults, 60, 15)
	finalChunks := rerankChunks(body.Query, fusedChunks, body.TopN)

	anyReranked := false
	for _, c := range finalChunks {
		if c.RelevanceScore != nil {
			anyReranked = true
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"query":  body.Query,
		"chunks": finalChunks,
		"meta": map[string]any{
			"dense_count":     len(denseResults),
			"sparse_count":    len(sparseResults),
			"fused_count":     len(fusedChunks),
			"returned":        len(finalChunks),
			"reranked":        anyReranked,
			"document_filter": targetDocs,
		},
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// @route  POST /v1/rag
// @desc   Full RAG pipeline: retrieve → augment → generate → post-process
// @access Private (apiAuth)
// ─────────────────────────────────────────────────────────────────────────────

func ragGenerate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query         string   `json:"query"`
		DocumentName  string   `json:"document_name"`
		DocumentNames []string `json:"document_names"`
		TopN          int      `json:"top_n"`
		MaxTokens     int      `json:"max_tokens"`
		Stream        bool     `json:"stream"`
		Model         string   `json:"model"`
	}
	body.TopN = 5
	body.MaxTokens = 1024

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"Invalid request body"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Query) == "" {
		http.Error(w, `{"error":"`+"`query`"+` is required."}`, http.StatusBadRequest)
		return
	}

	var targetDocs []string
	if len(body.DocumentNames) > 0 {
		targetDocs = body.DocumentNames
	} else if body.DocumentName != "" {
		targetDocs = []string{body.DocumentName}
	}

	model := body.Model
	if model == "" {
		model = "qwen3.5"
	}

	llmClient := openai.NewClient(
		option.WithBaseURL(os.Getenv("MAIN_GPU_URL")),
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	)

	log.Printf("[rag] Query: \"%s\"\n", body.Query[:min(len(body.Query), 80)])

	queryEmbedding, err := getQueryEmbedding(body.Query)
	if err != nil {
		http.Error(w, `{"error":"RAG pipeline failed.", "detail":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	denseResults, sparseResults, err := hybridSearch(body.Query, queryEmbedding, targetDocs, 20)
	if err != nil {
		http.Error(w, `{"error":"RAG pipeline failed.", "detail":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	fusedChunks := performRRF(denseResults, sparseResults, 60, 15)
	retrievedChunks := rerankChunks(body.Query, fusedChunks, body.TopN)

	if len(retrievedChunks) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"query":     body.Query,
			"answer":    "I do not have enough information to answer this based on the provided documents.",
			"citations": []Citation{},
			"meta":      map[string]any{"retrieved": 0},
		})
		return
	}

	contextText := assembleContext(retrievedChunks)
	userContent := contextText + "\n\nUser Question: " + body.Query + "\nAnswer:"

	messages := []openai.ChatCompletionMessageParamUnion{
		{OfSystem: &openai.ChatCompletionSystemMessageParam{Role: "system", Content: openai.ChatCompletionSystemMessageParamContentUnion{OfString: openai.String(ragSystemPrompt)}}},
		{OfUser: &openai.ChatCompletionUserMessageParam{Role: "user", Content: openai.ChatCompletionUserMessageParamContentUnion{OfString: openai.String(userContent)}}},
	}

	log.Println("[rag] Generating answer...")

	anyReranked := false
	for _, c := range retrievedChunks {
		if c.RelevanceScore != nil {
			anyReranked = true
			break
		}
	}

	// ── Streaming ─────────────────────────────────────────────────────────────
	if body.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, canFlush := w.(http.Flusher)
		stream := llmClient.Chat.Completions.NewStreaming(context.Background(), openai.ChatCompletionNewParams{
			Model:               openai.ChatModel(model),
			Messages:            messages,
			MaxCompletionTokens: param.NewOpt(int64(body.MaxTokens)),
			Temperature:         param.NewOpt(float64(0.1)),
		})

		var fullText strings.Builder
		for stream.Next() {
			chunk := stream.Current()
			raw, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			if canFlush {
				flusher.Flush()
			}
			if len(chunk.Choices) > 0 {
				fullText.WriteString(chunk.Choices[0].Delta.Content)
			}
		}
		if err := stream.Err(); err != nil {
			log.Printf("[rag] Stream error: %v\n", err)
		}

		_, citations := postProcessOutput(fullText.String(), retrievedChunks)
		citationJSON, _ := json.Marshal(map[string]any{"type": "citations", "citations": citations})
		fmt.Fprintf(w, "data: %s\n\n", citationJSON)
		if canFlush {
			flusher.Flush()
		}
		return
	}

	// ── Non-streaming ─────────────────────────────────────────────────────────
	resp, err := llmClient.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:               openai.ChatModel(model),
		Messages:            messages,
		MaxCompletionTokens: param.NewOpt(int64(body.MaxTokens)),
		Temperature:         param.NewOpt(float64(0.1)),
	})
	if err != nil {
		log.Printf("[rag] LLM error: %v\n", err)
		http.Error(w, `{"error":"RAG pipeline failed."}`, http.StatusInternalServerError)
		return
	}

	rawText := strings.TrimSpace(resp.Choices[0].Message.Content)
	answer, citations := postProcessOutput(rawText, retrievedChunks)
	log.Printf("[rag] Done. %d citations.\n", len(citations))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"query":     body.Query,
		"answer":    answer,
		"citations": citations,
		"meta": map[string]any{
			"retrieved":       len(retrievedChunks),
			"dense_count":     len(denseResults),
			"sparse_count":    len(sparseResults),
			"reranked":        anyReranked,
			"document_filter": targetDocs,
		},
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// @route  GET /v1/documents/{id}/snapshot
// @desc   Reconstructs the original document text from chunks
// @access Private (apiAuth)
// ─────────────────────────────────────────────────────────────────────────────

func getDocumentSnapshot(w http.ResponseWriter, r *http.Request) {
	docIDStr := r.PathValue("id")
	docID, err := strconv.ParseUint(docIDStr, 10, 32)
	if err != nil {
		http.Error(w, `{"error":"Invalid document ID"}`, http.StatusBadRequest)
		return
	}

	ch := getClickhouseClient()
	if ch == nil {
		http.Error(w, `{"error":"Database not initialized"}`, http.StatusInternalServerError)
		return
	}

	// Fetch all text chunks for this document, sorted by index
	q := `
		SELECT chunk_index, content
		FROM rag_embeddings
		WHERE document_id = $1 
		ORDER BY chunk_index ASC
	`
	rows, err := ch.Query(context.Background(), q, uint32(docID))
	if err != nil {
		log.Printf("[snapshot] Query error: %v\n", err)
		http.Error(w, `{"error":"Failed to retrieve document chunks"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type chunkData struct {
		index   uint32
		content string
	}
	var chunks []chunkData
	for rows.Next() {
		var c chunkData
		if err := rows.Scan(&c.index, &c.content); err != nil {
			continue
		}
		chunks = append(chunks, c)
	}

	if len(chunks) == 0 {
		http.Error(w, `{"error":"Document not found or has no text chunks"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"Streaming not supported"}`, http.StatusInternalServerError)
		return
	}

	re := regexp.MustCompile(`([.!?])\s+([A-Z])`)
	var prevIndex uint32

	for i, c := range chunks {
		var textToAppend string

		if i == 0 {
			textToAppend = c.content
		} else {
			if c.index == prevIndex+1 {
				// Sequential chunk: trim first 30 words (overlap)
				words := strings.Fields(c.content)
				if len(words) > 30 {
					textToAppend = " " + strings.Join(words[30:], " ")
				} else {
					// If chunk has <= 30 words, just append space to be safe, though unusual
					textToAppend = " "
				}
			} else {
				// Gap exists
				textToAppend = "\n\n[...]\n\n" + c.content
			}
		}
		prevIndex = c.index

		// Apply Auto-Paragraph formatting to chunk
		textToAppend = re.ReplaceAllString(textToAppend, "$1\n\n$2")

		// Escape for HTML presentation and preserve newlines as <br>
		escaped := html.EscapeString(textToAppend)
		htmlChunk := strings.ReplaceAll(escaped, "\n", "<br>")

		// Emit explicit HTMX SSE 'chunk' event
		fmt.Fprintf(w, "event: chunk\ndata: %s\n\n", htmlChunk)
		flusher.Flush()
	}

	fmt.Fprintf(w, "event: close\ndata: \n\n")
	flusher.Flush()
}
