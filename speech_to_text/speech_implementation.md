Here is a concise summary of the backend implementation for your README:

### Backend Overview
The backend is a real-time, Python-based WebSocket server designed for near-instant voice dictation. It receives raw PCM Float32 audio streams from the frontend, detects when the user is speaking, and processes the speech into text locally on an NVIDIA GPU.


# Install
- run the install_deps.sh
- run download_speech_models.sh to download models
- python server.py

running at localhost:8084

### Tech Stack & Architecture Choices

* **WebSocket Server (`websockets`)**: 
  * *Why:* Provides a persistent, low-latency, two-way connection essential for streaming raw audio continuously without the overhead of HTTP requests.
* **Speech-to-Text Model (Distil-Whisper Large v3.5)**: 
  * *Why:* It offers the high accuracy of Whisper Large but is distilled to be significantly faster and consume less VRAM. This is critical for achieving the "near-instant" feedback required for a dictation feature.
* **Voice Activity Detection (Silero VAD)**: 
  * *Why:* An extremely lightweight and accurate PyTorch model used to filter out background noise and detect natural pauses. It tells the backend exactly when a sentence ends, saving GPU resources by only transcribing actual speech.
* **Local Caching & Execution (PyTorch + RTX 3060)**:
  * *Why:* Running models locally on CUDA 13.0 guarantees zero API latency, zero recurring costs, and complete user privacy. Pre-downloading models locally ensures the server runs offline and consistently.

### Core Processing Logic
1. **Audio Ingestion**: Audio is received in small chunks (~4096 samples) from the browser.
2. **VAD Slicing**: Chunks are safely sliced into exact 512-sample segments (a strict requirement for Silero VAD at 16kHz) to check for speech probability.
3. **Smart Triggering**: Transcription runs automatically under two conditions:
   * **Pause Detected**: The user stops speaking for ~1 second (determined by `PAUSE_THRESHOLD`).
   * **Time Limit Reached**: The user speaks continuously for 15 seconds. This prevents the pipeline from crashing on >30s audio limits and guarantees the user sees text appearing dynamically instead of waiting until they finish a long paragraph. 
4. **Graceful Handling**: Uses `chunk_length_s=30` and timestamping to prevent PyTorch crashes on edge-case long audio, and actively clears CUDA VRAM on client disconnects to prevent memory leaks.