package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// disk reports 100 GiB (a 10 GiB reserve) with beyond bytes free past the
// reserve, minus what uploads have written so far.
func disk(t *testing.T, dir string, beyond int64) {
	t.Helper()
	orig := fsutil.DiskSpace
	t.Cleanup(func() { fsutil.DiskSpace = orig })
	fsutil.DiskSpace = func(string) (uint64, uint64, error) {
		written := int64(0)
		entries, _ := os.ReadDir(filepath.Join(dir, importDir))
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				written += info.Size()
			}
		}
		return uint64(10<<30 + beyond - written), 100 << 30, nil
	}
}

func upload(s *Server, body io.Reader, length int64) (string, int) {
	r := httptest.NewRequest("POST", "/api/import/x", body)
	r.ContentLength = length
	path, _, status, _ := s.receiveImport(httptest.NewRecorder(), r)
	return path, status
}

func leftovers(t *testing.T, dir string) int {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(dir, importDir))
	return len(entries)
}

// An upload without a length is checked against the reserve as it streams,
// and aborts (507) with its temp file removed once it would cross it.
func TestImportUploadStreamsAgainstReserve(t *testing.T) {
	s := &Server{stateDir: t.TempDir()}
	disk(t, s.stateDir, 6<<20)
	if _, status := upload(s, bytes.NewReader(make([]byte, 16<<20)), -1); status != http.StatusInsufficientStorage || leftovers(t, s.stateDir) != 0 || s.importPending.Load() != 0 {
		t.Fatalf("status %d, %d temp files, %d pending", status, leftovers(t, s.stateDir), s.importPending.Load())
	}
	if path, status := upload(s, bytes.NewReader(make([]byte, 1<<20)), -1); status != 0 || path == "" || s.importPending.Load() != 0 {
		t.Fatalf("small chunked upload: %d", status)
	}
}

// A second upload counts what the first still promises to write.
func TestImportUploadsCountEachOther(t *testing.T) {
	s := &Server{stateDir: t.TempDir()}
	disk(t, s.stateDir, 12<<20)
	pr, pw := io.Pipe()
	first := make(chan int)
	go func() { _, status := upload(s, pr, 8<<20); first <- status }()
	if _, err := pw.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	// 7 MiB still promised and 1 MiB written: 8 more do not fit in 12.
	if _, status := upload(s, bytes.NewReader(make([]byte, 8<<20)), 8<<20); status != http.StatusInsufficientStorage {
		t.Fatalf("concurrent upload past the reserve: %d", status)
	}
	if _, err := pw.Write(make([]byte, 7<<20)); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	if status := <-first; status != 0 || s.importPending.Load() != 0 {
		t.Fatalf("first upload: %d, %d pending", status, s.importPending.Load())
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.RemoveAll(filepath.Join(s.stateDir, importDir))) // imported and removed
	if _, status := upload(s, bytes.NewReader(make([]byte, 8<<20)), 8<<20); status != 0 {
		t.Fatalf("upload alone: %d", status)
	}
}
