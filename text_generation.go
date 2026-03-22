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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	_ "image/gif"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
		resized, err := resizeImage(data, 1568)
		if err != nil {
			log.Printf("[file] Resize error for %s: %v\n", filename, err)
			return []processedFile{{fileType: "text", text: fmt.Sprintf("Error reading file %s", filename)}}, nil
		}
		return []processedFile{{fileType: "image", data: resized, mime: "image/jpeg"}}, nil

	case "image/heic":
		jpgData, err := convertHEIC(data)
		if err != nil {
			return []processedFile{{fileType: "text", text: fmt.Sprintf("Error processing HEIC file %s", filename)}}, nil
		}
		resized, err := resizeImage(jpgData, 1568)
		if err != nil {
			return []processedFile{{fileType: "image", data: jpgData, mime: "image/jpeg"}}, nil
		}
		return []processedFile{{fileType: "image", data: resized, mime: "image/jpeg"}}, nil

	case "application/pdf":
		pages, err := pdfToImages(data, 50)
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

func llamaChat(w http.ResponseWriter, r *http.Request) {
	// Parse the multipart form — multer sends files + JSON fields
	if err := r.ParseMultipartForm(512 << 20); err != nil {
		// Not multipart — try JSON body
		r.Body = io.NopCloser(r.Body)
	}

	// Extract JSON body fields (they may be JSON string values or multipart form values)
	var rawMessages json.RawMessage
	var maxTokens int64 = 500000
	var stream bool
	var model = "qwen3.5"
	var tools json.RawMessage

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
	} else {
		var body struct {
			Messages  json.RawMessage `json:"messages"`
			MaxTokens int64           `json:"max_tokens"`
			Stream    bool            `json:"stream"`
			Model     string          `json:"model"`
			Tools     json.RawMessage `json:"tools"`
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

	// Re-encode messages as JSON to build openai params
	messagesJSON, _ := json.Marshal(messages)

	// Build openai messages — since messages are arbitrary maps, we pass them raw
	// by marshaling into openai.ChatCompletionMessageParamUnion via JSON round-trip
	var openaiMessages []openai.ChatCompletionMessageParamUnion
	json.Unmarshal(messagesJSON, &openaiMessages)

	llmClient := openai.NewClient(
		option.WithBaseURL("http://192.168.1.235:8080/v1"),
		option.WithAPIKey("sk-1234567890"),
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
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	// ── Streaming ─────────────────────────────────────────────────────────────
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	} else {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}

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
}
