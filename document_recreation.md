# Document Snapshot Reconstruction (RAG-Text Implementation)

## Overview
This feature provides a "snapshot" view of the original document without storing PDFs or images. It reconstructs the document's content on-the-fly using the raw text chunks already stored in our vector database.

## The Problem
Our Go-based ingestion pipeline uses a sliding window for chunking:
- **Max Words:** 150
- **Overlap:** 30 words
- **Tokenization:** `strings.Fields` (whitespace-based)
- **The "Why" of Overlap:** Overlap is required for RAG accuracy. It ensures that semantic context is preserved at the boundaries of chunks, preventing sentences or ideas from being cut in half during vector search.

**The Challenge:** If we simply concatenate these chunks in the UI, the user sees repeated sentences every 120 words, creating a poor reading experience.

## The Solution: Backend "Stitch & Slice"
We perform the deduplication logic on the **Backend** before sending data to the client.

### Why Backend?
1. **Bandwidth Efficiency:** Removes ~20% redundant data (the overlap) before it hits the network.
2. **Encapsulation:** The UI remains "dumb." If we change our overlap size (e.g., from 30 to 50 words) in the Go pipeline, we only update the backend logic; the frontend requires no changes.
3. **Consistency:** Ensures the "Auto-Paragraph" formatting is applied identically across Web, Mobile, or API consumers.

### Logic Flow:
1. **Retrieve:** Fetch all chunks from `rag_embeddings` associated with a specific `document_id`, filtered by `chunk_type = 'text'`, and sorted by `chunk_index`.
2. **First Chunk:** Keep as-is.
3. **Subsequent Chunks:** 
   - Check if the current chunk's `chunk_index` is exactly `previous_index + 1`.
   - **If Sequential:** Split the chunk by whitespace, remove the first 30 words (the overlap), and append the remainder.
   - **If Gap exists:** Insert a visual separator (e.g., `\n\n[...]\n\n`) and append the full chunk.
4. **Formatting:** Apply a regex-based "Auto-Paragraph" utility to restore readability lost during whitespace normalization.

## Data Structure (ClickHouse)
The `rag_embeddings` table stores the required metadata:
- `content`: The raw text chunk.
- `document_id`: The unique ID of the source document (`UInt32`).
- `chunk_index`: The position of the chunk (`UInt32`, 0-indexed).
- `chunk_type`: `'text'` or `'image_insight'`.

---

### Part 2: Implementation Prompt

**Role:** Senior Backend Developer (Go/Node.js)
**Task:** Build the "Document Snapshot" reconstruction service.

**Context:**
Chunks were created using `max_words = 150` and `overlap_words = 30`. We need to deliver a clean, non-redundant string to the frontend.

**Requirements:**

1.  **Reconstruction Logic:**
    *   Input: `chunks` array: `{ chunk_index: number, content: string }`.
    *   Sort by `chunk_index`.
    *   For `chunk_index === prev_index + 1`: 
        *   Split `content` by whitespace and remove the first 30 elements. 
        *   Join back with a single space and append to the accumulator.
    *   For gaps in `chunk_index`: 
        *   Append `"\n\n[...]\n\n"` + full current chunk.

2.  **Formatting Utility:**
    *   Implement a regex to inject paragraph breaks: `text.replace(/([.!?])\s+(?=[A-Z])/g, "$1\n\n")`.

3.  **API Response:**
    *   Return a single string of reconstructed text to the frontend.

---

### 3. VERIFY: Final Logic Check
*   **Storage:** 0 additional bytes (uses existing embeddings).
*   **Performance:** $O(N)$ reconstruction speed on the backend.
*   **Reliability:** `chunk_index` handles missing data gracefully with the `[...]` marker.