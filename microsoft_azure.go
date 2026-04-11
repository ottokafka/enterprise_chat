package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/confidential"
)

var pca confidential.Client
var redirectURI = "http://localhost:4445/redirect"
var globalClientID = "5aea4ce1-3074-4811-9c9b-d2e639d32e29"

func InitMSAL() {
	cred, err := confidential.NewCredFromSecret(os.Getenv("CLIENT_SECRET"))
	if err != nil {
		cred, _ = confidential.NewCredFromSecret("Vgi8Q~zEmklaniVvkVkOBamUXtEo4UObR2b~Sb5l")
	}

	clientID := os.Getenv("CLIENT_ID")
	if clientID != "" {
		globalClientID = clientID
	}

	cloudInstance := os.Getenv("CLOUD_INSTANCE")
	if cloudInstance == "" {
		cloudInstance = "https://login.microsoftonline.com/"
	}
	tenantID := os.Getenv("TENANT_ID")
	if tenantID == "" {
		tenantID = "4d0abef6-2673-47e8-90ee-11be6bf1cec9"
	}
	authority := cloudInstance + tenantID

	pca, err = confidential.New(authority, globalClientID, cred)
	if err != nil {
		log.Fatalf("Failed to initialize MSAL: %v", err)
	}

	rUri := os.Getenv("REDIRECT_URI")
	if rUri != "" {
		redirectURI = rUri
	}
}

type User struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	JobTitle string `json:"job_title"`
}

func findUser(email string) (*User, error) {
	rows, err := ClickhouseQuery("SELECT id, name, email, job_title FROM users WHERE email = ? LIMIT 1", email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.JobTitle); err != nil {
			return nil, err
		}
		return &u, nil
	}
	return nil, nil // Not found
}

func createUser(u User) error {
	id := time.Now().UnixMilli()
	_, err := ClickhouseExec(`
		INSERT INTO users (id, name, email, job_title) 
		VALUES (?, ?, ?, ?)
	`, id, u.Name, u.Email, u.JobTitle)
	return err
}

func createUserIfNotExist(u User) (*User, error) {
	existing, err := findUser(u.Email)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		err = createUser(u)
		if err != nil {
			return nil, err
		}
		return findUser(u.Email)
	}
	return existing, nil
}

func getMicrosoftProfile(accessToken string) string {
	req, _ := http.NewRequest("GET", "https://graph.microsoft.com/v1.0/me", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()

	var data struct {
		JobTitle string `json:"jobTitle"`
	}
	json.NewDecoder(resp.Body).Decode(&data)

	return data.JobTitle
}

func LoginHandler(w http.ResponseWriter, r *http.Request) {
	authCodeURL, err := pca.AuthCodeURL(context.Background(), globalClientID, redirectURI, []string{"user.read"})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, authCodeURL, http.StatusFound)
}

func RedirectHandler(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "No code in query string", http.StatusBadRequest)
		return
	}

	result, err := pca.AcquireTokenByAuthCode(context.Background(), code, redirectURI, []string{"user.read"})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	email := result.Account.PreferredUsername
	name := result.IDToken.Name
	jobTitle := getMicrosoftProfile(result.AccessToken)

	user, err := createUserIfNotExist(User{
		Name: name, Email: email, JobTitle: jobTitle,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	session, _ := Store.Get(r, "session")
	session.Values["account_name"] = name
	session.Values["account_email"] = email
	session.Values["user_id"] = user.ID
	session.Values["isAuthenticated"] = true
	session.Save(r, w)

	http.Redirect(w, r, "/chat", http.StatusFound)
}

func AuthStatusHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := Store.Get(r, "session")
	if auth, ok := session.Values["isAuthenticated"].(bool); ok && auth {
		w.Header().Set("HX-Redirect", "/chat")
		w.WriteHeader(http.StatusOK)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`
        <style>
            .login-card { background: white; border-radius: 16px; padding: 40px 32px; box-shadow: 0 10px 25px -5px rgba(0, 0, 0, 0.1); text-align: center; animation: fadeIn 0.5s ease-out; }
            .ms-logo { width: 48px; height: 48px; margin-bottom: 24px; }
            .title { font-size: 28px; color: #111827; margin: 0 0 12px 0; font-weight: 800; }
            .subtitle { color: #6B7280; margin-bottom: 32px; }
            .btn-ms { background: #00A4EF; color: white; padding: 14px 28px; border-radius: 8px; font-weight: 600; text-decoration: none; display: inline-flex; align-items: center; gap: 12px; transition: background 0.2s, transform 0.2s; box-shadow: 0 4px 6px -1px rgba(0, 164, 239, 0.2); }
            .btn-ms:hover { background: #0078D4; transform: translateY(-1px); box-shadow: 0 6px 8px -1px rgba(0, 164, 239, 0.3); }
        </style>
        <div class="login-card">
            <div style="display: grid; grid-template-columns: 20px 20px; gap: 4px; margin: 0 auto 24px; width: 44px;">
                <div style="width: 20px; height: 20px; background: #F25022;"></div>
                <div style="width: 20px; height: 20px; background: #7FBA00;"></div>
                <div style="width: 20px; height: 20px; background: #00A4EF;"></div>
                <div style="width: 20px; height: 20px; background: #FFB900;"></div>
            </div>
            <h2 class="title">Sign in to Dashboard</h2>
            <div class="subtitle">Access your personalized secure portal.</div>
            <a href="/login" class="btn-ms">Sign in with Microsoft</a>
        </div>
    `))
}

func GetUserHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := Store.Get(r, "session")
	name, ok1 := session.Values["account_name"].(string)
	email, ok2 := session.Values["account_email"].(string)

	if !ok1 || !ok2 {
		http.Error(w, `{"error": "Unauthorized"}`, http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"name":  name,
		"email": email,
	})
}

func GetAllUsersHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := ClickhouseQuery("SELECT id, name, job_title FROM users ORDER BY name")
	if err != nil {
		http.Error(w, `{"error": "Failed to fetch users"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type publicUser struct {
		ID       int    `json:"id"`
		Name     string `json:"name"`
		JobTitle string `json:"job_title"`
	}
	var users []publicUser
	for rows.Next() {
		var u publicUser
		if err := rows.Scan(&u.ID, &u.Name, &u.JobTitle); err == nil {
			users = append(users, u)
		}
	}
	if users == nil {
		users = []publicUser{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(users)
}

func LogoutHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := Store.Get(r, "session")
	session.Options.MaxAge = -1
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusFound)
}

func insertApiChat(userId int, usageType string) error {
	query := `INSERT INTO usage_log (user_id, ai_usage_type) 
	          SETTINGS async_insert=1, wait_for_async_insert=0 
	          VALUES (?, ?)`
	_, err := ClickhouseExec(query, userId, usageType)
	return err
}

func ApiAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			// Claude code anthropic support
			authHeader = r.Header.Get("x-api-key")
		}

		var userId int = 0
		session, _ := Store.Get(r, "session")

		if uid, ok := session.Values["user_id"].(int); ok && uid > 0 {
			userId = uid
		} else if authHeader != "" {
			apiKey := authHeader
			if strings.HasPrefix(authHeader, "Bearer ") {
				apiKey = authHeader[7:]
			}

			if apiKey == os.Getenv("OPENAI_API_KEY") {
				next.ServeHTTP(w, r)
				return
			}

			rows, err := ClickhouseQuery(`SELECT user_id FROM api_key WHERE key = ?`, apiKey)
			if err != nil {
				http.Error(w, `{"error": "Server error"}`, http.StatusInternalServerError)
				return
			}
			defer rows.Close()

			if rows.Next() {
				rows.Scan(&userId)
			} else {
				http.Error(w, `{"error": "Invalid API key"}`, http.StatusUnauthorized)
				return
			}
		} else {
			http.Error(w, `{"error": "Unauthorized: Missing authentication"}`, http.StatusUnauthorized)
			return
		}

		if userId > 0 {
			usageType := "chat"
			path := r.URL.Path
			if path == "/v1/music/generations" {
				usageType = "music"
			} else if path == "/v1/images/generations" {
				usageType = "image"
			} else if path == "/v1/chat/completions" {
				usageType = "chat"
			} else if path == "/v1/video/generations" {
				usageType = "video"
			}

			go func(uid int, utype string) {
				insertApiChat(uid, utype)
			}(userId, usageType)
		}

		// Inject the resolved userId into context so MCP tool handlers
		// (which receive context.Context, not *http.Request) can retrieve it.
		ctx := context.WithValue(r.Context(), userIdKey{}, userId)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
