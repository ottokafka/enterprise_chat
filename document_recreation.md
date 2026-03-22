# Document Snapshot Reconstruction (RAG-Text Implementation)

## Overview
This feature provides a "snapshot" view of the original document without storing PDFs or images. It reconstructs the document's content on-the-fly using the raw text chunks already stored in our vector database.

## The Problem
Our Go-based ingestion pipeline uses a sliding window for chunking:
- **Max Words:** 150
- **Overlap:** 30 words
- **Tokenization:** `strings.Fields` (whitespace-based, removing multi-spaces/newlines)
- **Assembly:** `strings.Join(words, " ")` (single space between words)

If we simply concatenate these chunks in the UI, the user sees repeated sentences every 120 words, creating a poor reading experience.

## The Solution: "Stitch & Slice"
We use a deduplication algorithm on the backend or frontend to "stitch" the chunks back together using their `chunk_index`.

### Logic Flow:
1. **Retrieve:** Fetch all chunks from `rag_embeddings` associated with a specific `document_id`, filtered by `chunk_type = 'text'`, and sorted by `chunk_index`.
2. **First Chunk:** Render as-is.
3. **Subsequent Chunks:** 
   - Check if the current chunk's `chunk_index` is exactly `previous_index + 1`.
   - **If Sequential:** Remove the first 30 words (the overlap) before appending.
   - **If Gap exists:** Insert a visual separator (e.g., `\n\n[...]\n\n`) and render the full chunk (do not slice).
4. **Formatting:** Since `strings.Fields` strips original formatting, we apply a regex-based "Auto-Paragraph" utility to improve readability (e.g., adding double newlines after sentences).

## Data Structure (ClickHouse)
The `rag_embeddings` table stores the required metadata:
- `content`: The raw text chunk.
- `document_id`: The unique ID of the source document (`UInt32`).
- `chunk_index`: The position of the chunk (`UInt32`, 0-indexed).
- `chunk_type`: `'text'` or `'image_insight'` (filter for `'text'` during reconstruction).

---

### Part 2: Implementation Prompt

**Role:** Senior Full-Stack Developer  
**Task:** Build a "Document Snapshot" reconstruction service.

**Context:**
Chunks were created in Go using `max_words = 150` and `overlap_words = 30`. Words are split by `\s+` and joined by a single space. We need to display these as a continuous document in the UI.

**Requirements:**

1.  **Reconstruction Logic:**
    *   Input: `chunks` array: `{ chunk_index: number, content: string }`.
    *   Sort by `chunk_index`.
    *   For the first chunk, keep all text.
    *   For `chunk_index === prev_index + 1`: 
        *   Split `content` by whitespace and remove the first 30 elements. 
        *   Join back with a single space and append.
    *   For gaps in `chunk_index`: 
        *   Append `"\n\n[...]\n\n"` + full current chunk.

2.  **Formatting Utility:**
    *   Implement a function to inject paragraph breaks.
    *   Regex Suggestion: `text.replace(/([.!?])\s+(?=[A-Z])/g, "$1\n\n")`.

3.  **UI Component:**
    *   Display in a scrollable container or modal with `white-space: pre-wrap;`.
    *   Include a "Copy to Clipboard" button.

---

### 3. VERIFY: Final Logic Check
*   **Storage:** 0 additional bytes (uses existing embeddings).
*   **Performance:** $O(N)$ reconstruction speed.
*   **Reliability:** `chunk_index` ensures exact stitching even if the vector search returns partial results.