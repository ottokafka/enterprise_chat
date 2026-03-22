


To build a robust data ingestion and processing pipeline for your Retrieval-Augmented Generation (RAG) architecture in Node.js, we need to handle text extraction, text chunking with overlap, image extraction, vision model processing, embedding generation, and finally, storing it all in ClickHouse.

Here is the complete architectural guide and Node.js code to accomplish this.

### 1. Best Libraries for DOCX Extraction in Node.js
A `.docx` file is under the hood just a zipped archive of XML files and media assets.
*   **For Text Extraction:** **`mammoth`**. It is the gold standard for extracting raw, clean semantic text from Word documents without needing Microsoft Office installed. 
*   **For Image Extraction:** **`adm-zip`**. Because `.docx` is a zip file, all embedded images are stored in a folder called `word/media/` inside the file. Unzipping it via `adm-zip` is the fastest and most reliable way to extract raw image buffers without relying on complex XML parsing or expensive cloud APIs.

### 2. Processing the Images with your Vision Model (vLLM)
Once the images are extracted via `adm-zip` as buffers, we convert them to **Base64** strings. Because vLLM generally exposes an OpenAI-compatible REST API, you can pass the Base64 string directly into the vLLM vision model to generate an image "insight" or "description". 
Once the vision model describes the image (e.g., *"A bar chart showing 40% revenue growth in Q3"*), we embed that text description using your `Qwen3-Embedding-8B-GGUF` API and store it in ClickHouse. This makes your RAG capable of answering questions based on the images inside the document.

### 3. ClickHouse Database Schema
ClickHouse is excellent for fast vector distance calculations. You'll want to store both text chunks and image insights in the same table so semantic search retrieves both natively.

Database connction string
CLICKHOUSE_CONN_STR="clickhouse://admin:admin@192.168.0.171:9000"


```sql
CREATE TABLE default.rag_embeddings (
    id UUID DEFAULT generateUUIDv4(),
    document_name String,
    chunk_type Enum8('text' = 1, 'image_insight' = 2),
    content String, 
    chunk_index UInt32,
    embedding Array(Float32) -- The Qwen3 8B embeddings array
) ENGINE = MergeTree()
ORDER BY id;
```

---

### 4. Complete Node.js Pipeline Code
Below is the full implementation. 

**Setup:**
```bash
npm install mammoth adm-zip axios @clickhouse/client
```

**Code (`pipeline.js`):**
```javascript
const mammoth = require('mammoth');
const AdmZip = require('adm-zip');
const axios = require('axios');
const { createClient } = require('@clickhouse/client');
const fs = require('fs');

// --- 1. CONFIGURATION ---
const EMBEDDING_API_URL = "https://alice.forest-interactive.com/v1/embeddings";
const API_KEY = "5e59cd1883bdcb8d1afeff7fbc74bfd8f32111c2d3853398a3afb25cf4423376";
const VLLM_VISION_API_URL = "https://alice.forest-interactive.com/v1/chat/completions"; 
const CLICKHOUSE_CONN_STR="clickhouse://admin:admin@192.168.0.171:9000"


const clickhouse = createClient({
    url: CLICKHOUSE_CONN_STR,
    username: 'default',
    password: '',
    database: 'default'
});

// --- 2. TEXT CHUNKING WITH OVERLAP ---
// Standard RAG Best Practice to solve the "Boundary Problem"
function chunkTextWithOverlap(text, maxWords = 150, overlapWords = 30) {
    const words = text.split(/\s+/);
    const chunks =[];
    let i = 0;
    
    while (i < words.length) {
        const chunk = words.slice(i, i + maxWords).join(' ');
        chunks.push(chunk);
        i += (maxWords - overlapWords);
    }
    return chunks;
}

// --- 3. GET EMBEDDINGS ---
async function getEmbedding(text) {
    try {
        const response = await axios.post(
            EMBEDDING_API_URL,
            {
                model: "Qwen3-Embedding-8B-GGUF",
                input: text
            },
            {
                headers: {
                    "Content-Type": "application/json",
                    "Authorization": `Bearer ${API_KEY}`
                }
            }
        );
        return response.data.data[0].embedding;
    } catch (error) {
        console.error("Error generating embedding:", error.message);
        throw error;
    }
}

// --- 4. PROCESS IMAGES WITH VLLM VISION ---
async function getVisionInsight(base64Image, extension) {
    try {
        const mimeType = extension === 'png' ? 'image/png' : 'image/jpeg';
        const response = await axios.post(VLLM_VISION_API_URL, {
            model: "your-vllm-vision-model-name", // Change to your vLLM model name
            messages: [
                {
                    role: "user",
                    content:[
                        { type: "text", text: "Describe this image in detail, capturing any data, charts, or concepts so it can be indexed in a text search database." },
                        { type: "image_url", image_url: { url: `data:${mimeType};base64,${base64Image}` } }
                    ]
                }
            ]
        });
        return response.data.choices[0].message.content;
    } catch (error) {
        console.error("Error getting vision insight:", error.message);
        return null;
    }
}

// --- 5. MAIN INGESTION PIPELINE ---
async function processDocxPipeline(filePath) {
    console.log(`Starting ingestion for: ${filePath}`);
    const docName = filePath.split('/').pop();
    const recordsToInsert =[];

    // A. EXTRACT AND PROCESS TEXT
    console.log("Extracting text...");
    const textResult = await mammoth.extractRawText({ path: filePath });
    const rawText = textResult.value;
    
    const chunks = chunkTextWithOverlap(rawText, 150, 30);
    console.log(`Created ${chunks.length} text chunks with overlap.`);

    for (let i = 0; i < chunks.length; i++) {
        const embedding = await getEmbedding(chunks[i]);
        recordsToInsert.push({
            document_name: docName,
            chunk_type: 'text',
            content: chunks[i],
            chunk_index: i,
            embedding: embedding
        });
    }

    // B. EXTRACT AND PROCESS IMAGES (Using adm-zip)
    console.log("Extracting images...");
    const zip = new AdmZip(filePath);
    const zipEntries = zip.getEntries();
    let imageIndex = 0;

    for (const zipEntry of zipEntries) {
        // DOCX stores all images in the "word/media/" directory
        if (zipEntry.entryName.startsWith("word/media/") && !zipEntry.isDirectory) {
            const extension = zipEntry.name.split('.').pop().toLowerCase();
            if (['png', 'jpg', 'jpeg'].includes(extension)) {
                
                const buffer = zipEntry.getData();
                const base64Image = buffer.toString('base64');
                
                console.log(`Processing image: ${zipEntry.name}`);
                const insightText = await getVisionInsight(base64Image, extension);
                
                if (insightText) {
                    const insightEmbedding = await getEmbedding(insightText);
                    recordsToInsert.push({
                        document_name: docName,
                        chunk_type: 'image_insight',
                        content: `[Image Insight]: ${insightText}`,
                        chunk_index: imageIndex,
                        embedding: insightEmbedding
                    });
                }
                imageIndex++;
            }
        }
    }

    // C. SAVE TO CLICKHOUSE
    if (recordsToInsert.length > 0) {
        console.log(`Inserting ${recordsToInsert.length} total rows into ClickHouse...`);
        await clickhouse.insert({
            table: 'rag_embeddings',
            values: recordsToInsert,
            format: 'JSONEachRow'
        });
        console.log("Ingestion Pipeline Complete!");
    }
}

// Execute the pipeline
processDocxPipeline('./employee_handbook.docx').catch(console.error);
```

### 5. How to Search ClickHouse (During Chatbot Retrieval)
When the user asks *"What caused the revenue growth in Q3?"*, you embed that prompt using the same `Qwen3-Embedding-8B-GGUF` API. Then, you run this query in ClickHouse utilizing the highly efficient `cosineDistance` array function:

```sql
SELECT 
    document_name, 
    chunk_type, 
    content, 
    cosineDistance(embedding,[/* USER QUERY EMBEDDING ARRAY */]) as distance
FROM default.rag_embeddings
ORDER BY distance ASC
LIMIT 5;
```
Because we embedded the image insights right alongside the textual data, if a bar chart in the document contained the answer to the user's question, ClickHouse will naturally surface the image's "insight text" alongside the standard overlapping text chunks.