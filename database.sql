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
    shared_with_user_ids Array(UInt32) DEFAULT[],
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

-- 6. System Prompts Table
CREATE TABLE IF NOT EXISTS default.system_prompts
(
    id UInt32,
    user_id UInt32,
    name String,
    content String,
    created_at Date DEFAULT toDate(now())
) ENGINE = MergeTree()
ORDER BY (user_id, id);