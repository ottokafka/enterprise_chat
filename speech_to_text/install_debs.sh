#!/bin/bash
set -e

echo "Updating packages..."
sudo apt-get update -y
sudo apt-get install -y python3-venv python3-pip python3-dev ffmpeg

echo "Creating Python virtual environment..."
python3 -m venv venv
source venv/bin/activate

echo "Installing PyTorch (Stable) with Torchaudio..."
# Stable PyTorch relies on CUDA 12.x wheels, which are 100% compatible with CUDA 13.0 drivers
pip3 install torch torchvision torchaudio

echo "Installing Transformers, Accelerate, and WebSockets..."
pip3 install transformers accelerate websockets numpy scipy

echo "Setup complete! Run 'source venv/bin/activate' to activate the environment."