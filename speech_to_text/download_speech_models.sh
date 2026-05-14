#!/bin/bash
set -e

# 1. Define the target workspace directory
WORKSPACE="/home/speech/steaming_speech"
WHISPER_DIR="$WORKSPACE/distil-large-v3.5"
TORCH_DIR="$WORKSPACE/torch_hub"

# Create directories if they don't exist
sudo mkdir -p "$WORKSPACE"
sudo chown -R $USER:$USER "$WORKSPACE" # Ensure current user has read/write permissions
mkdir -p "$WHISPER_DIR"
mkdir -p "$TORCH_DIR"

# 2. Download Distil-Whisper using the new 'hf' CLI
echo "Downloading distil-whisper-large-v3.5 to $WHISPER_DIR ..."

# Notice how --exclude is passed individually for each file type
hf download distil-whisper/distil-large-v3.5 \
    --local-dir "$WHISPER_DIR" \
    --exclude "*.md" \
    --exclude "*.h5" \
    --exclude "*.ot" \
    --exclude "*.msgpack"

# 3. Download Silero VAD
echo "Pre-downloading Silero VAD to $TORCH_DIR ..."
export TORCH_HOME="$TORCH_DIR"
python3 -c "
import torch
print('Fetching Silero VAD...')
torch.hub.load(repo_or_dir='snakers4/silero-vad', model='silero_vad', force_reload=False, trust_repo=True)
print('Silero VAD downloaded successfully!')
"

echo "==================================================="
echo "All models downloaded successfully to $WORKSPACE!"