#!/bin/bash

set -e

VERSION=${1:-$(./tools/version.sh)}
echo "Updating Homebrew formula for version: $VERSION"

# Create temporary directory for downloads
TEMP_DIR=$(mktemp -d)
cd "$TEMP_DIR"

echo "Downloading release assets..."

# Download the release assets
curl -L -o "atlas9-${VERSION}-apple-amd64.tar.gz" \
  "https://github.com/sio2boss/atlas9/releases/download/${VERSION}/atlas9-${VERSION}-apple-amd64.tar.gz"
curl -L -o "atlas9-${VERSION}-apple-arm64.tar.gz" \
  "https://github.com/sio2boss/atlas9/releases/download/${VERSION}/atlas9-${VERSION}-apple-arm64.tar.gz"
curl -L -o "atlas9-${VERSION}-linux-amd64.tar.gz" \
  "https://github.com/sio2boss/atlas9/releases/download/${VERSION}/atlas9-${VERSION}-linux-amd64.tar.gz"
curl -L -o "atlas9-${VERSION}-linux-arm64.tar.gz" \
  "https://github.com/sio2boss/atlas9/releases/download/${VERSION}/atlas9-${VERSION}-linux-arm64.tar.gz"

echo "Generating checksums..."

# Generate checksums
sha256sum *.tar.gz > checksums.txt
cat checksums.txt

# Extract checksums
APPLE_AMD64_SHA=$(grep "apple-amd64" checksums.txt | cut -d' ' -f1)
APPLE_ARM64_SHA=$(grep "apple-arm64" checksums.txt | cut -d' ' -f1)
LINUX_AMD64_SHA=$(grep "linux-amd64" checksums.txt | cut -d' ' -f1)
LINUX_ARM64_SHA=$(grep "linux-arm64" checksums.txt | cut -d' ' -f1)

echo "Apple AMD64 SHA256: $APPLE_AMD64_SHA"
echo "Apple ARM64 SHA256: $APPLE_ARM64_SHA"
echo "Linux AMD64 SHA256: $LINUX_AMD64_SHA"
echo "Linux ARM64 SHA256: $LINUX_ARM64_SHA"

# Go back to the original directory
cd - > /dev/null

# Update the local Homebrew formula
echo "Updating local Homebrew formula..."

cat > homebrew/Formula/atlas9.rb << EOF
class Atlas9 < Formula
  desc "Download and install files from the web with automatic extraction and installation"
  homepage "https://github.com/sio2boss/atlas9"
  version "${VERSION#v}"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/sio2boss/atlas9/releases/download/${VERSION}/atlas9-${VERSION}-apple-arm64.tar.gz"
      sha256 "${APPLE_ARM64_SHA}"
    else
      url "https://github.com/sio2boss/atlas9/releases/download/${VERSION}/atlas9-${VERSION}-apple-amd64.tar.gz"
      sha256 "${APPLE_AMD64_SHA}"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/sio2boss/atlas9/releases/download/${VERSION}/atlas9-${VERSION}-linux-arm64.tar.gz"
      sha256 "${LINUX_ARM64_SHA}"
    else
      url "https://github.com/sio2boss/atlas9/releases/download/${VERSION}/atlas9-${VERSION}-linux-amd64.tar.gz"
      sha256 "${LINUX_AMD64_SHA}"
    end
  end

  def install
    bin.install "atlas9"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/atlas9 --version")
  end
end
EOF

echo "Formula updated! You can now commit and push the changes to your homebrew tap."
echo ""
echo "Next steps:"
echo "1. Commit and push the changes to homebrew-tap"
echo "2. Users can then install with: brew install sio2boss/tap/atlas9"
echo ""

# Cleanup
rm -rf "$TEMP_DIR" 