.PHONY: test build-cgo build-c-test run-c-test clean test-c

# Build CGO package as shared library (using test runner script)
build-cgo:
	./test_runner.sh build-only || true

# Build C test program
build-c-test: build-cgo
	gcc -o test_c test_c.c -L. -lgkv -I. -std=c99 -Wall

# Run C test
run-c-test: build-c-test
	LD_LIBRARY_PATH=. ./test_c

# Run C tests using test runner script
test-c:
	cd tests && ./test_runner.sh

# Run Go tests
test:
	go test -tags cgo -v .

# Clean build artifacts
clean:
	rm -f test_c libgkv.so libgkv.h *.o

# All tests
all: test test-c
