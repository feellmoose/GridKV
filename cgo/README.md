# GridKV CGO Package

This package provides C language bindings for GridKV.

## Building and Testing

### Run C Tests

The easiest way to run C tests is using the test runner script:

```bash
cd cgo/tests
./test_runner.sh
```

Or use make from cgo directory:

```bash
cd cgo
make test-c
```

Or run via Go test:

```bash
cd cgo
go test -tags cgo -v .
```

### Manual Build (if needed)

If you need to build manually:

1. Build the shared library (requires creating a wrapper):
```bash
# Create a wrapper main package
mkdir -p _wrapper
cat > _wrapper/main.go << 'EOF'
package main
import _ "github.com/feellmoose/gridkv/cgo"
func main() {}
EOF

# Build shared library
go build -tags cgo -buildmode=c-shared -o libgkv.so _wrapper
```

2. Compile C test:
```bash
gcc -o test_c test_c.c -L. -lgkv -I. -std=c99 -Wall
```

3. Run test:
```bash
LD_LIBRARY_PATH=. ./test_c
```

### Run Go Tests

```bash
cd cgo
go test -tags cgo -v .
```

## API Overview

### Core Operations
- `gkv_new()` - Create new GridKV instance
- `gkv_set()` - Set key-value pair
- `gkv_get()` - Get value by key
- `gkv_delete()` - Delete key
- `gkv_close()` - Close instance

### Statistics and Monitoring
- `gkv_stats()` - Get comprehensive statistics (cluster, network, storage)
- `gkv_health_check()` - Health check
- `gkv_wait_ready()` - Wait for cluster ready
- `gkv_version()` - Get library version

### Helper Functions
- `gkv_result_has_error()` - Check if result has error (convenience function)
- `gkv_get_result_has_error()` - Check if get result has error (convenience function)

### Memory Management
- `gkv_free_result()` - Free result from gkv_new()
- `gkv_free_get_result()` - Free result from gkv_get()
- `gkv_free_stats()` - Free stats from gkv_stats()
- `gkv_free_string()` - Free error strings

## Files

- `cgo.go` - CGO interface implementation
- `gkv.h` - C header file with complete API documentation
- `cgo_test.go` - Go test wrapper for C tests
- `Makefile` - Build automation
- `tests/` - C language test directory
  - `test_c.c` - Pure C language test program with comprehensive tests
  - `test_runner.sh` - Test runner script that builds shared library and runs C tests
- `bindings_example.py` - Python binding example using ctypes
- `bindings_example.java` - Java binding example using Panama FFI
- `BINDINGS_EVALUATION.md` - Multi-language binding evaluation report

## Why C Language Tests?

The C language tests (`tests/test_c.c`) serve important purposes:

1. **Verification**: Ensures the C interface can be used by pure C programs without Go runtime
2. **Example**: Provides a complete reference implementation for C developers
3. **Integration**: Validates that the shared library works correctly when linked by external C code

The test runner script automates:
- Building the shared library from Go code
- Compiling the C test program
- Running and validating all C interface functions