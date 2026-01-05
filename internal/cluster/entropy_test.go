package cluster

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/feellmoose/gridkv/internal/mem_storage"
	"github.com/feellmoose/gridkv/internal/utils/executor"
)

func TestAntiEntropy_DigestAndSync(t *testing.T) {
	storeA, _ := mem_storage.New(mem_storage.DefaultConfig())
	storeB, _ := mem_storage.New(mem_storage.DefaultConfig())

	_ = storeA.Set("user:1", &mem_storage.StoredItem{Key: "user:1", Value: []byte("a"), Version: 1})
	_ = storeA.Set("user:2", &mem_storage.StoredItem{Key: "user:2", Value: []byte("a"), Version: 1})
	_ = storeB.Set("user:1", &mem_storage.StoredItem{Key: "user:1", Value: []byte("b"), Version: 2})
	_ = storeB.Set("user:3", &mem_storage.StoredItem{Key: "user:3", Value: []byte("c"), Version: 1})

	aeA := newAntiEntropy(antiEntropyConfig{Store: storeA})
	aeB := newAntiEntropy(antiEntropyConfig{Store: storeB})

	bloomB, vvB := aeB.Digest("user:")
	diffKeys, err := aeA.Sync(bloomB, vvB)
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	got := make(map[string]bool)
	for _, k := range diffKeys {
		got[k] = true
	}
	for _, k := range []string{"user:1", "user:2"} {
		if !got[k] {
			t.Fatalf("missing expected diff key %s", k)
		}
	}
}

func TestAntiEntropy_Digest(t *testing.T) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 1})

	entropy := newAntiEntropy(antiEntropyConfig{
		Store:    store,
		Executor: exec,
		Interval: 1 * time.Minute,
	})

	for i := 0; i < 5; i++ {
		key := "key" + string(rune(i))
		item := &mem_storage.StoredItem{
			Version: int64(i),
			Value:   []byte("value"),
		}
		store.Set(key, item)
	}

	bloom, vv := entropy.Digest("")
	if len(bloom) == 0 {
		t.Fatal("Digest() returned empty bloom")
	}
	if vv == nil {
		t.Fatal("Digest() returned nil version vector")
	}
}

func TestAntiEntropy_Sync(t *testing.T) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 1})

	entropy := newAntiEntropy(antiEntropyConfig{
		Store:    store,
		Executor: exec,
		Interval: 1 * time.Minute,
	})

	diffKeys, err := entropy.Sync(nil, nil)
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if diffKeys == nil {
		t.Fatal("Sync() returned nil diffKeys")
	}
}

func TestAntiEntropy_Lifecycle(t *testing.T) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	exec, _ := executor.New(executor.Opts{Name: "test", Workers: 1})

	entropy := newAntiEntropy(antiEntropyConfig{
		Store:    store,
		Executor: exec,
		Interval: 50 * time.Millisecond,
	})

	if err := entropy.start(); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if err := entropy.stop(); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}

func TestReplay_SaveLoadCheckpoint(t *testing.T) {
	store, _ := mem_storage.New(mem_storage.DefaultConfig())
	r := newReplay(replayConfig{
		CheckpointPath: filepath.Join(t.TempDir(), "cp.dat"),
		Store:          store,
	})

	ops := []*mem_storage.SyncOperation{
		{
			Key:    "k1",
			OpType: mem_storage.OpSet,
			Item: &mem_storage.StoredItem{
				Key:     "k1",
				Value:   []byte("v1"),
				Version: 10,
			},
		},
		{
			Key:    "k2",
			OpType: mem_storage.OpDelete,
			Item: &mem_storage.StoredItem{
				Key:     "k2",
				Version: 11,
			},
		},
	}

	if err := r.SaveCheckpoint(ops); err != nil {
		t.Fatalf("SaveCheckpoint() error = %v", err)
	}

	loaded, err := r.LoadCheckpoint()
	if err != nil {
		t.Fatalf("LoadCheckpoint() error = %v", err)
	}

	if len(loaded) != len(ops) {
		t.Fatalf("loaded ops len = %d, want %d", len(loaded), len(ops))
	}
	for i := range ops {
		if loaded[i].Key != ops[i].Key || loaded[i].OpType != ops[i].OpType || loaded[i].Item.Version != ops[i].Item.Version {
			t.Fatalf("loaded op mismatch: got %+v want %+v", loaded[i], ops[i])
		}
	}
}
