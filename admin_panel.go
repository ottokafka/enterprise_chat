package main

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strings"
)

// CheckAdminMiddleware guards /admin/* routes.
// Requires session["isAdmin"] == true, otherwise redirects to /admin/login.
func CheckAdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ := Store.Get(r, "session")
		isAdmin, _ := session.Values["isAdmin"].(bool)
		if !isAdmin {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		uid, _ := session.Values["user_id"].(int)
		ctx := context.WithValue(r.Context(), userIdKey{}, uid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AdminPanelHandler serves the admin usage dashboard.
func AdminPanelHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	weekly, err := usageLogReport(7)
	if err != nil || weekly == nil {
		weekly = []UsageRow{}
	}
	monthly, err := usageLogReport(30)
	if err != nil || monthly == nil {
		monthly = []UsageRow{}
	}

	w.Write([]byte(adminPanelHTML(weekly, monthly)))
}

// adminPanelHTML returns the admin page using the current sidebar (from chat.html) + weekly and monthly usage display (from email_report.go).
func adminPanelHTML(weekly, monthly []UsageRow) string {
	weeklyTable := renderUsageTable(weekly)
	monthlyTable := renderUsageTable(monthly)
	wu, wc, wi, wt := computeStats(weekly)
	mu, mc, mi, mt := computeStats(monthly)

	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0, maximum-scale=1.0, user-scalable=no">
<title>Admin — Usage Report</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&display=swap" rel="stylesheet">
<link rel="stylesheet" href="/chat.css">
<style>
  /* ── Admin-specific overrides (current sidebar + reports) ── */
  .main-area {
    display: flex;
    flex-direction: column;
    overflow: hidden;
    background: var(--bg-base);
  }

  .admin-body {
    flex: 1;
    overflow-y: auto;
    padding: 28px 36px;
  }

  .page-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 20px;
  }
  .page-title { font-size: 20px; font-weight: 800; color: var(--text-primary); }
  .page-subtitle { font-size: 13px; color: var(--text-muted); margin-top: 2px; }

  .refresh-btn {
    display: flex; align-items: center; gap: 7px;
    background: var(--bg-card);
    border: 1px solid var(--border);
    color: var(--text-muted);
    border-radius: var(--radius-sm);
    padding: 8px 14px;
    font-size: 13px; font-weight: 600; font-family: var(--font);
    cursor: pointer; transition: all var(--transition);
    text-decoration: none;
  }
  .refresh-btn:hover { color: var(--text-primary); border-color: var(--border-mid); }

  .reports-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(520px, 1fr));
    gap: 24px;
  }
  @media (max-width: 1100px) {
    .reports-grid { grid-template-columns: 1fr; }
  }

  .report-section {
    background: var(--bg-card);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    overflow: hidden;
  }

  .report-header {
    padding: 14px 18px 10px;
    border-bottom: 1px solid var(--border);
    background: var(--bg-surface);
  }
  .report-header h3 {
    margin: 0 0 4px;
    font-size: 15px;
    font-weight: 700;
    color: var(--text-primary);
  }
  .report-header .report-note {
    font-size: 12px;
    color: var(--text-muted);
  }

  .report-stats {
    display: flex;
    gap: 16px;
    padding: 12px 18px;
    background: var(--bg-card);
    border-bottom: 1px solid var(--border);
  }
  .report-stat {
    font-size: 12px;
    color: var(--text-muted);
  }
  .report-stat strong {
    font-size: 15px;
    font-weight: 700;
    color: var(--text-primary);
    margin-right: 4px;
  }

  .usage-table-wrap {
    background: var(--bg-card);
    overflow: hidden;
  }
  .usage-table {
    width: 100%;
    border-collapse: collapse;
  }
  .usage-table thead th {
    background: var(--bg-surface);
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 700;
    letter-spacing: 0.08em;
    text-transform: uppercase;
    padding: 11px 16px;
    text-align: left;
    border-bottom: 1px solid var(--border);
  }
  .usage-table tbody tr {
    border-bottom: 1px solid var(--border);
    transition: background var(--transition);
  }
  .usage-table tbody tr:last-child { border-bottom: none; }
  .usage-table tbody tr:hover { background: var(--bg-hover); }
  .usage-table td {
    padding: 11px 16px;
    font-size: 13px;
    color: var(--text-primary);
    vertical-align: middle;
  }

  .avatar-sm {
    width: 28px; height: 28px;
    border-radius: 50%;
    background: var(--accent);
    display: inline-flex; align-items: center; justify-content: center;
    font-size: 11px; font-weight: 700; color: #fff;
    flex-shrink: 0;
  }
  .name-cell { display: flex; align-items: center; gap: 9px; }
  .name-text { font-weight: 600; }
  .job-text { font-size: 11.5px; color: var(--text-muted); margin-top: 1px; }

  .pill {
    display: inline-flex; align-items: center; gap: 4px;
    padding: 2px 9px; border-radius: 20px; font-size: 12px; font-weight: 600;
  }
  .pill-chat  { background: rgba(99,102,241,0.15); color: #a5b4fc; }
  .pill-image { background: rgba(168,85,247,0.15); color: #d8b4fe; }

  .last-used-text { color: var(--text-muted); font-size: 12px; }

  .empty-state {
    text-align: center; padding: 36px 20px; color: var(--text-muted); font-size: 13px;
  }

  /* Sidebar nav items for admin (inline to match current sidebar) */
  .sidebar-nav-item {
    display: flex; align-items: center; gap: 10px;
    padding: 8px 11px; margin: 2px 0;
    border-radius: var(--radius-sm);
    color: var(--text-muted); text-decoration: none; font-size: 13px; font-weight: 500;
    transition: all var(--transition);
  }
  .sidebar-nav-item:hover { background: var(--bg-hover); color: var(--text-primary); }
  .sidebar-nav-item.active { background: var(--bg-hover); color: var(--text-primary); font-weight: 600; }
</style>
</head>
<body>
<div class="app-shell">

  <!-- ══ Current Sidebar (adapted from chat.html) ══════════════════════ -->
  <aside class="sidebar" aria-label="Navigation">
    <div class="sidebar-header">
      <div class="sidebar-logo">
        <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="2">
          <path d="M12 2a10 10 0 0 1 10 10c0 5.523-4.477 10-10 10a10 10 0 0 1-10-10C2 6.477 6.477 2 12 2z"/>
          <path d="M8 12h8M12 8v8"/>
        </svg>
        Enterprise Chat
      </div>
    </div>

    <div class="sidebar-body" style="padding: 8px 8px 12px;">
      <div class="sidebar-section-label" style="color: white; padding: 0 6px 6px; font-size: 11px; letter-spacing: 0.5px;">MENU</div>

      <a href="/chat" class="sidebar-nav-item">
        <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2">
          <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>
        </svg>
        Chat
      </a>
      <a href="/admin" class="sidebar-nav-item active">
        <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2">
          <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/>
        </svg>
        Usage Report
      </a>
      <a href="/developer" class="sidebar-nav-item">
        <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2">
          <polyline points="16 18 22 12 16 6"/><polyline points="8 6 2 12 8 18"/>
        </svg>
        Developer
      </a>
    </div>

    <div class="sidebar-footer">
      <div class="user-profile">
        <div class="avatar-circle" style="background: var(--accent);">A</div>
        <div class="user-info">
          <div class="user-name">Admin</div>
          <div class="user-email">admin@local</div>
        </div>
        <div style="margin-left: auto;">
          <a href="/logout" style="display:flex; align-items:center; gap:6px; color: var(--text-muted); text-decoration:none; font-size:12px; padding:6px; border-radius:6px; transition: all var(--transition);"
             onmouseover="this.style.color='#ef4444'" onmouseout="this.style.color='var(--text-muted)'" data-tooltip="Logout">
            <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2">
              <path d="M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4M10 17l5-5-5-5M15 12H3"/>
            </svg>
          </a>
        </div>
      </div>
    </div>
  </aside>

  <!-- ══ Main Area ════════════════════════════════════════ -->
  <main class="main-area">
    <!-- Topbar (current style) -->
    <div class="topbar">
      <span class="topbar-title">Usage Report</span>
      <div style="flex:1;"></div>
      <a href="/admin" class="refresh-btn">
        <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2.2">
          <polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/>
          <path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/>
        </svg>
        Refresh
      </a>
    </div>

    <div class="admin-body">

      <div class="page-header">
        <div>
          <div class="page-title">AI Usage — Weekly &amp; Monthly</div>
          <div class="page-subtitle">Data from usage_log (matches email reports)</div>
        </div>
      </div>

      <div class="reports-grid">

        <!-- Weekly -->
        <div class="report-section">
          <div class="report-header">
            <h3>nPro AI Weekly Usage Report</h3>
            <div class="report-note">Scheduled every Monday at 8 AM. Covers 7 days of chat and image generation usage.</div>
          </div>
          <div class="report-stats">
            <div class="report-stat"><strong>` + fmt.Sprintf("%d", wu) + `</strong> users</div>
            <div class="report-stat"><strong>` + fmt.Sprintf("%d", wc) + `</strong> chats</div>
            <div class="report-stat"><strong>` + fmt.Sprintf("%d", wi) + `</strong> images</div>
            <div class="report-stat"><strong>` + fmt.Sprintf("%d", wt) + `</strong> total</div>
          </div>
          <div class="usage-table-wrap">
            <table class="usage-table">
              <thead>
                <tr>
                  <th>User</th>
                  <th>Chats</th>
                  <th>Images</th>
                  <th>Last Used</th>
                </tr>
              </thead>
              <tbody>
                ` + weeklyTable + `
              </tbody>
            </table>
          </div>
        </div>

        <!-- Monthly -->
        <div class="report-section">
          <div class="report-header">
            <h3>nPro AI Monthly Usage Report</h3>
            <div class="report-note">Scheduled every 1st of the month at 8 AM. Covers 30 days of chat and image generation usage.</div>
          </div>
          <div class="report-stats">
            <div class="report-stat"><strong>` + fmt.Sprintf("%d", mu) + `</strong> users</div>
            <div class="report-stat"><strong>` + fmt.Sprintf("%d", mc) + `</strong> chats</div>
            <div class="report-stat"><strong>` + fmt.Sprintf("%d", mi) + `</strong> images</div>
            <div class="report-stat"><strong>` + fmt.Sprintf("%d", mt) + `</strong> total</div>
          </div>
          <div class="usage-table-wrap">
            <table class="usage-table">
              <thead>
                <tr>
                  <th>User</th>
                  <th>Chats</th>
                  <th>Images</th>
                  <th>Last Used</th>
                </tr>
              </thead>
              <tbody>
                ` + monthlyTable + `
              </tbody>
            </table>
          </div>
        </div>

      </div>

    </div>
  </main>

</div>
</body>
</html>`
}

// renderUsageTable builds a styled usage table (server-rendered).
func renderUsageTable(rows []UsageRow) string {
	if len(rows) == 0 {
		return `<tr><td colspan="4"><div class="empty-state">No usage data for this period.</div></td></tr>`
	}
	var sb strings.Builder
	for _, r := range rows {
		initial := "U"
		if len(r.Name) > 0 {
			initial = strings.ToUpper(string(r.Name[0]))
		}
		name := html.EscapeString(r.Name)
		job := html.EscapeString(r.JobTitle)
		last := html.EscapeString(r.LastUsed)
		sb.WriteString(fmt.Sprintf(`
<tr>
  <td>
    <div class="name-cell">
      <div class="avatar-sm">%s</div>
      <div>
        <div class="name-text">%s</div>
        <div class="job-text">%s</div>
      </div>
    </div>
  </td>
  <td><span class="pill pill-chat">
    <svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>
    %d
  </span></td>
  <td><span class="pill pill-image">
    <svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="8.5" cy="8.5" r="1.5"/><polyline points="21 15 16 10 5 21"/></svg>
    %d
  </span></td>
  <td><span class="last-used-text">%s</span></td>
</tr>`, initial, name, job, r.ChatCount, r.ImageCount, last))
	}
	return sb.String()
}

// computeStats returns (users, chats, images, total) for a set of rows.
func computeStats(rows []UsageRow) (int, int, int, int) {
	users := len(rows)
	chats, images := 0, 0
	for _, r := range rows {
		chats += r.ChatCount
		images += r.ImageCount
	}
	return users, chats, images, chats + images
}
