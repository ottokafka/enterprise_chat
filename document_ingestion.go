package main

// document_ingestion.go — Port of controllers/document_ingestion.js
//
// Replaces:
//   - mammoth       → archive/zip + encoding/xml  (DOCX text extraction)
//   - adm-zip       → archive/zip                 (DOCX image extraction)
//   - pdf2pic       → dslipak/pdf + pdfcpu         (PDF → text + images)
//   - @clickhouse   → clickhouse-go/v2             (vector store)
//   - openai        → openai/openai-go             (embeddings + vision)
//   - EventEmitter  → Go channels + sync.Map       (SSE progress events)

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dslipak/pdf"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/pdfcpu/pdfcpu/pkg/api"
)

// ─────────────────────────────────────────────────────────────────────────────
// Embedding — replaces openai.embeddings.create
// ─────────────────────────────────────────────────────────────────────────────

func getEmbedding(text string) ([]float64, error) {
	client := openai.NewClient(
		option.WithBaseURL(os.Getenv("EMBEDDING_URL")),
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	)

	res, err := client.Embeddings.New(context.Background(), openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel("Qwen3-Embedding-8B"),
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: []string{text}},
	})
	if err != nil {
		return nil, fmt.Errorf("embedding error: %w", err)
	}
	if len(res.Data) == 0 {
		return nil, fmt.Errorf("embedding: empty response")
	}

	embedding := make([]float64, len(res.Data[0].Embedding))
	copy(embedding, res.Data[0].Embedding)
	return embedding, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Vision insight — replaces openai.chat.completions.create with image content
// ─────────────────────────────────────────────────────────────────────────────

const visionPrompt = "Describe this image in detail, capturing any data, charts, tables, or key concepts " +
	"so it can be accurately indexed in a text search database."

var mimeExtMap = map[string]string{
	"png":  "image/png",
	"webp": "image/webp",
}

func getVisionInsight(base64Image, extension string) (string, error) {
	mimeType, ok := mimeExtMap[strings.ToLower(extension)]
	if !ok {
		mimeType = "image/jpeg"
	}

	client := openai.NewClient(
		option.WithBaseURL(os.Getenv("MAIN_GPU_URL")),
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	)

	imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64Image)

	res, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model: openai.ChatModel("qwen3.5-30b-3b"),
		Messages: []openai.ChatCompletionMessageParamUnion{
			{
				OfUser: &openai.ChatCompletionUserMessageParam{
					Role: "user",
					Content: openai.ChatCompletionUserMessageParamContentUnion{
						OfArrayOfContentParts: []openai.ChatCompletionContentPartUnionParam{
							openai.TextContentPart(visionPrompt),
							openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
								URL: imageURL,
							}),
						},
					},
				},
			},
		},
		MaxCompletionTokens: param.NewOpt(int64(1024)),
	})
	if err != nil {
		return "", fmt.Errorf("vision error: %w", err)
	}
	if len(res.Choices) == 0 {
		return "", nil
	}
	return res.Choices[0].Message.Content, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Text chunking with sliding window + overlap
// ─────────────────────────────────────────────────────────────────────────────

func chunkTextWithOverlap(text string, maxWords, overlapWords int) []string {
	words := strings.Fields(text) // equivalent of split(/\s+/).filter(Boolean)
	var chunks []string
	step := maxWords - overlapWords
	if step <= 0 {
		step = 1
	}
	for i := 0; i < len(words); i += step {
		end := i + maxWords
		if end > len(words) {
			end = len(words)
		}
		chunk := strings.Join(words[i:end], " ")
		if strings.TrimSpace(chunk) != "" {
			chunks = append(chunks, chunk)
		}
	}
	return chunks
}

// ─────────────────────────────────────────────────────────────────────────────
// DOCX processor — replaces mammoth (text) + adm-zip (images)
// Uses standard library: archive/zip + encoding/xml
// ─────────────────────────────────────────────────────────────────────────────

var supportedImgExts = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "webp": true,
}

// EmbeddingRecord mirrors the shape inserted into ClickHouse rag_embeddings.
type EmbeddingRecord struct {
	UserID       uint32    `json:"user_id"       ch:"user_id"`
	DocumentID   uint32    `json:"document_id"   ch:"document_id"`
	DocumentName string    `json:"document_name" ch:"document_name"`
	ChunkType    string    `json:"chunk_type"    ch:"chunk_type"`
	Content      string    `json:"content"       ch:"content"`
	ChunkIndex   int       `json:"chunk_index"   ch:"chunk_index"`
	Embedding    []float64 `json:"embedding"     ch:"embedding"`
}

type ProgressFn func(stage string, current, total int)

// extractDocxText opens a DOCX (ZIP) and returns raw text from word/document.xml.
// This replaces mammoth.extractRawText.
func extractDocxText(data []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}

	for _, f := range r.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()

		// Walk XML tokens, collecting <w:t> text nodes
		var sb strings.Builder
		dec := xml.NewDecoder(rc)
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return sb.String(), nil // return what we have
			}
			if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "t" {
				var txt string
				if err := dec.DecodeElement(&txt, &se); err == nil {
					sb.WriteString(txt)
					sb.WriteString(" ")
				}
			}
		}
		return sb.String(), nil
	}
	return "", fmt.Errorf("word/document.xml not found in docx")
}

func processDocx(data []byte, documentName string, userId, documentId uint32, onProgress ProgressFn) ([]EmbeddingRecord, error) {
	var records []EmbeddingRecord

	// ── Text ──────────────────────────────────────────────────────────────────
	onProgress("Extracting text...", 0, 100)
	log.Printf("[docx] Extracting text: %s\n", documentName)
	rawText, err := extractDocxText(data)
	if err != nil {
		return nil, fmt.Errorf("[docx] text extraction failed: %w", err)
	}

	chunks := chunkTextWithOverlap(rawText, 150, 30)
	log.Printf("[docx] %d text chunks\n", len(chunks))

	for i, chunk := range chunks {
		onProgress("Embedding text chunks...", i, len(chunks))
		log.Printf("[docx] Processing chunk: %d out of %d\n", i, len(chunks))
		embedding, err := getEmbedding(chunk)
		if err != nil {
			log.Printf("[docx] embedding error chunk %d: %v\n", i, err)
			continue
		}
		records = append(records, EmbeddingRecord{
			UserID:       userId,
			DocumentID:   documentId,
			DocumentName: documentName,
			ChunkType:    "text",
			Content:      chunk,
			ChunkIndex:   i,
			Embedding:    embedding,
		})
	}

	// ── Images ────────────────────────────────────────────────────────────────
	log.Printf("[docx] Extracting images: %s\n", documentName)
	onProgress("Extracting images...", 0, 100)

	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		log.Printf("[docx] zip open for images failed: %v\n", err)
	} else {
		type imgEntry struct {
			f   *zip.File
			ext string
		}
		var imgEntries []imgEntry
		for _, f := range r.File {
			if f.FileInfo().IsDir() || !strings.HasPrefix(f.Name, "word/media/") {
				continue
			}
			ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(f.Name), "."))
			if supportedImgExts[ext] {
				imgEntries = append(imgEntries, imgEntry{f, ext})
			}
		}

		for idx, ie := range imgEntries {
			onProgress(fmt.Sprintf("Analyzing image %d/%d...", idx+1, len(imgEntries)), idx, len(imgEntries))
			rc, err := ie.f.Open()
			if err != nil {
				continue
			}
			imgData, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				continue
			}

			log.Printf("[docx] Processing image: %s\n", ie.f.Name)
			b64 := base64.StdEncoding.EncodeToString(imgData)
			insightText, err := getVisionInsight(b64, ie.ext)
			if err != nil || insightText == "" {
				continue
			}
			embedding, err := getEmbedding(insightText)
			if err != nil {
				log.Printf("[docx] embedding error for image: %v\n", err)
				continue
			}
			records = append(records, EmbeddingRecord{
				UserID:       userId,
				DocumentID:   documentId,
				DocumentName: documentName,
				ChunkType:    "image_insight",
				Content:      fmt.Sprintf("[Image Insight]: %s", insightText),
				ChunkIndex:   idx,
				Embedding:    embedding,
			})
		}
	}

	onProgress("Finalizing...", 100, 100)
	log.Printf("[docx] Done. %d records\n", len(records))
	return records, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// PDF processor — replaces ImageMagick (convert) using dslipak/pdf and pdfcpu
// ─────────────────────────────────────────────────────────────────────────────

func processPdf(data []byte, documentName string, userId, documentId uint32, onProgress ProgressFn) ([]EmbeddingRecord, error) {
	var records []EmbeddingRecord

	// 1. Setup Temp Files
	tmpPDF, err := os.CreateTemp("", "ingest-*.pdf")
	if err != nil {
		return nil, fmt.Errorf("temp file error: %w", err)
	}
	defer os.Remove(tmpPDF.Name())
	if _, err := tmpPDF.Write(data); err != nil {
		return nil, fmt.Errorf("write temp error: %w", err)
	}
	tmpPDF.Close()

	// 2. Extract Images to a temp directory using pdfcpu
	imgDir, err := os.MkdirTemp("", "pdf-images-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(imgDir)

	onProgress("Extracting text and images...", 10, 100)
	// pdfcpu extracts all embedded images as individual files
	err = api.ExtractImagesFile(tmpPDF.Name(), imgDir, nil, nil)
	if err != nil {
		log.Printf("[pdf] image extraction warning: %v", err)
	}

	// 3. Extract Text using dslipak/pdf
	contentReader, err := pdf.Open(tmpPDF.Name())
	if err != nil {
		return nil, fmt.Errorf("failed to open pdf for text: %w", err)
	}

	numPages := contentReader.NumPage()

	for i := 1; i <= numPages; i++ {
		onProgress(fmt.Sprintf("Processing page %d/%d...", i, numPages), i, numPages)

		// --- Part A: Handle Text ---
		page := contentReader.Page(i)
		text, _ := page.GetPlainText(nil)
		text = strings.TrimSpace(text)

		if text != "" {
			embedding, err := getEmbedding(text)
			if err == nil {
				records = append(records, EmbeddingRecord{
					UserID:       userId,
					DocumentID:   documentId,
					DocumentName: documentName,
					ChunkType:    "text",
					Content:      fmt.Sprintf("[Page %d Text]: %s", i, text),
					ChunkIndex:   len(records),
					Embedding:    embedding,
				})
			}
		}

		// --- Part B: Handle Extracted Images for this page ---
		// pdfcpu names images like: page_1_Image1.jpg, page_1_Image2.png, etc.
		pattern := filepath.Join(imgDir, fmt.Sprintf("page_%d_*.png", i))
		// Note: pdfcpu might output jpg/png depending on source. Check for both.
		imgFiles, _ := filepath.Glob(pattern)
		jpgPattern, _ := filepath.Glob(filepath.Join(imgDir, fmt.Sprintf("page_%d_*.jpg", i)))
		imgFiles = append(imgFiles, jpgPattern...)

		for _, imgPath := range imgFiles {
			imgData, err := os.ReadFile(imgPath)
			if err != nil {
				continue
			}

			// Run Vision API on the specific image found on this page
			b64 := base64.StdEncoding.EncodeToString(imgData)
			ext := filepath.Ext(imgPath)[1:] // remove dot
			insightText, err := getVisionInsight(b64, ext)
			if err != nil || insightText == "" {
				continue
			}

			embedding, err := getEmbedding(insightText)
			if err == nil {
				records = append(records, EmbeddingRecord{
					UserID:       userId,
					DocumentID:   documentId,
					DocumentName: documentName,
					ChunkType:    "image_insight",
					Content:      fmt.Sprintf("[Page %d Image Insight]: %s", i, insightText),
					ChunkIndex:   len(records),
					Embedding:    embedding,
				})
			}
		}
	}

	onProgress("Finalizing...", 100, 100)
	log.Printf("[pdf] Done. %d records\n", len(records))
	return records, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// processText — Plain text / Markdown / CSV processor
// ─────────────────────────────────────────────────────────────────────────────

func processText(data []byte, documentName string, userId, documentId uint32, onProgress ProgressFn) ([]EmbeddingRecord, error) {
	var records []EmbeddingRecord

	onProgress("Extracting text...", 0, 100)
	log.Printf("[text] Extracting text: %s\n", documentName)
	rawText := string(data)

	chunks := chunkTextWithOverlap(rawText, 150, 30)
	log.Printf("[text] %d text chunks\n", len(chunks))

	for i, chunk := range chunks {
		onProgress("Embedding text chunks...", i, len(chunks))
		log.Printf("[text] Processing chunk: %d out of %d\n", i, len(chunks))
		embedding, err := getEmbedding(chunk)
		if err != nil {
			log.Printf("[text] embedding error chunk %d: %v\n", i, err)
			continue
		}
		records = append(records, EmbeddingRecord{
			UserID:       userId,
			DocumentID:   documentId,
			DocumentName: documentName,
			ChunkType:    "text",
			Content:      chunk,
			ChunkIndex:   i,
			Embedding:    embedding,
		})
	}

	onProgress("Finalizing...", 100, 100)
	log.Printf("[text] Done. %d records\n", len(records))
	return records, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Processor registry — add new types here only
// ─────────────────────────────────────────────────────────────────────────────

type ProcessorFn func(data []byte, documentName string, userId, documentId uint32, onProgress ProgressFn) ([]EmbeddingRecord, error)

var processors = map[string]ProcessorFn{
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": processDocx,
	"application/pdf": processPdf,
	"text/plain":      processText,
	"text/markdown":   processText,
	"text/csv":        processText,
}

var extToMime = map[string]string{
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".pdf":  "application/pdf",
	".txt":  "text/plain",
	".md":   "text/markdown",
	".csv":  "text/csv",
}

func resolveContentType(contentType, filename string) string {
	if contentType != "" && contentType != "application/octet-stream" {
		return contentType
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if m, ok := extToMime[ext]; ok {
		return m
	}
	return contentType
}

// ─────────────────────────────────────────────────────────────────────────────
// Job tracking (replaces EventEmitter + Map<jobId, state>)
// ─────────────────────────────────────────────────────────────────────────────

type JobState struct {
	Status   string   `json:"status"`
	Stage    string   `json:"stage"`
	Progress int      `json:"progress"`
	Files    []string `json:"files"`
	Results  []any    `json:"results"`
}

type ingestionJob struct {
	state     JobState
	mu        sync.Mutex
	listeners []chan JobState
}

// activeJobs replaces the JS Map<string, jobState> + EventEmitter
var activeJobs sync.Map // map[string]*ingestionJob

func newJob(jobId string, fileNames []string) *ingestionJob {
	j := &ingestionJob{
		state: JobState{
			Status:   "processing",
			Stage:    "Starting...",
			Progress: 0,
			Files:    fileNames,
			Results:  []any{},
		},
	}
	activeJobs.Store(jobId, j)
	return j
}

func (j *ingestionJob) emitProgress(stage string, progress int) {
	j.mu.Lock()
	j.state.Stage = stage
	j.state.Progress = progress
	snapshot := j.state
	listenersCopy := make([]chan JobState, len(j.listeners))
	copy(listenersCopy, j.listeners)
	j.mu.Unlock()

	for _, ch := range listenersCopy {
		select {
		case ch <- snapshot:
		default:
		}
	}
}

func (j *ingestionJob) subscribe() chan JobState {
	ch := make(chan JobState, 10)
	j.mu.Lock()
	j.listeners = append(j.listeners, ch)
	j.mu.Unlock()
	return ch
}

func (j *ingestionJob) unsubscribe(ch chan JobState) {
	j.mu.Lock()
	for i, l := range j.listeners {
		if l == ch {
			j.listeners = append(j.listeners[:i], j.listeners[i+1:]...)
			break
		}
	}
	j.mu.Unlock()
}

func (j *ingestionJob) complete(results []any) {
	j.mu.Lock()
	j.state.Status = "completed"
	j.state.Progress = 100
	j.state.Stage = "Done"
	j.state.Results = results
	snapshot := j.state
	listenersCopy := make([]chan JobState, len(j.listeners))
	copy(listenersCopy, j.listeners)
	j.mu.Unlock()

	for _, ch := range listenersCopy {
		select {
		case ch <- snapshot:
		default:
		}
		close(ch)
	}
}

func getUserIdFromRequest(r *http.Request) int {
	session, err := Store.Get(r, "session")
	if err == nil {
		if uid, ok := session.Values["user_id"].(int); ok {
			return uid
		}
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// ClickHouse insert of embeddings
// ─────────────────────────────────────────────────────────────────────────────

func insertEmbeddings(records []EmbeddingRecord) error {
	ch := getClickhouseClient()
	if ch == nil {
		return fmt.Errorf("clickhouse client not initialized")
	}

	ctx := context.Background()
	batch, err := ch.PrepareBatch(ctx, "INSERT INTO rag_embeddings (user_id, document_id, document_name, chunk_type, content, chunk_index, embedding)")
	if err != nil {
		return fmt.Errorf("clickhouse prepare batch: %w", err)
	}

	for _, r := range records {
		if err := batch.Append(r.UserID, r.DocumentID, r.DocumentName, r.ChunkType, r.Content, r.ChunkIndex, r.Embedding); err != nil {
			return fmt.Errorf("clickhouse batch append: %w", err)
		}
	}

	return batch.Send()
}

// ─────────────────────────────────────────────────────────────────────────────
// @route    POST /v1/ingest
// @desc     Ingest documents into the RAG ClickHouse store
// @access   Private (apiAuth)
// ─────────────────────────────────────────────────────────────────────────────

func ingestDocument(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(MaxUploadBytes); err != nil { // Use MaxUploadBytes
		http.Error(w, `{"error":"Failed to parse multipart form"}`, http.StatusBadRequest)
		return
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		// Also try "file" (singular) like multer's upload.any()
		files = r.MultipartForm.File["file"]
	}
	if len(files) == 0 {
		http.Error(w, `{"error":"No files uploaded. Attach at least one document."}`, http.StatusBadRequest)
		return
	}

	userId := getUserIdFromRequest(r)
	isGlobal := r.FormValue("is_global") == "true"

	// Generate a job ID like: Math.random().toString(36).substring(2, 15)
	jobId := fmt.Sprintf("%x", rand.Int63())[:13]

	// Insert tracking records into Postgres
	type fileRecord struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	var fileRecords []fileRecord
	for _, fh := range files {
		id := time.Now().UnixMilli()
		_, err := ClickhouseExec(
			`INSERT INTO user_documents (id, user_id, document_name, status, is_global) VALUES (?, ?, ?, 'processing', ?)`,
			id, userId, fh.Filename, isGlobal,
		)
		if err != nil {
			log.Printf("CH Insert error: %v\n", err)
			continue
		}
		fileRecords = append(fileRecords, fileRecord{ID: int(id), Name: fh.Filename})
	}

	// Respond immediately with job ID — 202 Accepted
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{
		"message": "Ingestion started",
		"job_id":  jobId,
		"files":   fileRecords,
	})

	// Collect file names for job tracking
	fileNames := make([]string, len(files))
	for i, fh := range files {
		fileNames[i] = fh.Filename
	}

	// Read all file buffers before the goroutine (r.MultipartForm may get GC'd)
	type uploadedFile struct {
		ID          uint32
		Name        string
		ContentType string
		Data        []byte
	}
	var uploads []uploadedFile
	for i, fh := range files {
		f, err := fh.Open()
		if err != nil {
			log.Printf("[ingest] Cannot open uploaded file %s: %v\n", fh.Filename, err)
			continue
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			log.Printf("[ingest] Cannot read uploaded file %s: %v\n", fh.Filename, err)
			continue
		}
		ct := fh.Header.Get("Content-Type")
		uploads = append(uploads, uploadedFile{
			ID:          uint32(fileRecords[i].ID),
			Name:        fh.Filename,
			ContentType: ct,
			Data:        data,
		})
	}

	// Initialize job state
	job := newJob(jobId, fileNames)

	// Background goroutine — equivalent of the async IIFE in JS
	go func() {
		var results []any

		for idx, upload := range uploads {
			documentName := upload.Name
			mime := resolveContentType(upload.ContentType, documentName)
			processor, ok := processors[mime]

			if !ok {
				results = append(results, map[string]any{
					"document_name": documentName,
					"success":       false,
					"error":         fmt.Sprintf("Unsupported file type: %s", mime),
				})
				continue
			}

			log.Printf("[ingest] Starting: %s\n", documentName)

			totalFiles := len(uploads)
			onProgress := func(stage string, current, total int) {
				fileBaseProgress := (float64(idx) / float64(totalFiles)) * 100
				var internalProgress float64
				if total > 0 {
					internalProgress = (float64(current) / float64(total)) * (100.0 / float64(totalFiles))
				}
				overallProgress := int(fileBaseProgress + internalProgress)
				if overallProgress > 99 {
					overallProgress = 99
				}
				job.emitProgress(fmt.Sprintf("[%s] %s", documentName, stage), overallProgress)
			}

			records, err := processor(upload.Data, documentName, uint32(userId), upload.ID, onProgress)
			if err != nil {
				log.Printf("[ingest] Failed for %s: %v\n", documentName, err)
				ClickhouseExec(
					`ALTER TABLE user_documents UPDATE status = 'error' WHERE document_name = ? AND user_id = ?`,
					documentName, userId,
				)
				results = append(results, map[string]any{
					"document_name": documentName,
					"success":       false,
					"error":         err.Error(),
				})
				continue
			}

			if len(records) == 0 {
				results = append(results, map[string]any{
					"document_name": documentName,
					"success":       true,
					"ingested":      0,
					"message":       "No content extracted.",
				})
				continue
			}

			if err := insertEmbeddings(records); err != nil {
				log.Printf("[ingest] ClickHouse insert failed for %s: %v\n", documentName, err)
				ClickhouseExec(
					`ALTER TABLE user_documents UPDATE status = 'error' WHERE document_name = ? AND user_id = ?`,
					documentName, userId,
				)
				continue
			}

			ClickhouseExec(
				`ALTER TABLE user_documents UPDATE status = 'completed' WHERE document_name = ? AND user_id = ?`,
				documentName, userId,
			)

			textChunks := 0
			imageInsights := 0
			for _, rec := range records {
				if rec.ChunkType == "text" {
					textChunks++
				} else {
					imageInsights++
				}
			}

			results = append(results, map[string]any{
				"document_name": documentName,
				"success":       true,
				"ingested":      len(records),
				"breakdown": map[string]int{
					"text_chunks":    textChunks,
					"image_insights": imageInsights,
				},
			})
		}

		job.complete(results)

		// Clean up after 60 seconds (matches the JS 60000 ms timeout)
		time.AfterFunc(60*time.Second, func() {
			activeJobs.Delete(jobId)
		})
	}()
}

// ─────────────────────────────────────────────────────────────────────────────
// @route    GET /v1/ingest/progress
// @desc     SSE endpoint for observing ingest job progress
// @access   Private (apiAuth)
// ─────────────────────────────────────────────────────────────────────────────

func ingestProgress(w http.ResponseWriter, r *http.Request) {
	jobId := r.URL.Query().Get("job_id")
	if jobId == "" {
		http.Error(w, `{"error":"Missing job_id"}`, http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, canFlush := w.(http.Flusher)

	sendEvent := func(data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if canFlush {
			flusher.Flush()
		}
	}

	// Look up the job
	raw, ok := activeJobs.Load(jobId)
	if !ok {
		sendEvent(map[string]string{
			"status":  "completed",
			"message": "Job not found or already completed.",
		})
		return
	}

	job := raw.(*ingestionJob)

	// Send current state immediately
	job.mu.Lock()
	currentState := job.state
	job.mu.Unlock()
	sendEvent(currentState)

	if currentState.Status == "completed" {
		return
	}

	// Subscribe to future events
	ch := job.subscribe()
	defer job.unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case state, open := <-ch:
			if !open {
				return
			}
			sendEvent(state)
			if state.Status == "completed" || state.Status == "error" {
				return
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// @route    GET /v1/documents
// @desc     Get distinct documents uploaded by user
// @access   Private (apiAuth)
// ─────────────────────────────────────────────────────────────────────────────

func listDocuments(w http.ResponseWriter, r *http.Request) {
	userId := getUserIdFromRequest(r)
	rows, err := ClickhouseQuery(
		`SELECT id, user_id, document_name, status, is_global, shared_with_user_ids, created_at FROM user_documents WHERE user_id = ? OR is_global = true OR has(shared_with_user_ids, toUInt32(?)) ORDER BY created_at DESC`,
		userId, userId,
	)
	if err != nil {
		log.Printf("Failed to list docs: %v\n", err)
		http.Error(w, `{"error":"Failed to list documents"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type docRow struct {
		ID           int       `json:"id"`
		UserID       int       `json:"user_id"`
		DocumentName string    `json:"document_name"`
		Status       string    `json:"status"`
		IsGlobal     bool      `json:"is_global"`
		SharedWith   []uint32  `json:"shared_with_user_ids"`
		CreatedAt    time.Time `json:"created_at"`
	}

	var docs []docRow
	for rows.Next() {
		var d docRow
		rows.Scan(&d.ID, &d.UserID, &d.DocumentName, &d.Status, &d.IsGlobal, &d.SharedWith, &d.CreatedAt)
		docs = append(docs, d)
	}
	if docs == nil {
		docs = []docRow{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"documents": docs,
		"user_id":   userId,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// @route    DELETE /v1/documents/:id
// @desc     Delete a specific document tracking record and associated RAG vectors
// @access   Private (apiAuth)
// ─────────────────────────────────────────────────────────────────────────────

func deleteDocument(w http.ResponseWriter, r *http.Request) {
	userId := getUserIdFromRequest(r)
	docId, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, `{"error":"Invalid document id"}`, http.StatusBadRequest)
		return
	}

	// Look up document name first
	rows, err := ClickhouseQuery(
		`SELECT document_name FROM user_documents WHERE id = ? AND user_id = ?`,
		docId, userId,
	)
	if err != nil {
		http.Error(w, `{"error":"Failed to delete document"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	if !rows.Next() {
		http.Error(w, `{"error":"Document not found"}`, http.StatusNotFound)
		return
	}
	var documentName string
	rows.Scan(&documentName)
	rows.Close()

	// Delete from ClickHouse
	ClickhouseExec(
		`ALTER TABLE user_documents DELETE WHERE id = ? AND user_id = ?`,
		docId, userId,
	)

	// Delete from ClickHouse
	ch := getClickhouseClient()
	if ch != nil {
		err = ch.Exec(context.Background(),
			"ALTER TABLE rag_embeddings DELETE WHERE user_id = ? AND document_id = ?",
			userId, docId,
		)
		if err != nil {
			log.Printf("[delete] ClickHouse delete error: %v\n", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"message": "Document deleted",
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// @route    PATCH /v1/documents/:id
// @desc     Update global status of a document
// @access   Private (apiAuth)
// ─────────────────────────────────────────────────────────────────────────────

func updateDocumentGlobalStatus(w http.ResponseWriter, r *http.Request) {
	userId := getUserIdFromRequest(r)
	docId, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, `{"error":"Invalid document id"}`, http.StatusBadRequest)
		return
	}

	var body struct {
		IsGlobal *bool `json:"is_global"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.IsGlobal == nil {
		http.Error(w, `{"error":"Missing is_global value"}`, http.StatusBadRequest)
		return
	}

	_, err = ClickhouseExec(
		`ALTER TABLE user_documents UPDATE is_global = ? WHERE id = ? AND user_id = ?`,
		*body.IsGlobal, docId, userId,
	)
	if err != nil {
		log.Printf("Failed to update doc: %v\n", err)
		http.Error(w, `{"error":"Failed to update document"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"message": "Document updated",
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// @route    POST /v1/documents/:id/share
// @desc     Update sharing settings for a document
// @access   Private (apiAuth)
// ─────────────────────────────────────────────────────────────────────────────

func shareDocument(w http.ResponseWriter, r *http.Request) {
	userId := getUserIdFromRequest(r)
	docId, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, `{"error":"Invalid document id"}`, http.StatusBadRequest)
		return
	}

	var body struct {
		IsGlobal          *bool    `json:"is_global"`
		SharedWithUserIds []uint32 `json:"shared_with_user_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.IsGlobal == nil || body.SharedWithUserIds == nil {
		http.Error(w, `{"error":"Missing is_global or shared_with_user_ids value"}`, http.StatusBadRequest)
		return
	}

	_, err = ClickhouseExec(
		`ALTER TABLE user_documents UPDATE is_global = ?, shared_with_user_ids = ? WHERE id = ? AND user_id = ?`,
		*body.IsGlobal, body.SharedWithUserIds, docId, userId,
	)
	if err != nil {
		log.Printf("Failed to share doc: %v\n", err)
		http.Error(w, `{"error":"Failed to secure document sharing"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"message": "Document sharing updated",
	})
}
