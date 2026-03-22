package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
)

// SystemPrompt represents a system prompt record
type SystemPrompt struct {
	ID        uint32 `json:"id"`
	UserID    uint32 `json:"user_id"`
	Name      string `json:"name"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// @route  GET /v1/system-prompts
// @desc   List all system prompts for the authenticated user
func listSystemPrompts(w http.ResponseWriter, r *http.Request) {
	userId := getUserIdFromRequest(r)
	if userId == 0 {
		http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}

	rows, err := ClickhouseQuery(
		"SELECT id, user_id, name, content, formatDateTime(created_at, '%d-%m-%Y') as created_at FROM system_prompts WHERE user_id = ? ORDER BY created_at DESC",
		userId,
	)
	if err != nil {
		log.Printf("[system_prompts] list error: %v\n", err)
		http.Error(w, `{"error":"Server error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var prompts []SystemPrompt
	for rows.Next() {
		var p SystemPrompt
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Content, &p.CreatedAt); err != nil {
			log.Printf("[system_prompts] scan error: %v\n", err)
			continue
		}
		prompts = append(prompts, p)
	}

	if prompts == nil {
		prompts = []SystemPrompt{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(prompts)
}

// @route  POST /v1/system-prompts
// @desc   Create a new system prompt
func createSystemPrompt(w http.ResponseWriter, r *http.Request) {
	userId := getUserIdFromRequest(r)
	if userId == 0 {
		http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var body struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" || body.Content == "" {
		http.Error(w, `{"error":"Invalid request body"}`, http.StatusBadRequest)
		return
	}

	id := uint32(time.Now().UnixMilli() % 4294967295) // Fit into UInt32
	_, err := ClickhouseExec(
		"INSERT INTO system_prompts (id, user_id, name, content) VALUES (?, ?, ?, ?)",
		id, userId, body.Name, body.Content,
	)
	if err != nil {
		log.Printf("[system_prompts] create error: %v\n", err)
		http.Error(w, `{"error":"Server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"id": id, "message": "System prompt created successfully"})
}

// @route  PATCH /v1/system-prompts/{id}
// @desc   Update an existing system prompt
func updateSystemPrompt(w http.ResponseWriter, r *http.Request) {
	userId := getUserIdFromRequest(r)
	if userId == 0 {
		http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}

	// Extract ID from path (requires Go 1.22+ routing syntax)
	idStr := r.PathValue("id")
	id, _ := strconv.ParseUint(idStr, 10, 32)
	if id == 0 {
		http.Error(w, `{"error":"Invalid ID"}`, http.StatusBadRequest)
		return
	}

	var body struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"Invalid request body"}`, http.StatusBadRequest)
		return
	}

	// ClickHouse ALTER TABLE UPDATE is asynchronous but suitable for small tables like this.
	_, err := ClickhouseExec(
		"ALTER TABLE system_prompts UPDATE name = ?, content = ? WHERE id = ? AND user_id = ?",
		body.Name, body.Content, id, userId,
	)
	if err != nil {
		log.Printf("[system_prompts] update error: %v\n", err)
		http.Error(w, `{"error":"Server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "System prompt updated successfully"})
}

// @route  DELETE /v1/system-prompts/{id}
// @desc   Delete a system prompt
func deleteSystemPrompt(w http.ResponseWriter, r *http.Request) {
	userId := getUserIdFromRequest(r)
	if userId == 0 {
		http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}

	idStr := r.PathValue("id")
	id, _ := strconv.ParseUint(idStr, 10, 32)
	if id == 0 {
		http.Error(w, `{"error":"Invalid ID"}`, http.StatusBadRequest)
		return
	}

	_, err := ClickhouseExec(
		"ALTER TABLE system_prompts DELETE WHERE id = ? AND user_id = ?",
		id, userId,
	)
	if err != nil {
		log.Printf("[system_prompts] delete error: %v\n", err)
		http.Error(w, `{"error":"Server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "System prompt deleted successfully"})
}
