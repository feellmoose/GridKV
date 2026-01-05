package zerocopy

import (
	"reflect"
	"testing"
)

func TestStringToBytes(t *testing.T) {
	s := "hello"
	b := StringToBytes(s)

	if len(b) != len(s) {
		t.Fatalf("Expected length %d, got %d", len(s), len(b))
	}

	for i := 0; i < len(s); i++ {
		if b[i] != s[i] {
			t.Fatalf("Mismatch at index %d: expected %c, got %c", i, s[i], b[i])
		}
	}
}

func TestStringToBytes_Empty(t *testing.T) {
	b := StringToBytes("")
	if b != nil {
		t.Fatalf("Expected nil for empty string, got %v", b)
	}
}

func TestBytesToString(t *testing.T) {
	b := []byte("hello")
	s := BytesToString(b)

	if s != "hello" {
		t.Fatalf("Expected 'hello', got '%s'", s)
	}
}

func TestBytesToString_Empty(t *testing.T) {
	s := BytesToString(nil)
	if s != "" {
		t.Fatalf("Expected empty string for nil, got '%s'", s)
	}

	s = BytesToString([]byte{})
	if s != "" {
		t.Fatalf("Expected empty string for empty slice, got '%s'", s)
	}
}

func TestFastCloneBytes(t *testing.T) {
	src := []byte("hello")
	dst := FastCloneBytes(src)

	if !reflect.DeepEqual(src, dst) {
		t.Fatalf("Expected %v, got %v", src, dst)
	}

	if len(dst) != len(src) {
		t.Fatalf("Expected length %d, got %d", len(src), len(dst))
	}

	dst[0] = 'H'
	if src[0] == 'H' {
		t.Fatal("Modifying dst should not affect src")
	}
}

func TestFastCloneBytes_Nil(t *testing.T) {
	dst := FastCloneBytes(nil)
	if dst != nil {
		t.Fatalf("Expected nil, got %v", dst)
	}
}

func TestFastCloneBytes_Empty(t *testing.T) {
	dst := FastCloneBytes([]byte{})
	if len(dst) != 0 {
		t.Fatalf("Expected empty slice, got length %d", len(dst))
	}
}

func BenchmarkStringToBytes(b *testing.B) {
	s := "hello world"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = StringToBytes(s)
	}
}

func BenchmarkBytesToString(b *testing.B) {
	bytes := []byte("hello world")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BytesToString(bytes)
	}
}

func BenchmarkFastCloneBytes(b *testing.B) {
	src := []byte("hello world")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FastCloneBytes(src)
	}
}
