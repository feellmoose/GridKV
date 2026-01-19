#!/bin/bash
set -e

cd "$(dirname "$0")"

echo "Building GridKV CGO shared library..."

# Get script directory and project root
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CGO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PROJECT_ROOT="$(cd "$CGO_DIR/.." && pwd)"

# Create wrapper in project root
WRAPPER_DIR="$PROJECT_ROOT/_cgo_wrapper"
cd "$PROJECT_ROOT"
rm -rf "$WRAPPER_DIR"
mkdir -p "$WRAPPER_DIR"
trap "rm -rf $WRAPPER_DIR" EXIT

# Create wrapper main.go
cat > "$WRAPPER_DIR/main.go" << 'EOF'
//go:build cgo
// +build cgo

package main

import _ "github.com/feellmoose/gridkv/cgo"

func main() {}
EOF

# Build shared library
BUILD_OUTPUT=$(go build -tags cgo -buildmode=c-shared -o "$CGO_DIR/libgridkv_cgo.so" "$WRAPPER_DIR" 2>&1)
BUILD_STATUS=$?

if [ $BUILD_STATUS -ne 0 ]; then
    echo "Error: Failed to build shared library"
    echo "$BUILD_OUTPUT"
    exit 1
fi

# Filter warnings from output
echo "$BUILD_OUTPUT" | grep -v "warning" || true

cd "$CGO_DIR/tests"

echo "Building C test program..."
gcc -o test_c test_c.c -L.. -lgridkv_cgo -I.. -std=c99 -Wall

echo "Running C tests..."
LD_LIBRARY_PATH=.. ./test_c

echo "All C tests passed!"
