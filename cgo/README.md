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
go build -tags cgo -buildmode=c-shared -o libgridkv_cgo.so _wrapper
```

2. Compile C test:
```bash
gcc -o test_c test_c.c -L. -lgridkv_cgo -I. -std=c99 -Wall
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

## Files

- `cgo.go` - CGO interface implementation
- `gridkv_cgo.h` - C header file
- `cgo_test.go` - Go test wrapper for C tests
- `Makefile` - Build automation
- `tests/` - C language test directory
  - `test_c.c` - Pure C language test program (227 lines)
  - `test_runner.sh` - Test runner script that builds shared library and runs C tests

## Why C Language Tests?

The C language tests (`tests/test_c.c`) serve important purposes:

1. **Verification**: Ensures the C interface can be used by pure C programs without Go runtime
2. **Example**: Provides a complete reference implementation for C developers
3. **Integration**: Validates that the shared library works correctly when linked by external C code

The test runner script automates:
- Building the shared library from Go code
- Compiling the C test program
- Running and validating all C interface functions