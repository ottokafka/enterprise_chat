package main

import (
	"fmt"
	"log"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// ─── Data Types ──────────────────────────────────────────────────────────────

// UsageRow holds one row from the usage report query.
type UsageRow struct {
	Name       string
	JobTitle   string
	ChatCount  int
	ImageCount int
	LastUsed   string
}

// ─── email_template.go logic ─────────────────────────────────────────────────

func aliceCSSStyle() string {
	return `
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<style>
body {
    font-family: Helvetica, Arial, sans-serif;
    margin: 0;
    padding: 0;
    line-height: 1.5;
}
.container {
    max-width: 1000px;
    margin: 25px auto;
    padding: 10px;
}
table {
    border-collapse: collapse;
    border: 1px solid black;
}
th, td {
    border: 1px solid black;
    padding: 10px;
}
@media screen and (max-width: 600px) {
    .container {
        margin: 20px;
        padding: 10px;
    }
}
footer {
    text-align: center;
    margin-top: 20px;
}
</style>
`
}

func createUsageTable(data []UsageRow) string {
	var sb strings.Builder
	sb.WriteString("<table>")
	sb.WriteString(`
<thead>
<tr>
    <th>Name</th>
    <th>Job Title</th>
    <th>Chat</th>
    <th>Image</th>
    <th>Last Used</th>
</tr>
</thead>`)
	sb.WriteString("<tbody>")
	for _, row := range data {
		sb.WriteString(fmt.Sprintf(`
<tr>
    <td>%s</td>
    <td>%s</td>
    <td>%d</td>
    <td>%d</td>
    <td>%s</td>
</tr>`, row.Name, row.JobTitle, row.ChatCount, row.ImageCount, row.LastUsed))
	}
	sb.WriteString("</tbody>")
	sb.WriteString("</table>")
	return sb.String()
}

func monthlyHTMLReport(data []UsageRow) string {
	return fmt.Sprintf(`
<div class="container">
%s
<h3>nPro AI Monthly Usage Report</h3>
<h4>Scheduled every 1st of the month at 8 AM. Covers 30 days of chat, and image generation usage.</h4>
%s
</div>
`, aliceCSSStyle(), createUsageTable(data))
}

func weeklyHTMLReport(data []UsageRow) string {
	return fmt.Sprintf(`
<div class="container">
%s
<h3>nPro AI Weekly Usage Report</h3>
<h4>Scheduled every Monday at 8 AM. Covers 7 days of chat, and image generation usage.</h4>
%s
</div>
`, aliceCSSStyle(), createUsageTable(data))
}

// ─── email_config.go + email_report.go logic ─────────────────────────────────

// mailOut sends an HTML email via SMTP (mirrors the JS mail_out function).
func mailOut(htmlBody string) error {
	host := os.Getenv("EMAIL_HOST")
	port := os.Getenv("EMAIL_PORT")
	user := os.Getenv("EMAIL_USER")
	pass := os.Getenv("EMAIL_PASS")

	if port == "" {
		port = "1025"
	}

	addr := fmt.Sprintf("%s:%s", host, port)

	from := "noreply@npro.ai"
	to := []string{}
	cc := []string{"joe@forest-interactive.com", "otto@forest-interactive.com"}

	allRecipients := append(to, cc...)

	// Build RFC 2822 message
	headers := strings.Join([]string{
		fmt.Sprintf("From: \"nPro\" <%s>", from),
		fmt.Sprintf("To: %s", strings.Join(to, ", ")),
		fmt.Sprintf("Cc: %s", strings.Join(cc, ", ")),
		"Subject: nPro Ai usage report",
		"MIME-Version: 1.0",
		"Content-Type: text/html; charset=\"UTF-8\"",
	}, "\r\n")

	msg := []byte(headers + "\r\n\r\n" + htmlBody)

	var auth smtp.Auth
	if user != "" && pass != "" {
		auth = smtp.PlainAuth("", user, pass, host)
	}

	if err := smtp.SendMail(addr, auth, from, allRecipients, msg); err != nil {
		fmt.Printf("email failed to send: %v", err)
		return err
	}

	log.Printf("email sent successfully via %s", addr)
	return nil
}

// SendWeeklyReport generates and sends the weekly HTML usage report.
func SendWeeklyReport(data []UsageRow) error {
	html := weeklyHTMLReport(data)
	return mailOut(html)
}

// SendMonthlyReport generates and sends the monthly HTML usage report.
func SendMonthlyReport(data []UsageRow) error {
	html := monthlyHTMLReport(data)
	return mailOut(html)
}

// ─── cron_jobs.go logic ──────────────────────────────────────────────────────

// usageLogReport runs the DB query and returns rows for the given day range.
func usageLogReport(days int) ([]UsageRow, error) {
	if days <= 0 {
		days = 7
	}

	query := fmt.Sprintf(`
	SELECT
		u.name,
		u.job_title,
		ul.chat_count,
		ul.image_count,
		formatDateTime(ul.last_used_at, '%%d-%%m-%%Y %%I:%%M %%p') AS last_used
	FROM users AS u
	INNER JOIN (
		SELECT
			user_id,
			countIf(ai_usage_type = 'chat') AS chat_count,
			countIf(ai_usage_type = 'image') AS image_count,
			max(created_at) AS last_used_at
		FROM usage_log
		WHERE created_at >= now() - toIntervalDay(%d)
		GROUP BY user_id
	) AS ul ON u.id = ul.user_id
	ORDER BY ul.last_used_at DESC NULLS LAST;
	`, days)

	rows, err := ClickhouseQuery(query)
	if err != nil {
		return nil, fmt.Errorf("usageLogReport query failed: %w", err)
	}
	defer rows.Close()

	var result []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Name, &r.JobTitle, &r.ChatCount, &r.ImageCount, &r.LastUsed); err != nil {
			log.Printf("usageLogReport scan error: %v", err)
			continue
		}
		result = append(result, r)
	}
	return result, nil
}

// scheduleAt schedules fn to run every time the wall-clock matches hour:minute
// on the given weekday (use -1 to run every day) or day-of-month (monthDay > 0).
// This is a simple cron-like loop (no external dependency needed).
func scheduleAt(name string, hour, minute, weekday, monthDay int, fn func()) {
	go func() {
		log.Printf("[cron] %s scheduler started", name)
		for {
			now := time.Now()
			// Calculate next run
			next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			// Advance until weekday/month-day matches
			for {
				if weekday >= 0 && int(next.Weekday()) != weekday {
					next = next.Add(24 * time.Hour)
					continue
				}
				if monthDay > 0 && next.Day() != monthDay {
					next = next.Add(24 * time.Hour)
					continue
				}
				break
			}
			log.Printf("[cron] %s next run at %s", name, next.Format(time.RFC1123))
			time.Sleep(time.Until(next))
			fn()
		}
	}()
}

// InitEmailCronJobs starts the weekly and monthly email report cron jobs.
// Call this from main() after InitDB().
func InitEmailCronJobs() {
	// Weekly: every Monday (weekday=1) at 08:00
	scheduleAt("WeeklyReport", 8, 0, int(time.Monday), -1, func() {
		if os.Getenv("NODE_ENV") != "production" {
			log.Println("[cron] WeeklyReport skipped (not production)")
			return
		}
		log.Printf("[cron] Running Weekly nPro Usage report at %s", time.Now().Format(time.RFC1123))
		data, err := usageLogReport(7)
		if err != nil {
			log.Printf("[cron] WeeklyReport DB error: %v", err)
			return
		}
		if err := SendWeeklyReport(data); err != nil {
			log.Printf("[cron] WeeklyReport send error: %v", err)
		} else {
			log.Println("[cron] Weekly nPro Usage report completed")
		}
	})

	// Monthly: 1st of every month (monthDay=1) at 08:00
	scheduleAt("MonthlyReport", 8, 0, -1, 1, func() {
		if os.Getenv("NODE_ENV") != "production" {
			log.Println("[cron] MonthlyReport skipped (not production)")
			return
		}
		log.Printf("[cron] Running Monthly nPro Usage report at %s", time.Now().Format(time.RFC1123))
		data, err := usageLogReport(30)
		if err != nil {
			log.Printf("[cron] MonthlyReport DB error: %v", err)
			return
		}
		if err := SendMonthlyReport(data); err != nil {
			log.Printf("[cron] MonthlyReport send error: %v", err)
		} else {
			log.Println("[cron] Monthly nPro Usage report completed")
		}
	})
}
