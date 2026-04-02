#!/bin/bash

# Variables
SERVICE_NAME="enterprise_chat"
WORKING_DIR="$(pwd)"
CURRENT_USER="$(whoami)"
EXECUTABLE_PATH="${WORKING_DIR}/${SERVICE_NAME}"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

# Ensure the binary exists
if [ ! -f "$EXECUTABLE_PATH" ]; then
  echo "Error: The binary '$EXECUTABLE_PATH' does not exist."
  echo "Make sure you have built the application with 'go build -o $SERVICE_NAME' and are running this script from the project root."
  exit 1
fi

# Create the systemd service file
echo "Creating systemd service file at $SERVICE_FILE..."
sudo bash -c "cat > $SERVICE_FILE" <<EOL
[Unit]
Description=enterprise_chat Service
After=network.target

[Service]
Type=simple
ExecStart=$EXECUTABLE_PATH
Restart=on-failure
User=$CURRENT_USER
WorkingDirectory=$WORKING_DIR
Environment=GIN_MODE=release
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOL

# Set correct permissions
echo "Setting permissions for the service file..."
sudo chmod 644 $SERVICE_FILE

# Reload systemd daemon, start, and enable the service
echo "Reloading systemd..."
sudo systemctl daemon-reload

echo "Starting the service..."
sudo systemctl start $SERVICE_NAME

echo "Enabling the service to start on boot..."
sudo systemctl enable $SERVICE_NAME

# Check the status of the service
echo "Checking service status..."
sudo systemctl status $SERVICE_NAME

echo "Setup complete. The service is now running and enabled at boot."