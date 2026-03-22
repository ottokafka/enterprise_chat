


# 🚀 Phase 2: Advanced RAG Retrieval Pipeline

This repository contains the implementation guide for **Phase 2** of our Retrieval-Augmented Generation (RAG) architecture. 

While Phase 1 focused on ingesting and chunking documents (text & images) into ClickHouse, Phase 2 focuses on **Retrieval**. To prevent hallucinations and ensure high accuracy, we are moving beyond simple vector search and implementing the industry "Gold Standard" retrieval pipeline: **Hybrid Search (Dense + Sparse) with Reciprocal Rank Fusion (RRF) and Cross-Encoder Re-ranking.**

---

## 🏗 Architecture Overview

When a user asks a question, the pipeline executes the following steps:
1. **Metadata Extraction:** Extract structured filters (e.g., specific document names) from the query.
2. **Dense Retrieval (Vector Search):** Uses Qwen3 embeddings to find *semantically* similar chunks in ClickHouse.
3. **Sparse Retrieval (Keyword Search):** Uses ClickHouse token matching to find exact *keyword* matches (crucial for IDs, names, and acronyms).
4. **Reciprocal Rank Fusion (RRF):** A Node.js algorithm that merges Dense and Sparse results, penalizing chunks that only perform well in one category.
5. **Re-ranking:** Sends the top fused chunks to a Cross-Encoder model to score their actual logical relevance to the prompt.

---

## 📋 Prerequisites

*   **Node.js** (v18+)
*   **ClickHouse** database running with the `rag_embeddings` table from Phase 1.
*   **Embedding API** running `Qwen3-Embedding-8B-GGUF`.
*   **Re-ranker API** running Qwen3-Reranker-8B-Q4_K_M.gguf

Install required dependencies:
```bash
npm install axios @clickhouse/client
```

---

## 💻 Implementation Guide

### Step 1: ClickHouse Query Setup
ClickHouse is powerful enough to handle both our Dense (Vector) and Sparse (Keyword) searches. We will perform two separate queries in parallel and fuse them in Node.js.

### Step 2: The Node.js Retrieval Service (`retrieval.js`)

Create a new file `retrieval.js`. This module handles the entire Phase 2 pipeline.

```javascript
const axios = require('axios');
const { createClient } = require('@clickhouse/client');

// --- CONFIGURATION ---
const EMBEDDING_API_URL = "https://alice.forest-interactive.com/v1/embeddings";
const EMBEDDING_API_KEY = "5e59cd1883bdcb8d1afeff7fbc74bfd8f32111c2d3853398a3afb25cf4423376";
const RERANKER_API_URL = "http://192.168.1.235:8082/v1/rerank"; 

const clickhouse = createClient({
    url: 'http://localhost:8123',
    username: 'default',
    password: '',
    database: 'default'
});

// --- 1. GENERATE EMBEDDING FOR USER QUERY ---
async function getEmbedding(text) {
    const response = await axios.post(
        EMBEDDING_API_URL,
        { model: "Qwen3-Embedding-8B-GGUF", input: text },
        { headers: { "Content-Type": "application/json", "Authorization": `Bearer ${EMBEDDING_API_KEY}` } }
    );
    return response.data.data[0].embedding;
}

// --- 2. CLICKHOUSE HYBRID SEARCH ---
async function fetchFromClickHouse(userQuery, queryEmbedding, metadataFilter = null) {
    // A. Dense Search (Vector Similarity)
    // cosineDistance: lower is better (0 is perfect match)
    let denseQuery = `
        SELECT id, document_name, chunk_type, content, 
               cosineDistance(embedding,[${queryEmbedding.join(',')}]) as distance
        FROM rag_embeddings
    `;
    if (metadataFilter) denseQuery += ` WHERE document_name = '${metadataFilter}'`;
    denseQuery += ` ORDER BY distance ASC LIMIT 20`;

    // B. Sparse Search (Keyword Exact Match)
    // Extract key terms from user query for basic text search
    const keywords = userQuery.replace(/[^a-zA-Z0-9 ]/g, '').split(' ').filter(w => w.length > 3);
    const keywordConditions = keywords.map(kw => `hasTokenCaseInsensitive(content, '${kw}')`).join(' OR ');
    
    let sparseQuery = `
        SELECT id, document_name, chunk_type, content
        FROM rag_embeddings
        WHERE (${keywordConditions || '1=1'})
    `;
    if (metadataFilter) sparseQuery += ` AND document_name = '${metadataFilter}'`;
    sparseQuery += ` LIMIT 20`;

    // Execute both concurrently for high performance
    const [denseRes, sparseRes] = await Promise.all([
        clickhouse.query({ query: denseQuery, format: 'JSONEachRow' }).then(r => r.json()),
        clickhouse.query({ query: sparseQuery, format: 'JSONEachRow' }).then(r => r.json())
    ]);

    return { denseResults: denseRes, sparseResults: sparseRes };
}

// --- 3. RECIPROCAL RANK FUSION (RRF) ---
// Merges Dense and Sparse results. RRF Score = 1 / (k + rank)
function performRRF(denseResults, sparseResults, k = 60) {
    const rrfScores = new Map();
    const chunksData = new Map();

    // Process Dense Ranks
    denseResults.forEach((row, index) => {
        const rank = index + 1;
        const score = 1 / (k + rank);
        rrfScores.set(row.id, score);
        chunksData.set(row.id, row);
    });

    // Process Sparse Ranks
    sparseResults.forEach((row, index) => {
        const rank = index + 1;
        const currentScore = rrfScores.get(row.id) || 0;
        const score = 1 / (k + rank);
        rrfScores.set(row.id, currentScore + score);
        chunksData.set(row.id, row);
    });

    // Sort by combined RRF score descending
    const fusedResults = Array.from(rrfScores.entries())
        .map(([id, score]) => ({ score, data: chunksData.get(id) }))
        .sort((a, b) => b.score - a.score);

    // Return the top 15 fused chunks
    return fusedResults.slice(0, 15).map(res => res.data);
}

// --- 4. CROSS-ENCODER RE-RANKING ---
// Sends the top 15 chunks to a reranker model to pick the absolute best 5
async function rerankChunks(userQuery, fusedChunks) {
    try {
        const documents = fusedChunks.map(chunk => chunk.content);
        
        const response = await axios.post(RERANKER_API_URL, {
            model: "bge-reranker-base", // Change to your active reranker model
            query: userQuery,
            documents: documents,
            top_n: 5
        }, { headers: { "Content-Type": "application/json" } });

        // Map the results back to our chunk objects
        const bestChunks = response.data.results.map(res => {
            const originalChunk = fusedChunks[res.index];
            return {
                ...originalChunk,
                relevance_score: res.relevance_score
            };
        });

        return bestChunks;
    } catch (error) {
        console.warn("Re-ranker unavailable. Falling back to base RRF ranking.", error.message);
        return fusedChunks.slice(0, 5);
    }
}

// --- 5. MAIN RETRIEVAL EXECUTION ---
async function runRetrievalPipeline(userQuery, targetDocument = null) {
    console.log(`🔍 Query: "${userQuery}"`);
    
    // 1. Embed Query
    console.log("   -> Generating embedding...");
    const queryEmbedding = await getEmbedding(userQuery);

    // 2. Fetch from DB (Hybrid)
    console.log("   -> Running ClickHouse Hybrid Search...");
    const { denseResults, sparseResults } = await fetchFromClickHouse(userQuery, queryEmbedding, targetDocument);
    
    // 3. Apply RRF
    console.log(`   -> Fusing results (Dense: ${denseResults.length}, Sparse: ${sparseResults.length})...`);
    const fusedChunks = performRRF(denseResults, sparseResults);

    // 4. Re-rank
    console.log("   -> Re-ranking top candidates...");
    const finalChunks = await rerankChunks(userQuery, fusedChunks);

    console.log("\n✅ Top 5 Context Chunks Retrieved:");
    finalChunks.forEach((chunk, i) => {
        console.log(`\n[${i + 1}] Source: ${chunk.document_name} | Type: ${chunk.chunk_type}`);
        console.log(`Content: ${chunk.content.substring(0, 100)}...`);
    });

    return finalChunks;
}

// Execute test
runRetrievalPipeline("What caused the revenue growth in Q3?")
    .catch(console.error);
```

---

## 🌟 Best Practices Implemented

1. **Reciprocal Rank Fusion (RRF):** Vector embeddings (`cosineDistance`) struggle with specific IDs or names (e.g., "Product XZ-99"). By mapping `hasTokenCaseInsensitive` (Sparse) alongside the Vector search (Dense) and using RRF, we guarantee that exact string matches are not lost in the mathematical vector space.
2. **Post-Retrieval Re-ranking:** Vector databases retrieve *similar* text, not necessarily *relevant* answers. By pulling 15-20 candidates from ClickHouse and passing them through a Cross-Encoder (which compares the user query and the chunk context simultaneously), we drastically reduce hallucinations.
3. **Metadata Filtering (Self-Querying prep):** Notice the `targetDocument` filter parameter. You can use an LLM pre-step to analyze the user's prompt and extract constraints (e.g., User: *"In the 2023 report, what was the revenue?"* -> Extract Metadata: `targetDocument: 'employee_handbook.docx'`). Applying this directly in the ClickHouse SQL `WHERE` clause drastically improves accuracy and speeds up search times. 
4. **Concurrent DB Queries:** `Promise.all` is used to fire the Vector search and Keyword search simultaneously to ClickHouse, ensuring zero latency penalty for implementing Hybrid Search.