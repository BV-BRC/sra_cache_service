package cache

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPathShardingAndShortAccessionSafety(t *testing.T) {
	c := New("/cache-root")

	cases := []struct {
		accession string
		want      string
	}{
		{"SRR12345678", filepath.Join("/cache-root", "sra", "SRR", "SRR123", "SRR12345678", "SRR12345678.sra")},
		{"SRR123", filepath.Join("/cache-root", "sra", "SRR", "SRR123", "SRR123", "SRR123.sra")}, // exactly 6 chars
		{"SR", filepath.Join("/cache-root", "sra", "SR", "SR", "SR", "SR.sra")},                  // shorter than p3
		{"S", filepath.Join("/cache-root", "sra", "S", "S", "S", "S.sra")},
	}
	for _, tc := range cases {
		got := c.Path(tc.accession)
		if got != tc.want {
			t.Errorf("Path(%q) = %q, want %q", tc.accession, got, tc.want)
		}
	}

	// Must never panic, including on the empty string.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Path(\"\") panicked: %v", r)
			}
		}()
		_ = c.Path("")
	}()
}

func TestPublishAndPath(t *testing.T) {
	root := t.TempDir()
	c := New(root)

	data := []byte("hello sra world")
	path, size, err := c.Publish("SRR00000001", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("Publish size = %d, want %d", size, len(data))
	}
	if path != c.Path("SRR00000001") {
		t.Errorf("Publish path = %q, want %q", path, c.Path("SRR00000001"))
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("published content = %q, want %q", got, data)
	}

	// No stray temp file left behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "SRR00000001.sra" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory contents = %v, want exactly [SRR00000001.sra]", names)
	}
}

func TestPublishEmptyAccessionErrors(t *testing.T) {
	c := New(t.TempDir())
	if _, _, err := c.Publish("", strings.NewReader("x")); err == nil {
		t.Fatalf("Publish(\"\"): want error, got nil")
	}
}

func TestRemoveCleansUpEmptyDirsButKeepsSiblings(t *testing.T) {
	root := t.TempDir()
	c := New(root)

	// Two accessions sharing the SRR/SRR123 shard prefix.
	if _, _, err := c.Publish("SRR12300001", strings.NewReader("a")); err != nil {
		t.Fatalf("Publish 1: %v", err)
	}
	if _, _, err := c.Publish("SRR12300002", strings.NewReader("b")); err != nil {
		t.Fatalf("Publish 2: %v", err)
	}

	if err := c.Remove("SRR12300001"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(c.Path("SRR12300001")); !os.IsNotExist(err) {
		t.Errorf("SRR12300001 file still present after Remove (err=%v)", err)
	}
	// Its own now-empty accession directory should be gone...
	if _, err := os.Stat(filepath.Dir(c.Path("SRR12300001"))); !os.IsNotExist(err) {
		t.Errorf("SRR12300001's accession directory still present after Remove")
	}
	// ...but the shared shard directory (still holding SRR12300002) must survive.
	sharedShardDir := filepath.Dir(filepath.Dir(c.Path("SRR12300001")))
	if _, err := os.Stat(sharedShardDir); err != nil {
		t.Errorf("shared shard directory %s removed, want kept (sibling still cached): %v", sharedShardDir, err)
	}
	// The sibling itself must be untouched.
	if _, err := os.Stat(c.Path("SRR12300002")); err != nil {
		t.Errorf("sibling SRR12300002 disturbed by Remove of SRR12300001: %v", err)
	}

	// Now remove the sibling too: the whole SRR/ tree down to "sra/" should
	// be pruned away, but "sra/" itself (the cache root's shard root) must
	// remain (it's the root Remove climbs up to and stops at).
	if err := c.Remove("SRR12300002"); err != nil {
		t.Fatalf("Remove sibling: %v", err)
	}
	sraRoot := filepath.Join(root, "sra")
	if _, err := os.Stat(sraRoot); err != nil {
		t.Errorf("sra root directory removed, want kept: %v", err)
	}
	entries, err := os.ReadDir(sraRoot)
	if err != nil {
		t.Fatalf("ReadDir sra root: %v", err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("sra root not empty after removing all accessions: %v", names)
	}
}

func TestRemoveMissingAccessionIsNotAnError(t *testing.T) {
	c := New(t.TempDir())
	if err := c.Remove("SRR_NEVER_PUBLISHED"); err != nil {
		t.Errorf("Remove of never-published accession: %v", err)
	}
}

// TestPublishAtomicity is the "partial-file invisibility" test the phase-5
// prompt calls for by name: a slow writer and a concurrent reader/stat loop
// must never observe a truncated file -- only "not found" or "complete".
func TestPublishAtomicity(t *testing.T) {
	root := t.TempDir()
	c := New(root)
	accession := "SRR55500001"
	path := c.Path(accession)

	const size = 4 << 20 // 4 MiB, big enough that a slow writer overlaps readers
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	slow := &slowReader{r: bytes.NewReader(data), delay: 200 * time.Microsecond, chunk: 32 * 1024}

	var (
		stop         int32
		wg           sync.WaitGroup
		sawPartial   bool
		sawPartialMu sync.Mutex
		observations int64
	)

	checkFile := func() {
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return // fine: not published yet
			}
			return // transient open error (e.g. mid-rename on some platforms); not a correctness failure here
		}
		defer f.Close()
		got, err := io.ReadAll(f)
		atomic.AddInt64(&observations, 1)
		if err != nil {
			return
		}
		if len(got) != size || !bytes.Equal(got, data) {
			sawPartialMu.Lock()
			sawPartial = true
			sawPartialMu.Unlock()
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for atomic.LoadInt32(&stop) == 0 {
			checkFile()
		}
		// one final check after the writer is done, to make sure we observe
		// the completed file at least once
		checkFile()
	}()

	if _, _, err := c.Publish(accession, slow); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	atomic.StoreInt32(&stop, 1)
	wg.Wait()

	sawPartialMu.Lock()
	defer sawPartialMu.Unlock()
	if sawPartial {
		t.Fatalf("concurrent reader observed a partial/corrupt file during Publish")
	}
	if atomic.LoadInt64(&observations) == 0 {
		t.Log("reader never successfully opened the file during the race window (not a failure, but a weak test run)")
	}

	// Final state must be the complete file.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after Publish: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("final published content does not match source")
	}
}

// slowReader dribbles out src in small chunks with a delay between them, to
// widen the window in which a concurrent reader could observe a partial
// write if Publish were not actually atomic.
type slowReader struct {
	r     io.Reader
	delay time.Duration
	chunk int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if len(p) > s.chunk {
		p = p[:s.chunk]
	}
	time.Sleep(s.delay)
	return s.r.Read(p)
}
