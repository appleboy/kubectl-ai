#!/usr/bin/env bash
set -euo pipefail

# Check for required commands
for cmd in curl tar; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Error: Required command '$cmd' is not installed. Please install '$cmd' to continue."
    exit 1
  fi
done

REPO="GoogleCloudPlatform/kubectl-ai"
BINARY="kubectl-ai"

# Detect OS
sysOS="$(uname | tr '[:upper:]' '[:lower:]')"
case "$sysOS" in
linux)
  OS="Linux"
  EXT="tar.gz"
  ;;
darwin)
  OS="Darwin"
  EXT="tar.gz"
  ;;
mingw* | msys* | cygwin* | windowsnt)
  OS="Windows"
  EXT="zip"
  ;;
*)
  echo "Unsupported operating system detected."
  echo "Please refer to the manual installation guide:"
  echo "https://github.com/GoogleCloudPlatform/kubectl-ai#manual-installation-linux-macos-and-windows"
  exit 1
  ;;
esac

# Detect ARCH
ARCH="$(uname -m)"
case "$ARCH" in
x86_64 | amd64) ARCH="x86_64" ;;
arm64 | aarch64) ARCH="arm64" ;;
*)
  echo "Unsupported system architecture detected."
  echo "Please refer to the manual installation guide:"
  echo "https://github.com/GoogleCloudPlatform/kubectl-ai#manual-installation-linux-macos-and-windows"
  exit 1
  ;;
esac

# Get latest version tag from GitHub API, Use GITHUB_TOKEN if available to avoid potential rate limit
if [ -n "${GITHUB_TOKEN:-}" ]; then
  auth_hdr="Authorization: token $GITHUB_TOKEN"
else
  auth_hdr=""
fi
LATEST_TAG=$(curl -s -H "$auth_hdr" \
  "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
if [ -z "$LATEST_TAG" ]; then
  echo "Error: Unable to retrieve the latest release version from GitHub."
  exit 1
fi

# Compose download URL
ARCHIVE="kubectl-ai_${OS}_${ARCH}.${EXT}"
URL="https://github.com/$REPO/releases/download/$LATEST_TAG/$ARCHIVE"

# Download and extract
echo "Downloading release from: $URL ..."
curl -fSL --retry 3 "$URL" -o "$ARCHIVE"

if [ "$OS" = "Windows" ]; then
  if command -v unzip >/dev/null 2>&1; then
    unzip -o "$ARCHIVE"
  else
    # Try PowerShell extraction
    if command -v powershell.exe >/dev/null 2>&1; then
      powershell.exe -Command "Expand-Archive -Path '$ARCHIVE' -DestinationPath . -Force"
    else
      echo "Error: Neither 'unzip' nor PowerShell is available to extract the zip file on Windows."
      exit 1
    fi
  fi
  # Move binary to current directory (user must add to PATH manually)
  echo "Please move '$BINARY.exe' to a directory included in your PATH (e.g., C:\\Windows\\System32) to complete the installation."
else
  tar --no-same-owner -xzf "$ARCHIVE"
  # Move binary to /usr/local/bin (may require sudo)
  echo "Installing '$BINARY' to /usr/local/bin (administrator privileges may be required)..."
  sudo install -m 0755 "$BINARY" /usr/local/bin/
fi

# Clean up
rm "$ARCHIVE"

echo "✅ Installation complete! You can now run '$BINARY --help' to get started."
