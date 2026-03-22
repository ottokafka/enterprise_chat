package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
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
		body.ResponseFormat = "b64_json"
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
	if imageGenURL == "" {
		// Default to the one provided in the curl example if env is missing
		imageGenURL = "https://alice.forest-interactive.com/v1"
	}
	fullURL := imageGenURL + "/images/generations"

	// Authorization
	apiKey := os.Getenv("ALICE_API_KEY")
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

	// Forward the external service's response (status and body) back to the client
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("[image-gen] Copy error: %v", err)
	}
}
