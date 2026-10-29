package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/9star-academy/bakctl/internal/config"
)

func TestLocalRoundTrip(t *testing.T) {
	client := NewLocalClient(filepath.Join(t.TempDir(), "backups"))
	ctx := context.Background()
	if err := client.Upload(ctx, "folder/备份.bin", strings.NewReader("contents"), 8); err != nil {
		t.Fatal(err)
	}
	files, err := client.List(ctx, "folder/")
	if err != nil || len(files) != 1 || files[0].Path != "folder/备份.bin" || files[0].Size != 8 {
		t.Fatalf("list: %+v %v", files, err)
	}
	r, err := client.Download(ctx, files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(data) != "contents" {
		t.Fatalf("download: %q %v", data, err)
	}
	if err := client.Delete(ctx, files[0].Path); err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(ctx, files[0].Path); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	files, err = client.List(ctx, "folder/")
	if err != nil || len(files) != 0 {
		t.Fatalf("list after delete: %v %v", files, err)
	}
}

type failingReader struct{}

func (failingReader) Read(p []byte) (int, error) {
	copy(p, "bad")
	return 3, errors.New("read failure")
}

type cancelReader struct{ cancel context.CancelFunc }

func (r cancelReader) Read(p []byte) (int, error) { r.cancel(); copy(p, "data"); return 4, io.EOF }

func TestLocalFailedUploadPreservesPreviousObject(t *testing.T) {
	for _, kind := range []string{"reader-error", "size-mismatch", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			client := NewLocalClient(dir)
			if err := os.WriteFile(filepath.Join(dir, "backup"), []byte("previous"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var r io.Reader = failingReader{}
			if kind == "size-mismatch" {
				r = strings.NewReader("short")
			}
			if kind == "cancelled" {
				r = cancelReader{cancel}
			}
			if err := client.Upload(ctx, "backup", r, 10); err == nil {
				t.Fatal("failed upload reported success")
			}
			got, err := os.ReadFile(filepath.Join(dir, "backup"))
			if err != nil || string(got) != "previous" {
				t.Fatalf("existing object changed: %q %v", got, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary upload leaked: %v %v", entries, err)
			}
		})
	}
}

func TestLocalRejectsPathEscape(t *testing.T) {
	client := NewLocalClient(t.TempDir())
	for _, name := range []string{"../escape", "/absolute", "a/../../escape", ".", "", `a\..\escape`} {
		t.Run(name, func(t *testing.T) {
			if err := client.Upload(context.Background(), name, strings.NewReader("x"), 1); err == nil {
				t.Fatal("unsafe upload accepted")
			}
			if r, err := client.Download(context.Background(), name); err == nil {
				r.Close()
				t.Fatal("unsafe download accepted")
			}
			if err := client.Delete(context.Background(), name); err == nil {
				t.Fatal("unsafe delete accepted")
			}
		})
	}
	if _, err := client.List(context.Background(), "../"); err == nil {
		t.Fatal("unsafe prefix accepted")
	}
}

func TestLocalRejectsSymlinkEscapeAndListErrors(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "backup"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	client := NewLocalClient(dir)
	if err := client.Upload(context.Background(), "link/backup", strings.NewReader("bad"), 3); err == nil {
		t.Fatal("symlink upload escaped")
	}
	if r, err := client.Download(context.Background(), "link/backup"); err == nil {
		r.Close()
		t.Fatal("symlink download escaped")
	}
	if err := client.Delete(context.Background(), "link/backup"); err == nil {
		t.Fatal("symlink delete escaped")
	}
	got, _ := os.ReadFile(filepath.Join(outside, "backup"))
	if string(got) != "outside" {
		t.Fatalf("outside file changed: %q", got)
	}
	if _, err := client.List(context.Background(), ""); err == nil {
		t.Fatal("symlink silently included/ignored")
	}
	missing := NewLocalClient(filepath.Join(t.TempDir(), "missing"))
	if _, err := missing.List(context.Background(), ""); err == nil {
		t.Fatal("missing storage directory silently listed")
	}
}

func TestLocalCancellation(t *testing.T) {
	client := NewLocalClient(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Upload(ctx, "backup", strings.NewReader("x"), 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("upload: %v", err)
	}
	if _, err := client.Download(ctx, "backup"); !errors.Is(err, context.Canceled) {
		t.Fatalf("download: %v", err)
	}
	if _, err := client.List(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("list: %v", err)
	}
	if err := client.Delete(ctx, "backup"); !errors.Is(err, context.Canceled) {
		t.Fatalf("delete: %v", err)
	}
}

func newTestS3(t *testing.T, endpoint string) *S3Client {
	t.Helper()
	s, err := NewS3Client(&config.StorageTarget{Endpoint: endpoint, Bucket: "bucket", AccessKey: "test-access", SecretKey: "test-secret", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Independently derives the signature from an explicitly supplied AWS canonical URI/query.
func expectedSignature(req *http.Request, canonicalURI, canonicalQuery string) string {
	date := req.Header.Get("x-amz-date")
	hdr := "host:" + req.URL.Host + "\nx-amz-content-sha256:" + req.Header.Get("x-amz-content-sha256") + "\nx-amz-date:" + date + "\n"
	signed := "host;x-amz-content-sha256;x-amz-date"
	canonical := strings.Join([]string{req.Method, canonicalURI, canonicalQuery, hdr, signed, req.Header.Get("x-amz-content-sha256")}, "\n")
	hash := sha256.Sum256([]byte(canonical))
	scope := date[:8] + "/us-east-1/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + date + "\n" + scope + "\n" + hex.EncodeToString(hash[:])
	mac := func(key []byte, value string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(value))
		return h.Sum(nil)
	}
	key := mac([]byte("AWS4test-secret"), date[:8])
	key = mac(key, "us-east-1")
	key = mac(key, "s3")
	key = mac(key, "aws4_request")
	return hex.EncodeToString(mac(key, toSign))
}

func TestS3CanonicalSigning(t *testing.T) {
	s := newTestS3(t, "https://s3.example.com")
	for _, tc := range []struct{ name, raw, uri, query string }{
		{"ascii", "https://bucket.s3.example.com/db.bak.gz", "/db.bak.gz", ""},
		{"empty-path", "https://bucket.s3.example.com?prefix=db_", "/", "prefix=db_"},
		{"unicode", "https://bucket.s3.example.com/备份 file+%.bak.gz", "/%E5%A4%87%E4%BB%BD%20file%2B%25.bak.gz", ""},
		{"query-space", "https://s3.example.com/bucket?prefix=my+backups%2Fdb_", "/bucket", "prefix=my%20backups%2Fdb_"},
		{"query-sort", "https://s3.example.com/bucket?z=3&a=2&a=1", "/bucket", "a=1&a=2&z=3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.raw
			if tc.name == "unicode" {
				raw = "https://bucket.s3.example.com/" + url.PathEscape("备份 file+%.bak.gz")
			}
			req, err := http.NewRequest(http.MethodGet, raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			s.signRequest(req, nil)
			got := strings.Split(req.Header.Get("Authorization"), "Signature=")[1]
			if want := expectedSignature(req, tc.uri, tc.query); got != want {
				t.Fatalf("signature %s, expected %s for %s?%s", got, want, tc.uri, tc.query)
			}
		})
	}
}

func TestS3URLPreservesEndpointAndKey(t *testing.T) {
	for _, tc := range []struct{ endpoint, key, want string }{
		{"https://s3.example.com/proxy", "folder//a/../b", "https://s3.example.com/proxy/bucket/folder//a/../b"},
		{"https://bucket.s3.example.com", "备份 name+%.bak", "https://bucket.s3.example.com/%E5%A4%87%E4%BB%BD%20name%2B%25.bak"},
		{"https://notbucket.s3.example.com/base/", "key", "https://notbucket.s3.example.com/base/bucket/key"},
	} {
		s := newTestS3(t, tc.endpoint)
		if got := s.getURL(tc.key); got != tc.want {
			t.Errorf("URL = %q; want %q", got, tc.want)
		}
	}
}

func TestS3Pagination(t *testing.T) {
	for _, explicitMarker := range []bool{false, true} {
		t.Run(fmt.Sprint(explicitMarker), func(t *testing.T) {
			calls := 0
			s := newTestS3(t, "https://bucket.s3.example.com")
			s.client.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				var b strings.Builder
				b.WriteString("<ListBucketResult>")
				if calls == 1 {
					b.WriteString("<IsTruncated>true</IsTruncated>")
					if explicitMarker {
						b.WriteString("<NextMarker>db_0999.bak.gz</NextMarker>")
					}
					for i := 0; i < 1000; i++ {
						fmt.Fprintf(&b, "<Contents><Key>db_%04d.bak.gz</Key><Size>1</Size><LastModified>2026-10-29T00:00:00Z</LastModified></Contents>", i)
					}
				} else {
					if r.URL.Query().Get("marker") != "db_0999.bak.gz" {
						t.Fatalf("wrong marker: %s", r.URL.RawQuery)
					}
					b.WriteString("<IsTruncated>false</IsTruncated><Contents><Key>db_1000.bak.gz</Key><Size>1</Size><LastModified>2026-10-29T01:00:00Z</LastModified></Contents>")
				}
				b.WriteString("</ListBucketResult>")
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(b.String()))}, nil
			})
			got, err := s.List(context.Background(), "db_")
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1001 || calls != 2 {
				t.Fatalf("got %d objects over %d calls", len(got), calls)
			}
		})
	}
}

func TestS3PaginationRejectsNoProgress(t *testing.T) {
	s := newTestS3(t, "https://s3.example.com")
	s.client.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>")), Header: make(http.Header)}, nil
	})
	if _, err := s.List(context.Background(), ""); err == nil {
		t.Fatal("truncated empty page accepted")
	}
}

func TestS3RoundTripHTTP(t *testing.T) {
	objects := make(map[string][]byte)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("unsigned request")
		}
		switch r.Method {
		case http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			if r.ContentLength != int64(len(data)) {
				t.Errorf("length=%d data=%d", r.ContentLength, len(data))
			}
			hash := sha256.Sum256(data)
			if r.Header.Get("x-amz-content-sha256") != hex.EncodeToString(hash[:]) {
				t.Error("payload hash mismatch")
			}
			objects[r.URL.Path] = data
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			data, ok := objects[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(data)
		case http.MethodDelete:
			delete(objects, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	s := newTestS3(t, "https://s3.example.com")
	s.client.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})
	for _, seekable := range []bool{true, false} {
		t.Run(fmt.Sprint(seekable), func(t *testing.T) {
			var input io.Reader = strings.NewReader("payload")
			if !seekable {
				input = io.LimitReader(input, 7)
			}
			if err := s.Upload(context.Background(), "folder/备份", input, 7); err != nil {
				t.Fatal(err)
			}
			r, err := s.Download(context.Background(), "folder/备份")
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(r)
			r.Close()
			if err != nil || string(data) != "payload" {
				t.Fatalf("download=%q %v", data, err)
			}
			if err := s.Delete(context.Background(), "folder/备份"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Download(context.Background(), "folder/备份"); err == nil {
				t.Fatal("missing object downloaded")
			}
		})
	}
}

func TestS3UploadFailsBeforeRequestForBadInput(t *testing.T) {
	s := newTestS3(t, "https://s3.example.com")
	s.client.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("request sent for invalid input")
		return nil, nil
	})
	if err := s.Upload(context.Background(), "key", strings.NewReader("short"), 10); err == nil {
		t.Fatal("size mismatch accepted")
	}
	if err := s.Upload(context.Background(), "key", failingReader{}, 10); err == nil {
		t.Fatal("reader error accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Upload(ctx, "key", bytes.NewReader([]byte("x")), 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled upload: %v", err)
	}
}

func TestS3ErrorsAndConfiguration(t *testing.T) {
	for _, endpoint := range []string{"https://", "ftp://example.com", "https://user:secret@example.com", "https://example.com?token=secret", "https://example.com/#fragment"} {
		_, err := NewS3Client(&config.StorageTarget{Endpoint: endpoint, Bucket: "bucket", AccessKey: "key", SecretKey: "secret"})
		if err == nil {
			t.Errorf("invalid endpoint accepted: %s", endpoint)
		}
	}
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := newTestS3(t, "https://s3.example.com")
			s.client.Transport = testTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("denied"))}, nil
			})
			if err := s.Upload(context.Background(), "key", strings.NewReader("x"), 1); err == nil {
				t.Fatal("upload accepted error")
			}
			if _, err := s.Download(context.Background(), "key"); err == nil {
				t.Fatal("download accepted error")
			}
			if _, err := s.List(context.Background(), ""); err == nil {
				t.Fatal("list accepted error")
			}
			if err := s.Delete(context.Background(), "key"); err == nil {
				t.Fatal("delete accepted error")
			}
		})
	}
}

func TestFactory(t *testing.T) {
	for _, kind := range []string{"local", "s3"} {
		_, err := NewClient(&config.StorageTarget{Type: kind, Bucket: "bucket", AccessKey: "key", SecretKey: "secret"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewClient(&config.StorageTarget{Type: "webdav"}); err == nil {
		t.Fatal("unsupported target accepted")
	}
}

// These public AWS fixtures detect regressions independently of our signer.
// https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sig-v4-header-based-auth.html
func TestAWSPublishedSigningVectors(t *testing.T) {
	s := &S3Client{accessKey: "AKIAIOSFODNN7EXAMPLE", secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", region: "us-east-1"}
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ url, rangeHeader, signature string }{
		{"https://examplebucket.s3.amazonaws.com/test.txt", "bytes=0-9", "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"},
		{"https://examplebucket.s3.amazonaws.com?lifecycle", "", "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543"},
		{"https://examplebucket.s3.amazonaws.com?max-keys=2&prefix=J", "", "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"},
	} {
		req, err := http.NewRequest(http.MethodGet, tc.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.rangeHeader != "" {
			req.Header.Set("Range", tc.rangeHeader)
		}
		s.signRequestHash(req, sha256Hex(nil), now)
		if !strings.HasSuffix(req.Header.Get("Authorization"), "Signature="+tc.signature) {
			t.Fatalf("AWS published vector mismatch: %s", req.Header.Get("Authorization"))
		}
	}
}

func TestS3RejectsMalformedLists(t *testing.T) {
	for _, body := range []string{"<ListBucketResult>", "<Error><Code>AccessDenied</Code></Error>", "<ListBucketResult><IsTruncated>true</IsTruncated><NextMarker>same</NextMarker></ListBucketResult>"} {
		s := newTestS3(t, "https://s3.example.com")
		calls := 0
		s.client.Transport = testTransport(func(*http.Request) (*http.Response, error) {
			calls++
			if calls > 2 {
				t.Fatal("pagination did not stop")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if _, err := s.List(context.Background(), ""); err == nil {
			t.Errorf("malformed response accepted: %s", body)
		}
	}
}

func TestS3UploadKeepsCallerFileOpenAndOffset(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "payload")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err = file.WriteString("skip-payload"); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Seek(5, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	s := newTestS3(t, "https://s3.example.com")
	s.client.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		got, err := io.ReadAll(r.Body)
		if err != nil || string(got) != "payload" {
			t.Fatalf("body=%q %v", got, err)
		}
		r.Body.Close()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if err := s.Upload(context.Background(), "key", file, 7); err != nil {
		t.Fatal(err)
	}
	if offset, err := file.Seek(0, io.SeekCurrent); err != nil || offset != 5 {
		t.Fatalf("caller file closed or moved: %d %v", offset, err)
	}
}
