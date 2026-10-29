package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/9star-academy/bakctl/internal/config"
	"github.com/9star-academy/bakctl/internal/crypto"
	"github.com/9star-academy/bakctl/internal/storage"
)

type memoryClient struct {
	data                            []byte
	files                           []storage.FileInfo
	listErr, downloadErr, deleteErr error
	deleted                         []string
}

func (m *memoryClient) Upload(context.Context, string, io.Reader, int64) error {
	return errors.New("not used")
}
func (m *memoryClient) Download(context.Context, string) (io.ReadCloser, error) {
	if m.downloadErr != nil {
		return nil, m.downloadErr
	}
	return io.NopCloser(bytes.NewReader(m.data)), nil
}
func (m *memoryClient) List(context.Context, string) ([]storage.FileInfo, error) {
	return m.files, m.listErr
}
func (m *memoryClient) Delete(_ context.Context, name string) error {
	m.deleted = append(m.deleted, name)
	return m.deleteErr
}

type tarEntry struct {
	name, body, link string
	kind             byte
}

func archiveBytes(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		hdr := &tar.Header{Name: entry.name, Typeflag: kind, Mode: 0600, Linkname: entry.link}
		if kind == tar.TypeDir {
			hdr.Mode = 0700
		}
		if kind == tar.TypeReg {
			hdr.Size = int64(len(entry.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestLegacyBackupCompatibility(t *testing.T) {
	tarData := archiveBytes(t, tarEntry{name: "original.txt", body: "legacy bytes"})
	compressed := gzipBytes(t, tarData)
	encrypted, err := crypto.Encrypt(compressed, crypto.DeriveKey("legacy key"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, kind, key, remote, want string
		data                          []byte
		directory                     bool
	}{
		{"single file", "file", "", "old.bak.gz", "legacy bytes", compressed, false},
		{"single file directory", "directory", "", "old.bak.gz", "legacy bytes", compressed, true},
		{"encrypted file", "file", "legacy key", "old.bak.gz.enc", "legacy bytes", encrypted, false},
		{"raw database", "raw", "", "old.bak.gz", "CREATE TABLE demo(id int);", gzipBytes(t, []byte("CREATE TABLE demo(id int);")), false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "restored")
			if err := NewEngine().RestoreWithType(context.Background(), &memoryClient{data: tt.data}, tt.remote, tt.key, dest, tt.kind); err != nil {
				t.Fatal(err)
			}
			result := dest
			if tt.directory {
				result = filepath.Join(dest, "original.txt")
			}
			got, err := os.ReadFile(result)
			if err != nil || string(got) != tt.want {
				t.Fatalf("restored=%q err=%v", got, err)
			}
		})
	}
	t.Run("ambiguous legacy without hint", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "restored")
		err := NewEngine().Restore(context.Background(), &memoryClient{data: compressed}, "old.bak.gz", "", dest)
		if err == nil || !strings.Contains(err.Error(), "--legacy-type") {
			t.Fatalf("expected actionable ambiguity error, got %v", err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatal("ambiguous restore published destination")
		}
	})
	t.Run("configured existing file hint", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "original")
		if err := os.WriteFile(source, []byte("source"), 0600); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(t.TempDir(), "restored")
		if err := NewEngine().RestoreForSource(context.Background(), &memoryClient{data: compressed}, "old.bak.gz", "", dest, source); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("configured database hint", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "restored")
		raw := gzipBytes(t, []byte("legacy sql"))
		if err := NewEngine().RestoreForSource(context.Background(), &memoryClient{data: raw}, "old.bak.gz", "", dest, "postgres://example/db"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unambiguous legacy directory", func(t *testing.T) {
		data := gzipBytes(t, archiveBytes(t, tarEntry{name: "sub", kind: tar.TypeDir}, tarEntry{name: "sub/data", body: "hello"}))
		dest := filepath.Join(t.TempDir(), "restored")
		if err := NewEngine().Restore(context.Background(), &memoryClient{data: data}, "old.bak.gz", "", dest); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRestoreRejectsUnsafeArchivesWithoutPublishing(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"parent traversal", []tarEntry{{name: "../escape", body: "bad"}}},
		{"absolute path", []tarEntry{{name: "/escape", body: "bad"}}},
		{"backslash", []tarEntry{{name: "dir\\escape", body: "bad"}}},
		{"escaping symlink", []tarEntry{{name: "link", kind: tar.TypeSymlink, link: "../outside"}}},
		{"absolute symlink", []tarEntry{{name: "link", kind: tar.TypeSymlink, link: "/outside"}}},
		{"chained symlink escape", []tarEntry{{name: "sub", kind: tar.TypeDir}, {name: "sub/link", kind: tar.TypeSymlink, link: ".."}, {name: "escape", kind: tar.TypeSymlink, link: "sub/link/../outside"}}},
		{"cyclic symlink", []tarEntry{{name: "a", kind: tar.TypeSymlink, link: "b"}, {name: "b", kind: tar.TypeSymlink, link: "a"}}},
		{"symlink parent", []tarEntry{{name: "alias", kind: tar.TypeSymlink, link: "."}, {name: "alias/evil", body: "bad"}}},
		{"duplicate", []tarEntry{{name: "file", body: "one"}, {name: "file", body: "two"}}},
		{"hardlink", []tarEntry{{name: "link", kind: tar.TypeLink, link: "file"}}},
		{"fifo", []tarEntry{{name: "fifo", kind: tar.TypeFifo}}},
		{"file root", []tarEntry{{name: ".", body: "bad"}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "restored")
			data := gzipBytes(t, archiveBytes(t, tt.entries...))
			if err := NewEngine().RestoreWithType(context.Background(), &memoryClient{data: data}, "old.bak.gz", "", dest, "directory"); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatalf("destination published after failure: %v", err)
			}
		})
	}
}

func TestRestoreIntegrityErrorsDoNotPublish(t *testing.T) {
	header := append([]byte(payloadMagic), kindFile, 0, 0, 1, 128)
	plain := gzipBytes(t, append(header, []byte("data")...))
	var encrypted bytes.Buffer
	if err := crypto.EncryptStream(&encrypted, bytes.NewReader(plain), "correct"); err != nil {
		t.Fatal(err)
	}
	damaged := append([]byte(nil), plain...)
	damaged[len(damaged)-5] ^= 0xff
	cases := []struct {
		name, remote, key, legacy string
		data                      []byte
		downloadErr               error
	}{
		{name: "download failure", remote: "x", downloadErr: errors.New("offline")},
		{name: "invalid gzip", remote: "x", data: []byte("not gzip")},
		{name: "truncated gzip", remote: "x", data: plain[:len(plain)-1]},
		{name: "CRC mismatch", remote: "x", data: damaged},
		{name: "missing key", remote: "x.enc", data: encrypted.Bytes()},
		{name: "wrong key", remote: "x.enc", key: "wrong", data: encrypted.Bytes()},
		{name: "truncated encrypted", remote: "x.enc", key: "correct", data: encrypted.Bytes()[:encrypted.Len()-20]},
		{name: "bad legacy type", remote: "x", legacy: "unknown", data: plain},
		{name: "truncated metadata", remote: "x", data: gzipBytes(t, []byte(payloadMagic))},
		{name: "unknown metadata kind", remote: "x", data: gzipBytes(t, append([]byte(payloadMagic), 'z', 0, 0, 1, 128))},
		{name: "invalid metadata permissions", remote: "x", data: gzipBytes(t, append([]byte(payloadMagic), kindFile, 255, 255, 255, 255))},
		{name: "legacy raw needs hint", remote: "x", data: gzipBytes(t, []byte("raw bytes"))},
		{name: "legacy file multiple entries", remote: "x", legacy: "file", data: gzipBytes(t, archiveBytes(t, tarEntry{name: "a", body: "one"}, tarEntry{name: "b", body: "two"}))},
		{name: "trailing tar garbage", remote: "x", legacy: "directory", data: gzipBytes(t, append(archiveBytes(t, tarEntry{name: "a", body: "one"}), 1))},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "result")
			client := &memoryClient{data: tt.data, downloadErr: tt.downloadErr}
			if err := NewEngine().RestoreWithType(context.Background(), client, tt.remote, tt.key, dest, tt.legacy); err == nil {
				t.Fatal("expected restore failure")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatalf("destination published: %v", err)
			}
		})
	}
}

func TestExactJobSelectionAndRetentionErrors(t *testing.T) {
	job := &config.BackupJob{Name: "app", Target: "local:shared", Keep: 1}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := &memoryClient{files: []storage.FileInfo{
		{Path: "shared/app_20260101_000000.bak.gz", LastModified: old},
		{Path: "shared/app_db_20260101_000000.bak.gz", LastModified: old},
		{Path: "shared/app_20261399_000000.bak.gz", LastModified: old},
		{Path: "shared/app_20260101_000001.000000001-0123456789abcdef.bak.gz.enc", LastModified: old.Add(time.Second)},
		{Path: "shared/app_notes.txt", LastModified: old},
		{Path: "elsewhere/app_20260101_000000.bak.gz", LastModified: old},
		{Path: "shared/app_sub/app_20260101_000000.bak.gz", LastModified: old},
	}}
	files, err := ListJobBackups(context.Background(), client, job)
	if err != nil || len(files) != 2 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if files[1].Path != "shared/app_20260101_000001.000000001-0123456789abcdef.bak.gz.enc" {
		t.Fatalf("latest=%s", files[1].Path)
	}
	if err := pruneOldBackups(context.Background(), client, job); err != nil {
		t.Fatal(err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != files[0].Path {
		t.Fatalf("deleted=%v", client.deleted)
	}
	client.deleteErr = errors.New("denied")
	if err := pruneOldBackups(context.Background(), client, job); err == nil {
		t.Fatal("delete failure must propagate")
	}
	client.listErr = errors.New("offline")
	if err := pruneOldBackups(context.Background(), client, job); err == nil {
		t.Fatal("list failure must propagate")
	}
}

func TestRestoreCancelledDoesNotPublish(t *testing.T) {
	e, job, target := fixture(t, "payload")
	res, err := e.RunBackup(context.Background(), job, target)
	if err != nil {
		t.Fatal(err)
	}
	client, err := storage.NewClient(target)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dest := filepath.Join(t.TempDir(), "restore")
	if err := e.Restore(ctx, client, res.FileName, "", dest); err == nil {
		t.Fatal("cancelled restore must fail")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("cancelled restore published destination")
	}
}

func TestEmptyDirectoryAndDanglingInternalSymlinks(t *testing.T) {
	for _, links := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "dangling and chained links"}[links], func(t *testing.T) {
			e, job, target := fixture(t, "unused")
			job.Source = t.TempDir()
			if links {
				if err := os.Mkdir(filepath.Join(job.Source, "sub"), 0700); err != nil {
					t.Fatal(err)
				}
				for name, link := range map[string]string{"missing": "not-created", "sub/up": "..", "alias": "sub/up/missing"} {
					if err := os.Symlink(link, filepath.Join(job.Source, filepath.FromSlash(name))); err != nil {
						t.Fatal(err)
					}
				}
			}
			res, err := e.RunBackup(context.Background(), job, target)
			if err != nil {
				t.Fatal(err)
			}
			client, err := storage.NewClient(target)
			if err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "restore")
			if err := e.Restore(context.Background(), client, res.FileName, "", dest); err != nil {
				t.Fatal(err)
			}
			if info, err := os.Stat(dest); err != nil || !info.IsDir() {
				t.Fatalf("restored directory missing: %v", err)
			}
			if links {
				if link, err := os.Readlink(filepath.Join(dest, "alias")); err != nil || link != "sub/up/missing" {
					t.Fatalf("restored link=%s err=%v", link, err)
				}
			}
		})
	}
}

func TestLegacyFilenameCannotMasqueradeAsVersionHeader(t *testing.T) {
	// v1 tar begins with a filename. The v2 magic therefore starts with NUL,
	// which cannot appear in the original filesystem filename.
	data := gzipBytes(t, archiveBytes(t, tarEntry{name: "BAKCTL2\nr", body: "original bytes"}))
	dest := filepath.Join(t.TempDir(), "restore")
	if err := NewEngine().RestoreWithType(context.Background(), &memoryClient{data: data}, "old.bak.gz", "", dest, "file"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "original bytes" {
		t.Fatalf("legacy collision: %q %v", got, err)
	}
}
