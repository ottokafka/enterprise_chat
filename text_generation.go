package main

// text_generation.go — Port of controllers/text_generation.js
//
// Implements:
//   - File attachment processing: images (with resize via imaging), HEIC,
//     PDF (via ImageMagick convert), XLSX (standard library), DOCX (archive/zip + encoding/xml)
//   - POST /v1/chat/completions — pass-through to vLLM (non-streaming + SSE streaming)
//
// Library mapping from port_nodejs_to_golang.md:
//   sharp       → github.com/disintegration/imaging  (pure Go, no CGO)
//   heic-convert → NOTE: No pure-Go HEIC decoder is available without CGO.
//                  We use ImageMagick convert (same as pdf2pic approach) as a fallback.
//   pdf2pic     → os/exec + ImageMagick convert
//   xlsx        → archive/zip + encoding/xml for CSV export
//   mammoth     → archive/zip + encoding/xml (same approach as document_ingestion.go)

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
)

// ─────────────────────────────────────────────────────────────────────────────
// MIME type resolver — mirrors resolveMimeType() in text_generation.js
// ─────────────────────────────────────────────────────────────────────────────

func resolveMimeType(filename, contentType string) string {
	if contentType != "" && contentType != "application/octet-stream" {
		return contentType
	}
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".heic":
		return "image/heic"
	case ".pdf":
		return "application/pdf"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".csv":
		return "text/csv"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".doc":
		return "application/msword"
	case ".txt", ".md":
		return "text/plain"
	default:
		if contentType != "" {
			return contentType
		}
		return "application/octet-stream"
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// processedFile represents an extracted piece of content from an uploaded file
// ─────────────────────────────────────────────────────────────────────────────

type processedFile struct {
	fileType string // "image" or "text"
	data     []byte // raw image bytes (if image)
	mime     string // MIME type for image
	text     string // text content (if text)
}

// ─────────────────────────────────────────────────────────────────────────────
// Image processing — replaces sharp resize
// Uses github.com/disintegration/imaging (pure Go, no CGO)
// ─────────────────────────────────────────────────────────────────────────────

func resizeImage(data []byte, maxDim int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	// Fit inside maxDim x maxDim, maintaining aspect ratio (like sharp's 'inside' fit)
	resized := imaging.Fit(img, maxDim, maxDim, imaging.Lanczos)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// HEIC → JPEG via ImageMagick (no CGO pure-Go alternative available)
// ─────────────────────────────────────────────────────────────────────────────

func convertHEIC(data []byte) ([]byte, error) {
	tmpIn, err := os.CreateTemp("", "heic-*.heic")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpIn.Name())
	tmpIn.Write(data)
	tmpIn.Close()

	tmpOut, err := os.CreateTemp("", "heic-*.jpg")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpOut.Name())
	tmpOut.Close()

	cmd := exec.Command("convert", tmpIn.Name(), tmpOut.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Println("[Warning] heic failed ot convert to jpg")
		return nil, fmt.Errorf("ImageMagick heic convert error: %v — %s", err, out)
	}

	return os.ReadFile(tmpOut.Name())
}

// ─────────────────────────────────────────────────────────────────────────────
// PDF → PNG pages via ImageMagick convert (same approach as document_ingestion.go)
// ─────────────────────────────────────────────────────────────────────────────

func pdfToImages(data []byte, maxPages int) ([][]byte, error) {
	tmpPDF, err := os.CreateTemp("", "chat-*.pdf")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpPDF.Name())
	tmpPDF.Write(data)
	tmpPDF.Close()

	tmpDir, err := os.MkdirTemp("", "chat-pdf-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	outPattern := filepath.Join(tmpDir, "page-%04d.png")
	cmd := exec.Command("convert", "-density", "100", "-resize", "1200x", tmpPDF.Name(), outPattern)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ImageMagick PDF error: %v — %s", err, out)
	}

	entries, _ := filepath.Glob(filepath.Join(tmpDir, "page-*.png"))
	if len(entries) > maxPages {
		entries = entries[:maxPages]
	}

	var pages [][]byte
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err == nil {
			pages = append(pages, b)
		}
	}
	return pages, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// XLSX → CSV — uses archive/zip + encoding/xml
// XLSX is a ZIP containing xl/worksheets/sheet1.xml
// We parse <c> (cell) elements with <v> (value) children
// ─────────────────────────────────────────────────────────────────────────────

type xlsxCell struct {
	R string `xml:"r,attr"` // cell ref e.g. A1
	T string `xml:"t,attr"` // type: "s" = shared string, "n" = number
	V string `xml:"v"`      // raw value
}
type xlsxRow struct {
	Cells []xlsxCell `xml:"c"`
}
type xlsxSheetData struct {
	Rows []xlsxRow `xml:"row"`
}
type xlsxWorksheet struct {
	SheetData xlsxSheetData `xml:"sheetData"`
}

func xlsxToCSV(data []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}

	// Parse shared strings first (xl/sharedStrings.xml)
	var sharedStrings []string
	for _, f := range r.File {
		if f.Name == "xl/sharedStrings.xml" {
			rc, err := f.Open()
			if err != nil {
				break
			}
			type si struct {
				T string `xml:"t"`
			}
			type sstDoc struct {
				SI []si `xml:"si"`
			}
			var sstData sstDoc
			xml.NewDecoder(rc).Decode(&sstData)
			rc.Close()
			for _, s := range sstData.SI {
				sharedStrings = append(sharedStrings, s.T)
			}
			break
		}
	}

	// Parse first worksheet
	for _, f := range r.File {
		if f.Name == "xl/worksheets/sheet1.xml" {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			defer rc.Close()
			var ws xlsxWorksheet
			xml.NewDecoder(rc).Decode(&ws)

			var buf bytes.Buffer
			w := csv.NewWriter(&buf)
			for _, row := range ws.SheetData.Rows {
				var record []string
				for _, cell := range row.Cells {
					val := cell.V
					if cell.T == "s" { // shared string reference
						idx := 0
						fmt.Sscanf(cell.V, "%d", &idx)
						if idx < len(sharedStrings) {
							val = sharedStrings[idx]
						}
					}
					record = append(record, val)
				}
				w.Write(record)
			}
			w.Flush()
			return buf.String(), nil
		}
	}
	return "", fmt.Errorf("sheet1.xml not found in xlsx")
}

// ─────────────────────────────────────────────────────────────────────────────
// processIncomingFile — mirrors the JS function of the same name
// ─────────────────────────────────────────────────────────────────────────────

func processIncomingFile(filename, contentType string, data []byte) ([]processedFile, error) {
	mime := resolveMimeType(filename, contentType)

	switch mime {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		// No resizing — send raw image data
		return []processedFile{{fileType: "image", data: data, mime: mime}}, nil

	case "image/heic":
		jpgData, err := convertHEIC(data)
		if err != nil {
			return []processedFile{{fileType: "text", text: fmt.Sprintf("Error processing HEIC file %s", filename)}}, nil
		}
		// No resizing — send converted JPEG directly
		return []processedFile{{fileType: "image", data: jpgData, mime: "image/jpeg"}}, nil

	case "application/pdf":
		pages, err := pdfToImages(data, 9999) // Lifted from 50
		if err != nil {
			log.Printf("[file] PDF conversion error: %v\n", err)
			return []processedFile{{fileType: "text", text: "Failed to read PDF pages."}}, nil
		}
		result := make([]processedFile, len(pages))
		for i, p := range pages {
			result[i] = processedFile{fileType: "image", data: p, mime: "image/png"}
		}
		return result, nil

	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "text/csv":
		csvText, err := xlsxToCSV(data)
		if err != nil {
			// For raw CSV just return as text
			return []processedFile{{fileType: "text", text: string(data)}}, nil
		}
		return []processedFile{{fileType: "text", text: csvText}}, nil

	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		text, err := extractDocxText(data)
		if err != nil {
			return []processedFile{{fileType: "text", text: fmt.Sprintf("Error reading %s", filename)}}, nil
		}
		return []processedFile{{fileType: "text", text: text}}, nil

	case "application/msword":
		// Word 97-2003 binary (.doc) — use the custom CFB extractor
		cfb, cleanup, err := openCFBFromBytes(data)
		if err != nil {
			return []processedFile{{fileType: "text", text: fmt.Sprintf("Error opening .doc file %s: %v", filename, err)}}, nil
		}
		defer cleanup()
		text, err := extractText(cfb)
		if err != nil || text == "" {
			return []processedFile{{fileType: "text", text: fmt.Sprintf("Could not extract text from %s", filename)}}, nil
		}
		return []processedFile{{fileType: "text", text: text}}, nil

	default:
		if strings.HasPrefix(mime, "text/") {
			return []processedFile{{fileType: "text", text: string(data)}}, nil
		}
		return []processedFile{{fileType: "text", text: fmt.Sprintf("Unsupported file type: %s", filename)}}, nil
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// @route  POST /v1/chat/completions
// @desc   Chat completions proxy to local vLLM (OpenAI-compatible)
//         Supports: text-only, file attachments, streaming
// @access Private (apiAuth)
// ─────────────────────────────────────────────────────────────────────────────

func openAiChat(w http.ResponseWriter, r *http.Request) {
	// Parse the multipart form — multer sends files + JSON fields
	if err := r.ParseMultipartForm(MaxUploadBytes); err != nil {
		// Not multipart — try JSON body
		r.Body = io.NopCloser(r.Body)
	}

	// Extract JSON body fields (they may be JSON string values or multipart form values)
	var rawMessages json.RawMessage
	var maxTokens int64 = 200000
	var stream bool
	var model = "qwen3.5"
	var tools json.RawMessage
	var webSearch bool

	if r.MultipartForm != nil {
		// Multipart request — values come as form fields
		if v := r.FormValue("messages"); v != "" {
			rawMessages = json.RawMessage(v)
		}
		if v := r.FormValue("stream"); v == "true" {
			stream = true
		}
		if v := r.FormValue("model"); v != "" {
			model = v
		}
		if v := r.FormValue("tools"); v != "" {
			tools = json.RawMessage(v)
		}
		if v := r.FormValue("max_tokens"); v != "" {
			fmt.Sscanf(v, "%d", &maxTokens)
		}
		if v := r.FormValue("web_search"); v == "true" {
			webSearch = true
		}
	} else {
		var body struct {
			Messages  json.RawMessage `json:"messages"`
			MaxTokens int64           `json:"max_tokens"`
			Stream    bool            `json:"stream"`
			Model     string          `json:"model"`
			Tools     json.RawMessage `json:"tools"`
			WebSearch bool            `json:"web_search"`
		}
		body.MaxTokens = maxTokens
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"Invalid request body"}`, http.StatusBadRequest)
			return
		}
		rawMessages = body.Messages
		if body.MaxTokens > 0 {
			maxTokens = body.MaxTokens
		}
		stream = body.Stream
		if body.Model != "" {
			model = body.Model
		}
		tools = body.Tools
		webSearch = body.WebSearch
	}

	// messages is a []map[string]any so we preserve arbitrary openai-format structures
	var messages []map[string]any
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		http.Error(w, `{"error":"Invalid messages format"}`, http.StatusBadRequest)
		return
	}

	// Process file attachments and inject them into the last user message
	if r.MultipartForm != nil {
		var allFiles []struct {
			name        string
			contentType string
			data        []byte
		}
		for _, fhs := range r.MultipartForm.File {
			for _, fh := range fhs {
				f, err := fh.Open()
				if err != nil {
					continue
				}
				data, err := io.ReadAll(f)
				f.Close()
				if err != nil {
					continue
				}
				allFiles = append(allFiles, struct {
					name        string
					contentType string
					data        []byte
				}{fh.Filename, fh.Header.Get("Content-Type"), data})
			}
		}

		if len(allFiles) > 0 && len(messages) > 0 {
			lastMsg := messages[len(messages)-1]
			// Convert string content → array of content parts (same as JS)
			if contentStr, ok := lastMsg["content"].(string); ok {
				lastMsg["content"] = []map[string]any{{"type": "text", "text": contentStr}}
			}
			contentParts, ok := lastMsg["content"].([]map[string]any)
			if !ok {
				contentParts = []map[string]any{}
			}

			for _, file := range allFiles {
				processed, err := processIncomingFile(file.name, file.contentType, file.data)
				if err != nil {
					log.Printf("[chat] File process error %s: %v\n", file.name, err)
					continue
				}
				log.Printf("[chat] Processing file slices for %s: %d\n", file.name, len(processed))
				for _, p := range processed {
					if p.fileType == "image" {
						b64 := base64.StdEncoding.EncodeToString(p.data)
						contentParts = append(contentParts, map[string]any{
							"type": "image_url",
							"image_url": map[string]string{
								"url": fmt.Sprintf("data:%s;base64,%s", p.mime, b64),
							},
						})
					} else {
						contentParts = append(contentParts, map[string]any{
							"type": "text",
							"text": fmt.Sprintf("\n\n--- Content from %s ---\n%s", file.name, p.text),
						})
					}
				}
			}
			lastMsg["content"] = contentParts
			messages[len(messages)-1] = lastMsg
		}
	}

	headersWritten := false
	writeHeadersOnce := func() {
		if !headersWritten && stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			if origin := r.Header.Get("Origin"); origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			} else {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
			headersWritten = true
		}
	}

	// ==========================================
	// 🔍 WEB SEARCH PIPELINE INTERCEPTION
	// ==========================================
	var finalWebResults []SearchResult

	if webSearch && len(messages) > 0 {
		var flusher http.Flusher
		var canFlush bool
		if stream {
			writeHeadersOnce()
			flusher, canFlush = w.(http.Flusher)
		}

		var progressMu sync.Mutex
		progressCb := func(msg string) {
			log.Println(msg)
			if stream {
				progressMu.Lock()
				defer progressMu.Unlock()

				// Send as reasoning partial
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
				fmt.Fprintf(w, "data: %s\n\n", rawChunk)
				if canFlush {
					flusher.Flush()
				}
			}
		}

		if stream {
			progressCb("[search] web search generating optimal search queries...")
		}

		queries, err := generateSearchQueries(
			context.Background(),
			messages,
			searchLLMModel(),
			searchLLMAPIKey(),
			searchLLMBaseURL(),
		)

		if err == nil && len(queries) > 0 {
			var webResults []SearchResult
			var contextText string
			contextText, webResults = executeParallelSearches(queries, true, progressCb)
			finalWebResults = webResults

			injection := fmt.Sprintf("\n\n--- REAL-TIME WEB SEARCH CONTEXT ---\n%s\n\nPlease answer my question using the context above. Cite sources as [N].", contextText)

			lastIdx := len(messages) - 1
			lastMsg := messages[lastIdx]

			if contentStr, ok := lastMsg["content"].(string); ok {
				lastMsg["content"] = contentStr + injection
			} else if contentParts, ok := lastMsg["content"].([]map[string]any); ok {
				contentParts = append(contentParts, map[string]any{
					"type": "text",
					"text": injection,
				})
				lastMsg["content"] = contentParts
			} else if contentParts, ok := lastMsg["content"].([]any); ok {
				contentParts = append(contentParts, map[string]any{
					"type": "text",
					"text": injection,
				})
				lastMsg["content"] = contentParts
			}
			messages[lastIdx] = lastMsg
		}
	}
	// ==========================================

	// Re-encode messages as JSON to build openai params
	messagesJSON, _ := json.Marshal(messages)

	// Build openai messages — since messages are arbitrary maps, we pass them raw
	// by marshaling into openai.ChatCompletionMessageParamUnion via JSON round-trip
	var openaiMessages []openai.ChatCompletionMessageParamUnion
	json.Unmarshal(messagesJSON, &openaiMessages)

	llmClient := openai.NewClient(
		option.WithBaseURL(os.Getenv("MAIN_GPU_URL")),
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	)

	params := openai.ChatCompletionNewParams{
		Model:               openai.ChatModel(model),
		Messages:            openaiMessages,
		MaxCompletionTokens: param.NewOpt(maxTokens),
	}

	// Attach tools if provided
	if len(tools) > 0 && string(tools) != "null" {
		var toolList []openai.ChatCompletionToolParam
		if err := json.Unmarshal(tools, &toolList); err == nil && len(toolList) > 0 {
			params.Tools = toolList
		}
	}

	// ── Non-streaming ─────────────────────────────────────────────────────────
	if !stream {
		resp, err := llmClient.Chat.Completions.New(context.Background(), params)
		if err != nil {
			log.Printf("[chat] Error: %v\n", err)
			http.Error(w, `{"error":"Chat completion failed"}`, http.StatusInternalServerError)
			return
		}

		if len(finalWebResults) > 0 && len(resp.Choices) > 0 {
			var sb strings.Builder
			sb.WriteString("\n\n---\n**Sources:**\n")
			for i, r := range finalWebResults {
				sb.WriteString(fmt.Sprintf("[%d] [%s](%s)\n", i+1, r.Title, r.URL))
			}
			resp.Choices[0].Message.Content += sb.String()
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	// ── Streaming ─────────────────────────────────────────────────────────────
	writeHeadersOnce()

	flusher, canFlush := w.(http.Flusher)
	streamResp := llmClient.Chat.Completions.NewStreaming(context.Background(), params)
	for streamResp.Next() {
		chunk := streamResp.Current()
		raw, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		if canFlush {
			flusher.Flush()
		}
	}
	if err := streamResp.Err(); err != nil {
		log.Printf("[chat] Stream error: %v\n", err)
	}

	if len(finalWebResults) > 0 {
		var sb strings.Builder
		sb.WriteString("\n\n---\n**Sources:**\n")
		for i, r := range finalWebResults {
			sb.WriteString(fmt.Sprintf("[%d] [%s](%s)\n", i+1, r.Title, r.URL))
		}

		chunk := map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"delta": map[string]interface{}{
						"content": sb.String(),
					},
				},
			},
		}
		raw, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		if canFlush {
			flusher.Flush()
		}
	}
}

func fetchAnthropicURLImage(imageURL string) (string, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	req, err := http.NewRequest("GET", imageURL, nil)
	if err != nil {
		return "", err
	}
	// Many CDNs reject requests with no User-Agent
	req.Header.Set("User-Agent", "EnterpriseChatProxy/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad status %d", resp.StatusCode)
	}

	// Limit read to 20MB
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return "", err
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(bodyBytes)
	}

	b64 := base64.StdEncoding.EncodeToString(bodyBytes)
	dataURI := fmt.Sprintf("data:%s;base64,%s", contentType, b64)

	return dataURI, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// @route  POST /v1/messages
// @desc   Chat completions proxy accepting Anthropic format and routing to local vLLM
// ─────────────────────────────────────────────────────────────────────────────

func anthropicChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model     string           `json:"model"`
		MaxTokens int64            `json:"max_tokens"`
		System    any              `json:"system"`
		Messages  []map[string]any `json:"messages"`
		Stream    bool             `json:"stream"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error": "Invalid request"}`, http.StatusBadRequest)
		return
	}

	if req.MaxTokens <= 0 {
		req.MaxTokens = 200000
	}

	var openaiMessages []map[string]any

	systemStr := ""
	if s, ok := req.System.(string); ok {
		systemStr = s
	} else if arr, ok := req.System.([]any); ok {
		for _, b := range arr {
			if block, ok := b.(map[string]any); ok {
				if t, ok := block["text"].(string); ok {
					systemStr += t
				}
			}
		}
	}

	if systemStr != "" {
		openaiMessages = append(openaiMessages, map[string]any{
			"role":    "system",
			"content": systemStr,
		})
	}

	for _, msg := range req.Messages {
		content := msg["content"]

		if contentArr, ok := content.([]any); ok {
			var newContent []map[string]any
			for _, item := range contentArr {
				if block, ok := item.(map[string]any); ok {
					blockType, _ := block["type"].(string)

					if blockType == "text" {
						newContent = append(newContent, map[string]any{
							"type": "text",
							"text": block["text"],
						})
					} else if blockType == "image" {
						if source, ok := block["source"].(map[string]any); ok {
							sourceType, _ := source["type"].(string)
							if sourceType == "base64" {
								mediaType, _ := source["media_type"].(string)
								data, _ := source["data"].(string)
								dataURI := fmt.Sprintf("data:%s;base64,%s", mediaType, data)
								newContent = append(newContent, map[string]any{
									"type": "image_url",
									"image_url": map[string]string{
										"url": dataURI,
									},
								})
							} else if sourceType == "url" {
								url, _ := source["url"].(string)
								dataURI, err := fetchAnthropicURLImage(url)
								if err != nil {
									log.Printf("[anthropic-proxy] image fetch failed: %v", err)
									continue
								}
								newContent = append(newContent, map[string]any{
									"type": "image_url",
									"image_url": map[string]string{
										"url": dataURI,
									},
								})
							}
						}
					}
				}
			}
			content = newContent
		}

		openaiMessages = append(openaiMessages, map[string]any{
			"role":    msg["role"],
			"content": content,
		})
	}

	openaiReq := map[string]any{
		"model":      req.Model,
		"messages":   openaiMessages,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
	}

	if req.Stream {
		openaiReq["stream_options"] = map[string]bool{"include_usage": true}
	}

	reqBody, _ := json.Marshal(openaiReq)

	baseURL := os.Getenv("MAIN_GPU_URL")
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	targetURL := baseURL + "chat/completions"

	httpReq, err := http.NewRequestWithContext(context.Background(), "POST", targetURL, bytes.NewBuffer(reqBody))
	if err != nil {
		http.Error(w, `{"error": "Failed to create upstream request"}`, http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		log.Printf("[anthropic-proxy] Error: %v\n", err)
		http.Error(w, `{"error":"Upstream connection failed"}`, http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		http.Error(w, string(bodyBytes), resp.StatusCode)
		return
	}

	if !req.Stream {
		var openaiResp struct {
			ID      string `json:"id"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&openaiResp); err != nil {
			http.Error(w, `{"error":"Failed to decode upstream response"}`, http.StatusInternalServerError)
			return
		}

		var contentStr string
		var stopReason = "end_turn"
		if len(openaiResp.Choices) > 0 {
			contentStr = openaiResp.Choices[0].Message.Content
			if openaiResp.Choices[0].FinishReason == "length" {
				stopReason = "max_tokens"
			}
		}

		anthropicResp := map[string]any{
			"id":    openaiResp.ID,
			"type":  "message",
			"role":  "assistant",
			"model": req.Model,
			"content": []map[string]any{
				{
					"type": "text",
					"text": contentStr,
				},
			},
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                openaiResp.Usage.PromptTokens,
				"output_tokens":               openaiResp.Usage.CompletionTokens,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropicResp)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	} else {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}

	flusher, canFlush := w.(http.Flusher)

	msgStart := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            "msg_anthropic_proxy",
			"type":          "message",
			"role":          "assistant",
			"model":         req.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                0,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		},
	}
	msgStartJSON, _ := json.Marshal(msgStart)
	fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", msgStartJSON)

	blockStart := map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	}
	blockStartJSON, _ := json.Marshal(blockStart)
	fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", blockStartJSON)

	if canFlush {
		flusher.Flush()
	}

	var stopReason string
	var completionTokens int
	var promptTokens int

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		dataStr := strings.TrimPrefix(line, "data: ")
		if dataStr == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}

		if err := json.Unmarshal([]byte(dataStr), &chunk); err != nil {
			log.Printf("[anthropic-proxy] stream parse error: %v", err)
			continue
		}

		if chunk.Usage != nil {
			promptTokens = chunk.Usage.PromptTokens
			completionTokens = chunk.Usage.CompletionTokens
		}

		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta.Content
			if delta != "" {
				deltaEvent := map[string]any{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]any{
						"type": "text_delta",
						"text": delta,
					},
				}
				deltaJSON, _ := json.Marshal(deltaEvent)
				fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", deltaJSON)
				if canFlush {
					flusher.Flush()
				}
			}
			if chunk.Choices[0].FinishReason != nil {
				reason := *chunk.Choices[0].FinishReason
				if reason == "stop" {
					stopReason = "end_turn"
				} else if reason == "length" {
					stopReason = "max_tokens"
				} else if reason != "" {
					stopReason = reason
				}
			}
		}
	}

	if stopReason == "" {
		stopReason = "end_turn"
	}

	blockStop := map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	}
	blockStopJSON, _ := json.Marshal(blockStop)
	fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n", blockStopJSON)

	messageDelta := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": stopReason,
		},
		"usage": map[string]any{
			"input_tokens":                promptTokens,
			"output_tokens":               completionTokens,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		},
	}
	messageDeltaJSON, _ := json.Marshal(messageDelta)
	fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n", messageDeltaJSON)

	msgStop := map[string]any{
		"type": "message_stop",
	}
	msgStopJSON, _ := json.Marshal(msgStop)
	fmt.Fprintf(w, "event: message_stop\ndata: %s\n\n", msgStopJSON)

	if canFlush {
		flusher.Flush()
	}
}
