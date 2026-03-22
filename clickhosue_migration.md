



```go
func usageLogReport(days int) ([]UsageRow, error) {
	
 	query := fmt.Sprintf(`
SELECT
    u.name,
    u.job_title,
    COALESCE(ul.chat_count, 0) AS chat_count,
    COALESCE(ul.image_count, 0) AS image_count,
    formatDateTime(u.created_at, '%%d-%%m-%%Y %%I:%%M %%p') AS last_used
FROM users  AS u
LEFT JOIN (
    SELECT 
        user_id,
        countIf(ai_usage_type = 'chat') AS chat_count,
        countIf(ai_usage_type = 'image') AS image_count
    FROM usage_log
    WHERE created_at >= now() - INTERVAL %d DAY
    GROUP BY user_id
) AS ul ON u.id = ul.user_id
ORDER BY u.created_at DESC;
`, days)

	rows, err := ClickhouseQuery(query)
	if err != nil {
		return nil, fmt.Errorf("usageLogReport query failed: %w", err)
	}
	defer rows.Close()

	var result
}

func insertApiChat(userId int, usageType string) error {
	// async_insert=1 offloads the batching to ClickHouse natively.
	// wait_for_async_insert=0 returns OK immediately to the API.
	query := `INSERT INTO usage_log (user_id, ai_usage_type) 
	          SETTINGS async_insert=1, wait_for_async_insert=0 
	          VALUES (?, ?)`
	
	_, err := ClickhouseExec(query, userId, usageType)
	return err
}
```
```sql
CREATE TABLE IF NOT EXISTS default.users
(
    id UInt32,
    name String,
    email String,
    job_title String,
    office_location Nullable(String),
    created_at Date DEFAULT toDate(now())
) ENGINE = MergeTree()
ORDER BY id;

-- 2. API Key Table
CREATE TABLE IF NOT EXISTS default.api_key
(
    id UInt32,
    user_id UInt32,
    created_at Date DEFAULT toDate(now()),
    key String
) ENGINE = MergeTree()
ORDER BY id;

-- 3. User Documents Table
CREATE TABLE IF NOT EXISTS default.user_documents 
(
    id UInt32,
    user_id UInt32,
    document_name String,
    status String DEFAULT 'processing',
    is_global Bool DEFAULT false,
    created_at Date DEFAULT toDate(now())
) ENGINE = MergeTree()
ORDER BY id;

-- 4. Usage Log Table 
-- Partitioned by Month to make dropping old data easy.
CREATE TABLE IF NOT EXISTS default.usage_log
(
    user_id UInt32,
    ai_usage_type Enum8(
        'chat' = 1, 
        'image' = 2, 
        'video' = 3, 
        'edit_image' = 4
    ),
    created_at Date DEFAULT toDate(now())
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(created_at)
ORDER BY (user_id, created_at);

-- 5. RAG Embeddings Table (High Volume)
CREATE TABLE IF NOT EXISTS default.rag_embeddings 
(
    id UUID DEFAULT generateUUIDv4(),
    user_id UInt32,                
    document_id UInt32,            
    document_name String,
    chunk_type Enum8('text' = 1, 'image_insight' = 2),
    content String, 
    chunk_index UInt32,
    embedding Array(Float32),
    created_at Date DEFAULT toDate(now())
) ENGINE = MergeTree()
ORDER BY (user_id, document_id, id);
```