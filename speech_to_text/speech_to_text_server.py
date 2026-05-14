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
    torch_dtype=torch.float16,
    chunk_length_s=30  # <-- ADD THIS: Enables safe chunking for audio > 30s
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
MAX_AUDIO_LENGTH = 15 * SAMPLE_RATE # <-- Force transcribe after 15 seconds of continuous speech

async def handle_audio_stream(websocket):
    print(f"\n[+] Client connected from {websocket.remote_address}")
    audio_buffer = []
    silence_counter = 0
    has_spoken = False

    try:
        async for message in websocket:
            audio_chunk = np.frombuffer(message, dtype=np.float32)
            audio_buffer.append(audio_chunk)

            speech_detected_in_chunk = False
            for i in range(0, len(audio_chunk), VAD_CHUNK_SIZE):
                slice_512 = audio_chunk[i : i + VAD_CHUNK_SIZE]
                if len(slice_512) == VAD_CHUNK_SIZE:
                    tensor_chunk = torch.from_numpy(slice_512).to(device)
                    speech_prob = vad_model(tensor_chunk, SAMPLE_RATE).item()
                    
                    if speech_prob > 0.5:
                        speech_detected_in_chunk = True
                        break

            if speech_detected_in_chunk:
                has_spoken = True
                silence_counter = 0
            else:
                if has_spoken:
                    silence_counter += 1

            # Check total accumulated audio length
            current_buffer_length = sum(len(chunk) for chunk in audio_buffer)
            is_over_time_limit = current_buffer_length >= MAX_AUDIO_LENGTH

            # Detect pause OR hit the maximum time limit to finalize the sentence
            if has_spoken and (silence_counter >= PAUSE_THRESHOLD or is_over_time_limit):
                full_audio = np.concatenate(audio_buffer)
                
                if len(full_audio) > SAMPLE_RATE * 0.5:
                    if is_over_time_limit:
                        print("Max audio limit reached. Forcing transcription...")
                    else:
                        print("Pause detected. Transcribing audio buffer...")
                    
                    # Add return_timestamps=True to satisfy the long-form generation requirement
                    result = asr_pipeline(
                        {"sampling_rate": SAMPLE_RATE, "raw": full_audio},
                        return_timestamps=True 
                    )
                    transcribed_text = result["text"].strip()
                    
                    if transcribed_text:
                        print(f"Recognized: {transcribed_text}")
                        await websocket.send(transcribed_text)
                
                # Reset buffers
                audio_buffer = []
                silence_counter = 0
                has_spoken = False # Wait for them to start speaking the next phrase

    except websockets.exceptions.ConnectionClosedOK:
        print(f"[-] Client {websocket.remote_address} disconnected normally.")
    except websockets.exceptions.ConnectionClosedError:
        print(f"[!] Client {websocket.remote_address} connection closed unexpectedly.")
    except Exception as e:
        print(f"[x] Error processing audio stream: {e}")
    finally:
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