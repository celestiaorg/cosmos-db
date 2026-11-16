package db

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/stretchr/testify/require"
)

func TestPebbleDBBackend(t *testing.T) {
	name := fmt.Sprintf("test_%x", randStr(12))
	dir := os.TempDir()
	db, err := NewDB(name, PebbleDBBackend, dir)
	require.NoError(t, err)
	defer cleanupDBDir(dir, name)

	_, ok := db.(*PebbleDB)
	require.True(t, ok)
}

// func TestPebbleDBStats(t *testing.T) {
// 	name := fmt.Sprintf("test_%x", randStr(12))
// 	dir := os.TempDir()
// 	db, err := NewDB(name, PebbleDBBackend, dir)
// 	require.NoError(t, err)
// 	defer cleanupDBDir(dir, name)

// 	require.NotEmpty(t, db.Stats())
// }

func BenchmarkPebbleDBRandomReadsWrites(b *testing.B) {
	name := fmt.Sprintf("test_%x", randStr(12))
	dir := os.TempDir()
	db, err := NewDB(name, PebbleDBBackend, dir)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		require.NoError(b, db.Close())
		cleanupDBDir("", name)
	}()

	benchmarkRandomReadsWrites(b, db)
}

// pebbleCacheSize reads the block cache size pebble recorded in its OPTIONS file.
func pebbleCacheSize(t *testing.T, dir, name string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, name+DBFileSuffix, "OPTIONS-*"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	bz, err := os.ReadFile(files[0])
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^\s*cache_size=(\d+)$`).FindSubmatch(bz)
	require.NotNil(t, m, "no cache_size in %s", files[0])
	return string(m[1])
}

func TestPebbleDBCacheSize(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"nil options", nil, "8388608"},
		{"key unset", OptionsMap{"maxopenfiles": 100}, "8388608"},
		{"zero", OptionsMap{PebbleCacheSizeKey: 0}, "8388608"},
		{"negative", OptionsMap{PebbleCacheSizeKey: -1}, "8388608"},
		{"not a number", OptionsMap{PebbleCacheSizeKey: "big"}, "8388608"},
		{"int", OptionsMap{PebbleCacheSizeKey: 64 << 20}, "67108864"},
		{"int64", OptionsMap{PebbleCacheSizeKey: int64(128 << 20)}, "134217728"},
		{"string", OptionsMap{PebbleCacheSizeKey: "33554432"}, "33554432"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := NewDBwithOptions("app", PebbleDBBackend, dir, tc.opts)
			require.NoError(t, err)
			require.NoError(t, db.Set([]byte("k"), []byte("v")))
			require.Equal(t, tc.want, pebbleCacheSize(t, dir, "app"))
			require.NoError(t, db.Close())
		})
	}
}

// capturePebbleCaches records every cache NewPebbleDB creates.
func capturePebbleCaches(t *testing.T) *[]*pebble.Cache {
	t.Helper()
	var caches []*pebble.Cache
	orig := newPebbleCache
	newPebbleCache = func(size int64) *pebble.Cache {
		c := orig(size)
		caches = append(caches, c)
		return c
	}
	t.Cleanup(func() { newPebbleCache = orig })
	return &caches
}

// probedCaches stays reachable so pebble's invariants finalizer never sees
// the count of 1 that a panicking Ref leaves behind.
var probedCaches []*pebble.Cache

// requireFreed checks the cache has no references left: Ref panics on a
// cache whose count reached zero.
func requireFreed(t *testing.T, c *pebble.Cache) {
	t.Helper()
	probedCaches = append(probedCaches, c)
	require.Panics(t, c.Ref, "cache still referenced")
}

func TestPebbleDBCacheReleasedOnClose(t *testing.T) {
	caches := capturePebbleCaches(t)
	dir := t.TempDir()
	db, err := NewDBwithOptions("app", PebbleDBBackend, dir, OptionsMap{PebbleCacheSizeKey: 16 << 20})
	require.NoError(t, err)
	require.Len(t, *caches, 1)
	c := (*caches)[0]
	require.Equal(t, int64(16<<20), c.MaxSize())

	// The DB holds the only reference while open.
	c.Ref()
	c.Unref()
	require.NoError(t, db.Set([]byte("k"), []byte("v")))
	v, err := db.Get([]byte("k"))
	require.NoError(t, err)
	require.Equal(t, []byte("v"), v)

	require.NoError(t, db.Close())
	requireFreed(t, c)
}

func TestPebbleDBCacheReleasedOnOpenError(t *testing.T) {
	caches := capturePebbleCaches(t)
	dir := t.TempDir()
	// A regular file where the DB directory should be makes Open fail.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app"+DBFileSuffix), nil, 0o600))
	_, err := NewDBwithOptions("app", PebbleDBBackend, dir, OptionsMap{PebbleCacheSizeKey: 16 << 20})
	require.Error(t, err)
	require.Len(t, *caches, 1)
	requireFreed(t, (*caches)[0])
}

// TestPebbleDBCacheNoLeak opens and closes repeatedly. With -tags invariants
// (without -race), pebble exits the process from a finalizer if a collected
// cache still holds a reference.
func TestPebbleDBCacheNoLeak(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		db, err := NewDBwithOptions("app", PebbleDBBackend, dir, OptionsMap{PebbleCacheSizeKey: 8 << 20})
		require.NoError(t, err)
		require.NoError(t, db.Set([]byte{byte(i)}, []byte("v")))
		require.NoError(t, db.Close())
	}
	runtime.GC()
	runtime.GC()
}
