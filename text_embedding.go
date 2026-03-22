package main

// text_embedding.go — Port of controllers/text_embedding.js

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// @route  POST /v1/embeddings
// @desc   OpenAI-compatible text embedding proxy to local vLLM
// @access Public (no apiAuth in routes.js)
func textEmbedding(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Input interface{} `json:"input"` // string or []string
		Model string      `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"Invalid request body"}`, http.StatusBadRequest)
		return
	}

	client := openai.NewClient(
		option.WithBaseURL("http://192.168.1.235:8081/v1"),
		option.WithAPIKey("sk-1234567890"),
	)

	model := body.Model
	if model == "" {
		model = "Qwen3-Embedding-8B"
	}

	// Build input union — handle both string and []string
	var inputUnion openai.EmbeddingNewParamsInputUnion
	switch v := body.Input.(type) {
	case string:
		inputUnion = openai.EmbeddingNewParamsInputUnion{OfString: openai.String(v)}
	case []interface{}:
		strs := make([]string, 0, len(v))
		for _, s := range v {
			if sv, ok := s.(string); ok {
				strs = append(strs, sv)
			}
		}
		inputUnion = openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: strs}
	default:
		http.Error(w, `{"error":"input must be a string or array of strings"}`, http.StatusBadRequest)
		return
	}

	response, err := client.Embeddings.New(context.Background(), openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel(model),
		Input: inputUnion,
	})
	if err != nil {
		log.Printf("[text_embedding] Error: %v\n", err)
		http.Error(w, `{"error":"Failed to fetch embedding"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
