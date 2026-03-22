#!/bin/bash

# Variables
SERVICE_NAME="enterprise_chat"
EXECUTABLE_PATH="/home/alice/enterprise_chat/enterprise_chat"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

# Ensure the binary exists
if [ ! -f "$EXECUTABLE_PATH" ]; then
  echo "Error: The binary '$EXECUTABLE_PATH' does not exist."
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
User=alice
WorkingDirectory=/home/alice/enterprise_chat
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