package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
)

// userIdKey is the context key for the authenticated user's ID.
// Using a private type prevents key collisions with other packages.
type userIdKey struct{}

// userIdFromContext retrieves the authenticated user ID stored by ApiAuthMiddleware.
// Returns 0 if not set (unauthenticated or anonymous path).
func userIdFromContext(ctx context.Context) int {
	if uid, ok := ctx.Value(userIdKey{}).(int); ok {
		return uid
	}
	return 0
}

// CheckAuthMiddleware secures the /chat component and api user fetch
func CheckAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ := Store.Get(r, "session")
		auth, _ := session.Values["isAuthenticated"].(bool)
		uid, _ := session.Values["user_id"].(int)

		if auth && uid > 0 {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userIdKey{}, uid)))
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}

// SpaHandler serves static files from the 'public' folder and falls back to index.html mimicking the JS router catch-all.
func SpaHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0")

	// Avoid directory traversal attacks
	cleanPath := filepath.Clean(r.URL.Path)
	path := filepath.Join("public", cleanPath)

	if stat, err := os.Stat(path); err == nil && !stat.IsDir() {
		http.ServeFile(w, r, path)
		return
	}

	// Fallback to index.html
	http.ServeFile(w, r, filepath.Join("public", "index.html"))
}

func InitRoutes() *http.ServeMux {
	mux := http.NewServeMux()

	// OpenAI Compatible & Music Generations
	// mux.Handle("POST /v1/music/generations", ApiAuthMiddleware(http.HandlerFunc(generateMusicHandler)))
	mux.Handle("POST /v1/images/generations", ApiAuthMiddleware(http.HandlerFunc(imageGenerate)))
	mux.Handle("POST /v1/chat/completions", ApiAuthMiddleware(http.HandlerFunc(openAiChat)))
	mux.Handle("POST /v1/messages", ApiAuthMiddleware(http.HandlerFunc(anthropicChat))) // Claude code anthropic support
	mux.Handle("POST /v1/embeddings", http.HandlerFunc(textEmbedding))                  // text embedding doesn't use apiAuth in routes.js

	// RAG Document Ingestion
	mux.Handle("POST /v1/ingest", ApiAuthMiddleware(http.HandlerFunc(ingestDocument)))
	mux.Handle("GET /v1/ingest/progress", ApiAuthMiddleware(http.HandlerFunc(ingestProgress)))
	mux.Handle("GET /v1/documents", ApiAuthMiddleware(http.HandlerFunc(listDocuments)))
	mux.Handle("DELETE /v1/documents/{id}", ApiAuthMiddleware(http.HandlerFunc(deleteDocument)))
	mux.Handle("PATCH /v1/documents/{id}", ApiAuthMiddleware(http.HandlerFunc(updateDocumentGlobalStatus)))
	mux.Handle("POST /v1/documents/{id}/share", ApiAuthMiddleware(http.HandlerFunc(shareDocument)))

	// MCP (Model Context Protocol) — Streamable HTTP transport
	// Handles GET and POST at /v1/mcp for MCP session negotiation and tool calls.
	mux.Handle("/v1/mcp", ApiAuthMiddleware(MCPHandler))

	// Web Search
	mux.Handle("POST /v1/search", ApiAuthMiddleware(http.HandlerFunc(webSearch)))

	// RAG Context Retrieval
	mux.Handle("GET /v1/documents/{id}/snapshot", ApiAuthMiddleware(http.HandlerFunc(getDocumentSnapshot)))
	mux.Handle("POST /v1/retrieve", ApiAuthMiddleware(http.HandlerFunc(retrieveContext)))
	mux.Handle("POST /v1/rerank", http.HandlerFunc(rerankHandler))
	mux.Handle("POST /v1/rag", ApiAuthMiddleware(http.HandlerFunc(ragGenerate)))

	// API Keys
	mux.Handle("GET /fetch-api-key", ApiAuthMiddleware(http.HandlerFunc(fetchApiKey)))
	mux.Handle("POST /create-api-key", ApiAuthMiddleware(http.HandlerFunc(createApiKey)))
	mux.Handle("DELETE /delete-api-key", ApiAuthMiddleware(http.HandlerFunc(deleteApiKey)))

	// System Prompts
	mux.Handle("GET /v1/system-prompts", ApiAuthMiddleware(http.HandlerFunc(listSystemPrompts)))
	mux.Handle("POST /v1/system-prompts", ApiAuthMiddleware(http.HandlerFunc(createSystemPrompt)))
	mux.Handle("PATCH /v1/system-prompts/{id}", ApiAuthMiddleware(http.HandlerFunc(updateSystemPrompt)))
	mux.Handle("DELETE /v1/system-prompts/{id}", ApiAuthMiddleware(http.HandlerFunc(deleteSystemPrompt)))

	// Web UI

	mux.Handle("GET /chat", CheckAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0")
		http.ServeFile(w, r, filepath.Join(".", "public", "chat.html"))
	})))
	mux.Handle("GET /developer", CheckAuthMiddleware(http.HandlerFunc(DeveloperDocsHandler)))

	// Microsoft Azure SSO Routes
	mux.HandleFunc("GET /login", LoginHandler)
	mux.HandleFunc("GET /redirect", RedirectHandler)
	mux.HandleFunc("GET /auth-status", AuthStatusHandler)
	mux.HandleFunc("GET /logout", LogoutHandler)
	mux.Handle("GET /api/user", CheckAuthMiddleware(http.HandlerFunc(GetUserHandler)))
	mux.Handle("GET /api/users", CheckAuthMiddleware(http.HandlerFunc(GetAllUsersHandler)))

	// Map images static directory
	mux.Handle("/images/", http.StripPrefix("/images/", http.FileServer(http.Dir("./images"))))

	// Map catch-all SPA router (requires Go 1.22+ routing syntax)
	mux.HandleFunc("/", SpaHandler)

	return mux
}

// DeveloperDocsHandler serves the developer documentation page
func DeveloperDocsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0")
	http.ServeFile(w, r, filepath.Join(".", "public", "developer.html"))
}
