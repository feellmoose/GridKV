package mem_storage

import (
	"testing"
	"time"
)

func TestStoredItem_ResolveConflict(t *testing.T) {
	tests := []struct {
		name     string
		item     *StoredItem
		other    *StoredItem
		expected bool
	}{
		{
			name:     "higher version wins",
			item:     &StoredItem{Version: 100},
			other:    &StoredItem{Version: 200},
			expected: true,
		},
		{
			name:     "lower version loses",
			item:     &StoredItem{Version: 200},
			other:    &StoredItem{Version: 100},
			expected: false,
		},
		{
			name:     "same version, prefer non-expired",
			item:     &StoredItem{Version: 100, ExpireAt: time.Unix(2000000000, 0)}, // Future time
			other:    &StoredItem{Version: 100, ExpireAt: time.Unix(1000000000, 0)}, // Past time
			expected: false,
		},
		{
			name:     "same version, prefer non-expired (reversed)",
			item:     &StoredItem{Version: 100, ExpireAt: time.Unix(1000000000, 0)}, // Past time
			other:    &StoredItem{Version: 100, ExpireAt: time.Unix(2000000000, 0)}, // Future time
			expected: true,
		},
		{
			name:     "nil other returns false",
			item:     &StoredItem{Version: 100},
			other:    nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.item.ResolveConflict(tt.other)
			if result != tt.expected {
				t.Errorf("ResolveConflict() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestStoredItem_CompareVersion(t *testing.T) {
	tests := []struct {
		name     string
		item     *StoredItem
		other    *StoredItem
		expected int
	}{
		{"item > other", &StoredItem{Version: 200}, &StoredItem{Version: 100}, 1},
		{"item < other", &StoredItem{Version: 100}, &StoredItem{Version: 200}, -1},
		{"item == other", &StoredItem{Version: 100}, &StoredItem{Version: 100}, 0},
		{"nil other", &StoredItem{Version: 100}, nil, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.item.CompareVersion(tt.other)
			if result != tt.expected {
				t.Errorf("CompareVersion() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestStoredItem_IsExpired(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		item     *StoredItem
		expected bool
	}{
		{"not expired", &StoredItem{ExpireAt: now.Add(time.Hour)}, false},
		{"expired", &StoredItem{ExpireAt: now.Add(-time.Hour)}, true},
		{"no expiration", &StoredItem{ExpireAt: time.Time{}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.item.IsExpired()
			if result != tt.expected {
				t.Errorf("IsExpired() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestStoredItem_IsTombstone(t *testing.T) {
	tests := []struct {
		name     string
		item     *StoredItem
		expected bool
	}{
		{"tombstone", &StoredItem{Value: nil, Version: 100}, true},
		{"tombstone empty slice", &StoredItem{Value: []byte{}, Version: 100}, true},
		{"not tombstone", &StoredItem{Value: []byte("data"), Version: 100}, false},
		{"no version", &StoredItem{Value: nil, Version: 0}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.item.IsTombstone()
			if result != tt.expected {
				t.Errorf("IsTombstone() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestStoredItem_Copy(t *testing.T) {
	original := &StoredItem{
		Version:  100,
		ExpireAt: time.Now(),
		Value:    []byte("test"),
		Key:      "key1",
	}

	copied := original.Copy()

	// Shallow copy: should share Value slice
	if copied.Value == nil || len(copied.Value) != len(original.Value) {
		t.Error("Copy() should preserve Value")
	}

	// Modify shared slice
	copied.Value[0] = 'X'

	// Original should be affected (shallow copy)
	if original.Value[0] != 'X' {
		t.Error("Copy() should share Value slice")
	}

	// Other fields should be independent
	copied.Version = 200
	if original.Version == 200 {
		t.Error("Copy() should have independent Version")
	}
}

func TestStoredItem_DeepCopy(t *testing.T) {
	original := &StoredItem{
		Version:  100,
		ExpireAt: time.Now(),
		Value:    []byte("test"),
		Key:      "key1",
	}

	copied := original.DeepCopy()

	// Deep copy: should have independent Value slice
	if len(copied.Value) != len(original.Value) {
		t.Error("DeepCopy() should preserve Value length")
	}

	// Modify copied slice
	copied.Value[0] = 'X'

	// Original should NOT be affected
	if original.Value[0] == 'X' {
		t.Error("DeepCopy() should have independent Value slice")
	}

	// All fields should be independent
	copied.Version = 200
	if original.Version == 200 {
		t.Error("DeepCopy() should have independent Version")
	}
}

func TestStoredItem_Reset(t *testing.T) {
	item := &StoredItem{
		Version:  100,
		ExpireAt: time.Now(),
		Value:    []byte("test"),
		Key:      "key1",
	}

	item.Reset()

	if item.Version != 0 {
		t.Error("Reset() should clear Version")
	}
	if !item.ExpireAt.IsZero() {
		t.Error("Reset() should clear ExpireAt")
	}
	if item.Value != nil {
		t.Error("Reset() should clear Value")
	}
	if item.Key != "" {
		t.Error("Reset() should clear Key")
	}
}

func TestStoredItem_WithVersion(t *testing.T) {
	original := &StoredItem{Version: 100, Value: []byte("test")}
	newItem := original.WithVersion(200)

	if newItem.Version != 200 {
		t.Error("WithVersion() should set new version")
	}
	if original.Version == 200 {
		t.Error("WithVersion() should not modify original")
	}
	if len(newItem.Value) != len(original.Value) {
		t.Error("WithVersion() should preserve Value")
	}
}

func TestStoredItem_WithTTL(t *testing.T) {
	original := &StoredItem{Value: []byte("test")}
	newItem := original.WithTTL(time.Hour)

	if newItem.ExpireAt.IsZero() {
		t.Error("WithTTL() should set ExpireAt")
	}
	if !original.ExpireAt.IsZero() {
		t.Error("WithTTL() should not modify original")
	}

	noTTL := original.WithTTL(0)
	if !noTTL.ExpireAt.IsZero() {
		t.Error("WithTTL(0) should clear ExpireAt")
	}
}

func TestStoredItem_Size(t *testing.T) {
	item := &StoredItem{
		Value: []byte("test"),
		Key:   "key1",
	}

	size := item.Size()
	if size <= 0 {
		t.Error("Size() should return positive value")
	}

	nilItem := (*StoredItem)(nil)
	if nilItem.Size() != 0 {
		t.Error("Size() of nil should be 0")
	}
}

func TestStoredItem_IsValid(t *testing.T) {
	tests := []struct {
		name     string
		item     *StoredItem
		expected bool
	}{
		{"valid", &StoredItem{Version: 100}, true},
		{"invalid no version", &StoredItem{Version: 0}, false},
		{"nil", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.item.IsValid()
			if result != tt.expected {
				t.Errorf("IsValid() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestStoredItem_Clone(t *testing.T) {
	original := &StoredItem{Version: 100, Value: []byte("test")}
	cloned := original.Clone()

	if cloned.Version != original.Version {
		t.Error("Clone() should preserve Version")
	}

	// Should be deep copy
	cloned.Value[0] = 'X'
	if original.Value[0] == 'X' {
		t.Error("Clone() should create independent copy")
	}
}
