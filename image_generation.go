package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// ImageGenRequest matches the shape of the incoming request and the outgoing JSON payload.
type ImageGenRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model,omitempty"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	Steps          int    `json:"steps,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

// imageGenerate handles POST /v1/images/generations — Port of controllers/image_generation.js
func imageGenerate(w http.ResponseWriter, r *http.Request) {
	var body ImageGenRequest
	// Decode incoming request
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		log.Printf("[image-gen] Decode error: %v", err)
		http.Error(w, `{"error":"Invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Apply defaults (merging JS logic and curl example requirements)
	if body.Prompt == "" {
		body.Prompt = "a white siamese cat"
	}
	if body.Size == "" {
		body.Size = "512x512"
	}
	if body.ResponseFormat == "" {
		body.ResponseFormat = "url"
	}
	if body.Model == "" {
		body.Model = "only default model flux"
	}
	if body.N == 0 {
		body.N = 1
	}
	if body.Steps == 0 {
		body.Steps = 20
	}

	// Re-marshal to send to the external service
	reqBody, _ := json.Marshal(body)

	// Determine endpoint URL
	imageGenURL := os.Getenv("IMAGE_GEN_URL")
	fullURL := imageGenURL + "/images/generations"

	// Authorization
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		apiKey = "fake_key" // fallback
	}

	log.Printf("[image-gen] Calling: %s", fullURL)

	req, err := http.NewRequest("POST", fullURL, bytes.NewBuffer(reqBody))
	if err != nil {
		log.Printf("[image-gen] Request creation error: %v", err)
		http.Error(w, `{"error":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[image-gen] HTTP request error: %v", err)
		http.Error(w, `{"error":"Image generation failed"}`, http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	// If the user requested b64_json, just proxy the response directly.
	if body.ResponseFormat == "b64_json" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Printf("[image-gen] Copy error: %v", err)
		}
		return
	}

	// Handle url generation by reading the b64 response from python and saving it
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	var pyResp struct {
		Created int `json:"created"`
		Data    []struct {
			B64Json string `json:"b64_json"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&pyResp); err != nil {
		log.Printf("[image-gen] Error decoding python response: %v", err)
		http.Error(w, `{"error":"Failed to decode image from server"}`, http.StatusInternalServerError)
		return
	}

	if len(pyResp.Data) == 0 || pyResp.Data[0].B64Json == "" {
		log.Printf("[image-gen] Missing b64_json in response")
		http.Error(w, `{"error":"Missing image data from server"}`, http.StatusInternalServerError)
		return
	}

	imgData, err := base64.StdEncoding.DecodeString(pyResp.Data[0].B64Json)
	if err != nil {
		log.Printf("[image-gen] Error decoding base64: %v", err)
		http.Error(w, `{"error":"Failed to decode base64 image"}`, http.StatusInternalServerError)
		return
	}

	filename := generateImageFilename(body.Prompt)
	filepathToSave := filepath.Join("images", filename)

	if err := os.WriteFile(filepathToSave, imgData, 0644); err != nil {
		log.Printf("[image-gen] Error saving file: %v", err)
		http.Error(w, `{"error":"Failed to save image file"}`, http.StatusInternalServerError)
		return
	}

	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = fmt.Sprintf("%s://%s", scheme, r.Host)
	}
	imageURLBase := origin

	// Create final json response with URL
	fileUrl := fmt.Sprintf("%s/images/%s", imageURLBase, filename)

	responsePayload := map[string]interface{}{
		"created": pyResp.Created,
		"data": []map[string]string{
			{"url": fileUrl},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responsePayload)
}

// GenerateImageFromTool is used by the MCP and LLM internal tools to fetch an image
// and securely store it, returning the generated file URL.
func GenerateImageFromTool(prompt, size string, steps int, baseURL string) (string, error) {
	reqData := ImageGenRequest{
		Prompt:         prompt,
		Size:           size,
		Steps:          steps,
		ResponseFormat: "url", // Always trigger URL storage logic
		Model:          "only default model flux",
		N:              1,
	}

	reqBody, _ := json.Marshal(reqData)

	imageGenURL := os.Getenv("IMAGE_GEN_URL")
	fullURL := imageGenURL + "/images/generations"

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		apiKey = "fake_key"
	}

	req, err := http.NewRequest("POST", fullURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("image generation failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var pyResp struct {
		Created int `json:"created"`
		Data    []struct {
			B64Json string `json:"b64_json"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&pyResp); err != nil {
		return "", err
	}

	if len(pyResp.Data) == 0 || pyResp.Data[0].B64Json == "" {
		return "", fmt.Errorf("missing image data from server")
	}

	imgData, err := base64.StdEncoding.DecodeString(pyResp.Data[0].B64Json)
	if err != nil {
		return "", err
	}

	filename := generateImageFilename(reqData.Prompt)
	filepathToSave := filepath.Join("images", filename)

	if err := os.WriteFile(filepathToSave, imgData, 0644); err != nil {
		return "", err
	}

	if baseURL == "" {
		baseURL = os.Getenv("IMAGE_URL")
	}
	return fmt.Sprintf("%s/images/%s", baseURL, filename), nil
}

func generateImageFilename(prompt string) string {
	llmClient := openai.NewClient(
		option.WithBaseURL(os.Getenv("MAIN_GPU_URL")),
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	)

	msgBytes, _ := json.Marshal(map[string]interface{}{
		"role":    "user",
		"content": fmt.Sprintf("Convert the following image prompt into a very short, snake_case filename without extension (max 3-4 words): %q. Output ONLY the filename, nothing else.", prompt),
	})
	var userMsg openai.ChatCompletionMessageParamUnion
	json.Unmarshal(msgBytes, &userMsg)

	resp, err := llmClient.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    openai.ChatModel("qwen3.5"),
		Messages: []openai.ChatCompletionMessageParamUnion{userMsg},
	})

	if err == nil && len(resp.Choices) > 0 {
		filename := strings.TrimSpace(resp.Choices[0].Message.Content)
		filename = strings.ReplaceAll(filename, "```", "")
		re := regexp.MustCompile(`[^a-zA-Z0-9_\-]`)
		filename = re.ReplaceAllString(filename, "")
		filename = strings.ToLower(filename)
		filename = strings.TrimLeft(filename, "-_")
		filename = strings.TrimRight(filename, "-_")
		if len(filename) > 0 {
			if len(filename) > 50 {
				filename = filename[:50]
			}
			// Generate timestamp
			ts := time.Now().Format("20060102150405") // e.g. 20251210143027

			return filename + "_" + ts + ".jpg"
		}
	}
	return fmt.Sprintf("%s.jpg", uuid.New().String())
}
