

# 🚀 Phase 3: Augmentation, Generation & Post-Processing

This repository contains the implementation guide for **Phase 3**, the final leg of our Retrieval-Augmented Generation (RAG) architecture. 

In Phase 2, we retrieved the top 5 most relevant chunks from ClickHouse using Hybrid Search and Re-ranking. In this final phase, we assemble those chunks into a strict prompt, generate the answer using our `qwen3.5` LLM, and apply post-processing guardrails to guarantee accurate citations.

## 🏗 Architecture Overview

1.  **Context Assembly (Augmentation):** Formats the top 5 chunks into a single structured string with explicit source IDs (e.g., `[Source 1]`, `[Source 2]`).
2.  **Generation:** Passes the strictly formulated prompt to the LLM using the OpenAI SDK format pointing to your local/private GPU endpoint.
3.  **Citation Mapping & Guardrails (Post-Processing):** Scans the final generated text for `[Source X]` tags and appends a structured metadata object containing the actual document names and ClickHouse chunk details.

---

## 💻 Implementation Guide

### The Node.js Express Controller (`rag_controller.js`)

Below is the complete controller that takes a user query, assumes you have the `retrievedChunks` from Phase 2, and handles the Phase 3 pipeline.

```javascript
const { OpenAI } = require('openai');
require('dotenv').config();

// Initialize OpenAI SDK pointing to your custom vLLM / Qwen server
const openai = new OpenAI({ 
    baseURL: process.env.MAIN_GPU_URL || "http://192.168.1.235:8000/v1",
});

// --- 1. CONTEXT ASSEMBLY ---
function assembleContext(chunks) {
    let contextString = "### Context Documents:\n";
    chunks.forEach((chunk, index) => {
        // We use index + 1 so sources start at[Source 1]
        contextString += `\n[Source ${index + 1}] (Document: ${chunk.document_name}, Type: ${chunk.chunk_type})\n`;
        contextString += `${chunk.content}\n`;
    });
    return contextString;
}

// --- 2. CITATION MAPPING & POST-PROCESSING ---
function postProcessOutput(llmResponse, chunks) {
    const citationRegex = /\[Source (\d+)\]/g;
    const usedSources = new Set();
    let match;

    // Scan the text for [Source X] markers
    while ((match = citationRegex.exec(llmResponse)) !== null) {
        const sourceIndex = parseInt(match[1], 10) - 1; // Convert back to 0-based array index
        if (chunks[sourceIndex]) {
            usedSources.add(sourceIndex);
        }
    }

    // Map verified sources to their metadata
    const metadata = Array.from(usedSources).map(index => ({
        citation_tag: `[Source ${index + 1}]`,
        document_name: chunks[index].document_name,
        chunk_type: chunks[index].chunk_type,
        preview: chunks[index].content.substring(0, 80) + "..."
    }));

    return {
        answer: llmResponse,
        citations: metadata
    };
}

// --- 3. THE FINAL ENDPOINT ---
exports.generate_text = async (req, res) => {
    // In a real flow, you would call Phase 2 here to get `retrievedChunks` based on `prompt`
    let { prompt, retrievedChunks, max_tokens } = req.body;

    try {
        // 1. Context Assembly (Augmentation)
        const contextStr = assembleContext(retrievedChunks);

        // Define the strict system prompt with Fallback Grounding
        const systemPrompt = `
You are an expert analytical assistant. You will be provided with a user question and several Context Documents. 
Answer the user's question USING ONLY the information provided in the Context Documents. 

RULES:
1. If the answer is not contained in the context, explicitly say: 'I do not have enough information to answer this based on the provided documents.'
2. When you use information from a context document, you MUST append its source tag exactly as provided (e.g., [Source 1], [Source 2]) at the end of the relevant sentence.
3. Do not invent URLs or markdown links. Only use the literal text '[Source X]'.
        `;

        const fullPrompt = `${systemPrompt}\n\n${contextStr}\n\nUser Question: ${prompt}\nAnswer:`;

        // 2. Generation
        const response = await openai.completions.create({
            model: "qwen3.5",
            prompt: fullPrompt,
            max_tokens: max_tokens || 500,
            temperature: 0.1 // Low temperature prevents hallucination in RAG
        });

        const rawText = response.choices[0].text.trim();

        // 3. Post-Processing
        const finalPayload = postProcessOutput(rawText, retrievedChunks);

        return res.json({
            success: true,
            data: finalPayload
        });

    } catch (error) {
        console.error("Generate text error:", error);
        return res.status(500).json({
            success: false,
            error: error.message || "Failed to generate text"
        });
    }
};
```

---

## 🌟 Key Features & Best Practices

*   **Separation of Text and Citations:** Instead of forcing the LLM to output ugly URLs or JSON directly in its response (which often breaks formatting or triggers JSON schema errors), we instruct the LLM to output simple markers like `[Source 1]`. The `postProcessOutput` function securely extracts these and attaches a clean JSON payload of actual document links. Your frontend UI can then parse `[Source 1]` and turn it into a beautiful clickable tooltip, hyperlinked document title, or side-panel reference.
*   **Fallback Grounding:** The instruction `"If the answer is not contained... explicitly say: 'I do not have enough information'"` is the most critical guardrail in RAG. Without it, the LLM will inevitably draw upon its pre-training data to answer the question, entirely bypassing your ClickHouse database, leading to confident hallucinations. 
*   **Low Temperature for Generation:** Notice the `temperature: 0.1` setting in the OpenAI API call. While standard chatbots use `0.7` for creativity, RAG requires analytical precision. A near-zero temperature forces the model to act predictably and stick strictly to the context window provided.

***

### 📊 Meta-Cognitive Outputs
*   **Confidence Level:** 0.98/1.0
*   **Key Caveats:** 
    *   The standard `openai.completions.create` (Legacy Completions API) is used based on your code snippet. If your `qwen3.5` deployment uses the newer Chat Completions API, you will need to change `openai.completions.create` to `openai.chat.completions.create` and pass an array of `messages` instead of a raw `prompt` string.
    *   The Regex used for post-processing (`\[Source (\d+)\]`) strictly looks for brackets. Ensure your LLM temperature is kept low (`0.1`) so it doesn't try to creatively alter the citation formatting (e.g., outputting `Source: 1` instead of `[Source 1]`).