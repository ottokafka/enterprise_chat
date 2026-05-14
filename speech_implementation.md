
This is our working demo for speech to text 

- goal implement speech to text feature in our chat app for the text input box. 
- use a microphone icon to start and stop the dictation service. this will connect or disconnect from the websocket server.


# frontend

```js
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Voice Dictation Demo</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; padding: 40px; max-width: 800px; margin: 0 auto; }
        textarea { width: 100%; height: 200px; font-size: 18px; padding: 10px; margin-top: 10px; }
        button { padding: 10px 20px; font-size: 16px; cursor: pointer; background: #007aff; color: white; border: none; border-radius: 5px; }
        button.recording { background: #ff3b30; }
        #status { margin-top: 10px; color: #666; }
    </style>
</head>
<body>

    <h2>Seamless Voice Dictation</h2>
    <p>Press <strong>Start Dictation</strong> (or simulate a hotkey) to begin speaking. The text will appear automatically upon pauses.</p>
    
    <button id="recordBtn">Start Dictation</button>
    <div id="status">Status: Disconnected</div>
    <textarea id="textOutput" placeholder="Your text will appear here..."></textarea>

    <script>
        // Note: For Cloudflare Tunnels, wss:// is required for WebSockets over HTTPS
        const WS_URL = "wss://speech_to_text.npro.ai"; 
        
        let websocket;
        let audioContext;
        let scriptProcessor;
        let mediaStream;
        let isRecording = false;

        const recordBtn = document.getElementById("recordBtn");
        const textOutput = document.getElementById("textOutput");
        const statusDiv = document.getElementById("status");

        recordBtn.addEventListener("click", toggleRecording);

        function connectWebSocket() {
            websocket = new WebSocket(WS_URL);
            websocket.onopen = () => { statusDiv.innerText = "Status: Connected to Server"; };
            websocket.onmessage = (event) => {
                // Append received text when the backend finalizes a sentence
                const text = event.data;
                textOutput.value += (textOutput.value.length > 0 ? " " : "") + text;
                textOutput.scrollTop = textOutput.scrollHeight; // Auto-scroll
            };
            websocket.onclose = () => { statusDiv.innerText = "Status: Disconnected"; };
            websocket.onerror = (e) => { console.error("WebSocket Error:", e); };
        }

        async function toggleRecording() {
            if (isRecording) {
                stopRecording();
            } else {
                await startRecording();
            }
        }

        async function startRecording() {
            if (!websocket || websocket.readyState !== WebSocket.OPEN) connectWebSocket();

            try {
                mediaStream = await navigator.mediaDevices.getUserMedia({ audio: true });
                
                // Set explicitly to 16kHz for Whisper / Silero
                audioContext = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 16000 });
                const source = audioContext.createMediaStreamSource(mediaStream);
                
                // Chunk size of 4096 is a good balance for streaming real-time
                scriptProcessor = audioContext.createScriptProcessor(4096, 1, 1);
                
                source.connect(scriptProcessor);
                scriptProcessor.connect(audioContext.destination);

                scriptProcessor.onaudioprocess = (e) => {
                    if (websocket && websocket.readyState === WebSocket.OPEN) {
                        const audioData = e.inputBuffer.getChannelData(0); // Float32Array
                        websocket.send(audioData);
                    }
                };

                isRecording = true;
                recordBtn.innerText = "Stop Dictation";
                recordBtn.classList.add("recording");
            } catch (err) {
                console.error("Error accessing microphone:", err);
                alert("Microphone access denied or not available.");
            }
        }

        function stopRecording() {
            if (scriptProcessor) scriptProcessor.disconnect();
            if (mediaStream) mediaStream.getTracks().forEach(track => track.stop());
            if (audioContext) audioContext.close();
            if (websocket) websocket.close();

            isRecording = false;
            recordBtn.innerText = "Start Dictation";
            recordBtn.classList.remove("recording");
        }
        
        // Auto-connect socket on load
        connectWebSocket();
    </script>
</body>
</html>
```

```py
import os
import asyncio
import websockets
import numpy as np
import warnings

# 1. Point PyTorch to the local offline cache BEFORE importing torch
os.environ["TORCH_HOME"] = "/home/speech/steaming_speech/torch_hub"

import torch
from transformers import pipeline

# Suppress warnings for cleaner logs
warnings.filterwarnings("ignore")

# Define Server Port and Paths
PORT = 8084
WORKSPACE = "/home/speech/steaming_speech"
WHISPER_LOCAL_DIR = f"{WORKSPACE}/distil-large-v3.5"

# Device Configuration (Using your RTX 3060)
device = "cuda:0" if torch.cuda.is_available() else "cpu"
print(f"Using device: {device}")

# 2. Load Distil-Whisper from local directory
print(f"Loading Distil-Whisper model from {WHISPER_LOCAL_DIR}...")
asr_pipeline = pipeline(
    "automatic-speech-recognition",
    model=WHISPER_LOCAL_DIR,
    device=device,
    torch_dtype=torch.float16
)
print("Distil-Whisper loaded successfully.")

# 3. Load Silero VAD from local cache
print("Loading Silero VAD model from local cache...")
vad_model, utils = torch.hub.load(
    repo_or_dir='snakers4/silero-vad',
    model='silero_vad',
    force_reload=False,  # Ensures it uses the downloaded cache
    trust_repo=True
)
vad_model = vad_model.to(device)
print("Silero VAD loaded successfully.")

# Constants for Audio
SAMPLE_RATE = 16000
# The frontend scriptProcessor sends 4096 samples per chunk.
# Silero VAD strictly prefers chunks of 512, 1024, or 1536 samples.
VAD_CHUNK_SIZE = 512 
# Adjust this to change how long the user needs to pause before text appears
PAUSE_THRESHOLD = 4  # 4 chunks of silence ≈ 1 second of pausing

async def handle_audio_stream(websocket):
    print(f"\n[+] Client connected from {websocket.remote_address}")
    audio_buffer = []
    silence_counter = 0
    has_spoken = False

    try:
        async for message in websocket:
            # Decode raw Float32 buffer from the browser
            audio_chunk = np.frombuffer(message, dtype=np.float32)
            audio_buffer.append(audio_chunk)

            # Evaluate VAD by iterating over the chunk in 512-sample steps
            speech_detected_in_chunk = False
            
            for i in range(0, len(audio_chunk), VAD_CHUNK_SIZE):
                slice_512 = audio_chunk[i : i + VAD_CHUNK_SIZE]
                
                # Ensure we only pass exactly 512 samples to Silero
                if len(slice_512) == VAD_CHUNK_SIZE:
                    tensor_chunk = torch.from_numpy(slice_512).to(device)
                    speech_prob = vad_model(tensor_chunk, SAMPLE_RATE).item()
                    
                    if speech_prob > 0.5:
                        speech_detected_in_chunk = True
                        break  # We found speech, no need to check the rest of this websocket message

            # Update our pause detection logic based on the whole message
            if speech_detected_in_chunk:
                has_spoken = True
                silence_counter = 0
            else:
                if has_spoken:
                    silence_counter += 1

            # Detect pause to finalize the sentence
            if has_spoken and silence_counter >= PAUSE_THRESHOLD:
                # Merge accumulated audio chunks
                full_audio = np.concatenate(audio_buffer)
                
                # Only transcribe if we have a reasonable amount of audio (>0.5 seconds)
                if len(full_audio) > SAMPLE_RATE * 0.5:
                    print("Pause detected. Transcribing audio buffer...")
                    
                    # Run Whisper Inference on RTX 3060
                    result = asr_pipeline({"sampling_rate": SAMPLE_RATE, "raw": full_audio})
                    transcribed_text = result["text"].strip()
                    
                    if transcribed_text:
                        print(f"Recognized: {transcribed_text}")
                        # Send text back to frontend
                        await websocket.send(transcribed_text)
                
                # Reset buffers to capture the next sentence
                audio_buffer = []
                silence_counter = 0
                has_spoken = False

    except websockets.exceptions.ConnectionClosedOK:
        print(f"[-] Client {websocket.remote_address} disconnected normally.")
    except websockets.exceptions.ConnectionClosedError:
        print(f"[!] Client {websocket.remote_address} connection closed unexpectedly.")
    except Exception as e:
        print(f"[x] Error processing audio stream: {e}")
    finally:
        # Cleanup memory when a connection closes
        audio_buffer = []
        torch.cuda.empty_cache()
        
async def main():
    print(f"\nStarting WebSocket server on ws://localhost:{PORT}...")
    # Serve the websocket connections
    async with websockets.serve(handle_audio_stream, "0.0.0.0", PORT):
        await asyncio.Future()  # Run forever

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        print("\nServer gracefully shut down.")
```