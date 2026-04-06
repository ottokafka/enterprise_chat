package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/sessions"
)

// Store is the global session store variable.
// In Go, gorilla/sessions supports cookie based or postgres based stores.
// Since the Express app used secure_cookie = false with a 90 day max age, we replicate this securely enough.
var Store *sessions.CookieStore

func InitSessionStore() {
	secret := os.Getenv("EXPRESS_SESSION_SECRET")
	if secret == "" {
		secret = "Who_give_a_fuck"
	}

	// CookieStore is fine for holding small states.
	Store = sessions.NewCookieStore([]byte(secret))

	// Configure matching the JS settings
	Store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   90 * 24 * 60 * 60, // approx 3 months
		HttpOnly: true,
		Secure:   false, // per the JS code `secure_cookie = false`
	}
}

// GlobalMiddleware wraps all requests to log them (like the express middleware).
// and serves as the mounting point for router or application level middlewares.
func GlobalMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Log the request method and path, similar to Express logger in middleware.js
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Now().Format("02/01/2006")) // DD/MM/YYYY format used

		// Wait for next middleware in chain
		next.ServeHTTP(w, r)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Upload Middleware — replaces middleware/upload_middleware.js (multer memory storage)
//
// In Go, multipart/form-data file uploads are handled natively by net/http via
// r.ParseMultipartForm(maxBytes). No third-party library is needed.
//
// The handlers that need file access (openAiChat, ingestDocument) call
// r.ParseMultipartForm themselves. This helper wraps any such handler with a
// generous 512 MB body limit consistent with the Node.js app's 50 MB JSON limit
// but allowing for large file uploads.
// ─────────────────────────────────────────────────────────────────────────────

const MaxUploadBytes = 2048 << 20 // 2GB

// UploadMiddleware enforces the max request body size for file upload routes.
func UploadMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes)
		next.ServeHTTP(w, r)
	})
}
