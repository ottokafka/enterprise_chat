package main

import (
	"crypto/subtle"
	"net/http"
)

const (
	adminUsername    = "admin"
	adminPassword    = "Automated@1"
	adminSyntheticID = 1 // session-only, no DB row required
)

// AdminLoginPageHandler serves the styled admin login form.
func AdminLoginPageHandler(w http.ResponseWriter, r *http.Request) {
	errMsg := r.URL.Query().Get("error")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminLoginHTML(errMsg)))
}

// AdminLoginHandler validates the posted credentials and creates an admin session.
func AdminLoginHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	// Constant-time comparison to mitigate timing attacks.
	usernameMatch := subtle.ConstantTimeCompare([]byte(username), []byte(adminUsername))
	passwordMatch := subtle.ConstantTimeCompare([]byte(password), []byte(adminPassword))

	if usernameMatch != 1 || passwordMatch != 1 {
		http.Redirect(w, r, "/admin/login?error=Invalid+username+or+password", http.StatusFound)
		return
	}

	session, _ := Store.Get(r, "session")
	session.Values["isAuthenticated"] = true
	session.Values["isAdmin"] = true
	session.Values["account_name"] = "Admin"
	session.Values["account_email"] = "admin@local"
	session.Values["user_id"] = adminSyntheticID
	if err := session.Save(r, w); err != nil {
		http.Error(w, "Failed to save session", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/chat", http.StatusFound)
}

// adminLoginHTML returns the self-contained login form HTML.
func adminLoginHTML(errMsg string) string {
	errorBlock := ""
	if errMsg != "" {
		errorBlock = `
		<div class="error-box">
			<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg>
			` + errMsg + `
		</div>`
	}

	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Admin Login — Enterprise Portal</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&display=swap" rel="stylesheet">
<style>
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

  body {
    font-family: 'Inter', system-ui, sans-serif;
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    background: linear-gradient(135deg, #EEF2FF 0%, #E0E7FF 100%);
    color: #1F2937;
  }

  .container { width: 100%; max-width: 420px; padding: 20px; }

  .card {
    background: white;
    border-radius: 20px;
    padding: 44px 36px;
    box-shadow: 0 20px 60px -10px rgba(99, 102, 241, 0.15), 0 4px 16px -4px rgba(0,0,0,0.08);
    animation: fadeUp 0.4s cubic-bezier(0.16, 1, 0.3, 1) both;
  }

  @keyframes fadeUp {
    from { opacity: 0; transform: translateY(16px); }
    to   { opacity: 1; transform: translateY(0); }
  }

  .shield-icon {
    width: 52px; height: 52px;
    background: linear-gradient(135deg, #6366F1, #8B5CF6);
    border-radius: 14px;
    display: flex; align-items: center; justify-content: center;
    margin: 0 auto 24px;
    box-shadow: 0 8px 20px -4px rgba(99, 102, 241, 0.4);
  }

  .title {
    font-size: 26px; font-weight: 800;
    color: #111827; text-align: center; margin-bottom: 6px;
  }

  .subtitle {
    font-size: 14px; color: #6B7280;
    text-align: center; margin-bottom: 32px;
  }

  .form-group { margin-bottom: 18px; }

  label {
    display: block; font-size: 13px; font-weight: 600;
    color: #374151; margin-bottom: 7px; letter-spacing: 0.01em;
  }

  input[type="text"], input[type="password"] {
    width: 100%;
    padding: 12px 14px;
    border: 1.5px solid #E5E7EB;
    border-radius: 10px;
    font-size: 15px; font-family: inherit;
    background: #F9FAFB;
    color: #111827;
    transition: border-color 0.2s, box-shadow 0.2s, background 0.2s;
    outline: none;
  }

  input[type="text"]:focus, input[type="password"]:focus {
    border-color: #6366F1;
    background: white;
    box-shadow: 0 0 0 3px rgba(99, 102, 241, 0.12);
  }

  .btn-submit {
    width: 100%; padding: 13px;
    background: linear-gradient(135deg, #6366F1, #8B5CF6);
    color: white; border: none; border-radius: 10px;
    font-size: 15px; font-weight: 700; font-family: inherit;
    cursor: pointer; margin-top: 8px;
    transition: opacity 0.2s, transform 0.15s, box-shadow 0.2s;
    box-shadow: 0 4px 14px -2px rgba(99, 102, 241, 0.45);
  }
  .btn-submit:hover { opacity: 0.92; transform: translateY(-1px); box-shadow: 0 6px 18px -2px rgba(99, 102, 241, 0.5); }
  .btn-submit:active { transform: translateY(0); }

  .error-box {
    display: flex; align-items: center; gap: 8px;
    background: #FEF2F2; border: 1px solid #FECACA;
    color: #DC2626; border-radius: 9px;
    padding: 11px 14px; font-size: 13px; font-weight: 500;
    margin-bottom: 20px; animation: shake 0.35s ease both;
  }
  @keyframes shake {
    0%,100% { transform: translateX(0); }
    20%      { transform: translateX(-6px); }
    60%      { transform: translateX(6px); }
  }

  .back-link {
    display: block; text-align: center;
    margin-top: 24px; font-size: 13px; color: #9CA3AF;
    text-decoration: none; transition: color 0.15s;
  }
  .back-link:hover { color: #6366F1; }
</style>
</head>
<body>
<div class="container">
  <div class="card">
    <div class="shield-icon">
      <svg width="26" height="26" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round">
        <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/>
      </svg>
    </div>
    <h1 class="title">Admin Login</h1>
    <p class="subtitle">Restricted access — administrators only</p>

    ` + errorBlock + `

    <form method="POST" action="/admin/login">
      <div class="form-group">
        <label for="username">Username</label>
        <input type="text" id="username" name="username" autocomplete="username" placeholder="admin" required>
      </div>
      <div class="form-group">
        <label for="password">Password</label>
        <input type="password" id="password" name="password" autocomplete="current-password" placeholder="••••••••••" required>
      </div>
      <button type="submit" class="btn-submit" id="admin-login-btn">Sign In</button>
    </form>

    <a href="/" class="back-link">← Back to Microsoft Login</a>
  </div>
</div>
</body>
</html>`
}
