package main

// api_keys.go — Port of controllers/api_keys.js

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
)

// @route  POST /create-api-key
// @desc   Create a new API key for the authenticated user
// @access Private (apiAuth)
func createApiKey(w http.ResponseWriter, r *http.Request) {
	userId := getUserIdFromRequest(r)

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		http.Error(w, `{"error":"Server error"}`, http.StatusInternalServerError)
		return
	}
	apiKey := "sk-" + hex.EncodeToString(keyBytes)

	id := time.Now().UnixMilli()
	_, err := ClickhouseExec(
		"INSERT INTO api_key (id, key, user_id) VALUES (?, ?, ?)",
		id, apiKey, userId,
	)
	if err != nil {
		log.Printf("[api_keys] create error: %v\n", err)
		http.Error(w, `{"error":"Server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"id": id, "key": apiKey})
}

// @route  DELETE /delete-api-key
// @desc   Delete an API key by id for the authenticated user
// @access Private (apiAuth)
func deleteApiKey(w http.ResponseWriter, r *http.Request) {
	userId := getUserIdFromRequest(r)

	var body struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == 0 {
		http.Error(w, `{"error":"Missing id"}`, http.StatusBadRequest)
		return
	}

	_, err := ClickhouseExec(
		"ALTER TABLE api_key DELETE WHERE id = ? AND user_id = ?",
		body.ID, userId,
	)
	if err != nil {
		log.Printf("[api_keys] delete error: %v\n", err)
		http.Error(w, `{"error":"Server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "API key deleted successfully"})
}

// @route  GET /fetch-api-key
// @desc   Fetch all API keys for the authenticated user
// @access Private (apiAuth)
func fetchApiKey(w http.ResponseWriter, r *http.Request) {
	userId := getUserIdFromRequest(r)

	rows, err := ClickhouseQuery(
		`SELECT id, key, formatDateTime(created_at, '%d-%m-%Y') as created_at FROM api_key WHERE user_id = ?`,
		userId,
	)
	if err != nil {
		log.Printf("[api_keys] fetch error: %v\n", err)
		http.Error(w, `{"error":"Server error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type apiKeyRow struct {
		ID        string `json:"id"`
		Key       string `json:"key"`
		CreatedAt string `json:"created_at"`
	}

	var results []apiKeyRow
	for rows.Next() {
		var row apiKeyRow
		var id int
		rows.Scan(&id, &row.Key, &row.CreatedAt)
		row.ID = strconv.Itoa(id)
		results = append(results, row)
	}
	if results == nil {
		results = []apiKeyRow{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"results": results})
}
