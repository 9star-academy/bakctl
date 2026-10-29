package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/9star-academy/bakctl/internal/config"
)

type S3Client struct {
	endpoint  string
	bucket    string
	accessKey string
	secretKey string
	region    string
	client    *http.Client
}

func NewS3Client(t *config.StorageTarget) (*S3Client, error) {
	if t == nil {
		return nil, fmt.Errorf("S3 存储配置不能为空")
	}
	endpoint := t.Endpoint
	if endpoint == "" {
		endpoint = "https://s3.amazonaws.com"
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("S3 endpoint 必须是有效的 HTTP(S) 地址，且不能包含用户信息、查询参数或片段")
	}
	if t.Bucket == "" || strings.ContainsAny(t.Bucket, "/\\\x00\r\n") {
		return nil, fmt.Errorf("必须提供有效的 S3 bucket 名称")
	}
	if t.AccessKey == "" || t.SecretKey == "" {
		return nil, fmt.Errorf("必须提供 S3 access-key 和 secret-key")
	}
	region := t.Region
	if region == "" {
		region = "auto"
		if t.Endpoint == "" {
			region = "us-east-1"
		}
	}
	return &S3Client{
		endpoint: u.String(), bucket: t.Bucket, accessKey: t.AccessKey,
		secretKey: t.SecretKey, region: region,
		client: &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error {
			// Signatures are bound to the host/path. Report redirects so the user
			// can configure the correct regional endpoint rather than reusing auth.
			return http.ErrUseLastResponse
		}},
	}, nil
}

func (s *S3Client) bucketURL() *url.URL {
	u, _ := url.Parse(s.endpoint) // Validated when the client is created.
	if !strings.HasPrefix(strings.ToLower(u.Hostname()), strings.ToLower(s.bucket)+".") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + s.bucket
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawPath = uriEncode(u.Path, false)
	return u
}

func (s *S3Client) getURL(objectKey string) string {
	u := s.bucketURL()
	if objectKey != "" {
		// S3 object names are not filesystem paths: do not clean '..' or '//'.
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + objectKey
	}
	u.RawPath = uriEncode(u.Path, false)
	return u.String()
}

func (s *S3Client) Upload(ctx context.Context, remotePath string, reader io.Reader, size int64) error {
	if remotePath == "" {
		return fmt.Errorf("S3 对象名称不能为空")
	}
	body, payloadHash, cleanup, err := preparePayload(ctx, reader, size)
	if err != nil {
		return err
	}
	defer cleanup()
	// Hide Close so net/http never closes a file owned by the caller.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.getURL(remotePath), struct{ io.Reader }{body})
	if err != nil {
		return err
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	s.signRequestHash(req, payloadHash, time.Now().UTC())
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("上传至 S3/R2 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s3ResponseError("上传", resp)
	}
	return nil
}

// preparePayload hashes from the reader's current offset, then rewinds it for
// HTTP. Nonseekable readers are spooled to a private file, keeping memory bounded.
func preparePayload(ctx context.Context, reader io.Reader, size int64) (io.Reader, string, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, "", nil, err
	}
	if size < 0 {
		return nil, "", nil, fmt.Errorf("备份大小不能为负数")
	}
	h := sha256.New()
	var body io.ReadSeeker
	var cleanup func()
	var start int64
	var n int64
	var err error
	if seeker, ok := reader.(io.ReadSeeker); ok {
		start, err = seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, "", nil, err
		}
		body = seeker
		cleanup = func() { _, _ = seeker.Seek(start, io.SeekStart) }
		n, err = io.Copy(h, contextReader{ctx, seeker})
	} else {
		f, createErr := os.CreateTemp("", "bakctl-upload-*")
		if createErr != nil {
			return nil, "", nil, createErr
		}
		body = f
		cleanup = func() { f.Close(); os.Remove(f.Name()) }
		n, err = io.Copy(io.MultiWriter(f, h), contextReader{ctx, reader})
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && n != size {
		err = fmt.Errorf("备份大小不一致: 预期 %d 字节，实际 %d 字节", size, n)
	}
	if err == nil {
		_, err = body.Seek(start, io.SeekStart)
	}
	if err != nil {
		cleanup()
		return nil, "", nil, err
	}
	return contextReader{ctx, body}, hex.EncodeToString(h.Sum(nil)), cleanup, nil
}

func (s *S3Client) Download(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	if remotePath == "" {
		return nil, fmt.Errorf("S3 对象名称不能为空")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.getURL(remotePath), nil)
	if err != nil {
		return nil, err
	}
	s.signRequest(req, nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, s3ResponseError("下载", resp)
	}
	return resp.Body, nil
}

type listBucketResult struct {
	XMLName     xml.Name `xml:"ListBucketResult"`
	IsTruncated bool     `xml:"IsTruncated"`
	NextMarker  string   `xml:"NextMarker"`
	Contents    []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
}

func (s *S3Client) List(ctx context.Context, prefix string) ([]FileInfo, error) {
	var list []FileInfo
	marker := ""
	for {
		u := s.bucketURL()
		q := url.Values{}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if marker != "" {
			q.Set("marker", marker)
		}
		u.RawQuery = canonicalQuery(q)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		s.signRequest(req, nil)
		res, err := s.listPage(req)
		if err != nil {
			return nil, err
		}
		for _, c := range res.Contents {
			list = append(list, FileInfo{Path: c.Key, Size: c.Size, LastModified: c.LastModified})
		}
		if !res.IsTruncated {
			return list, nil
		}
		next := res.NextMarker
		if next == "" && len(res.Contents) > 0 {
			next = res.Contents[len(res.Contents)-1].Key
		}
		if next == "" || next <= marker {
			return nil, fmt.Errorf("S3 分页响应无有效的后续 marker，不能返回不完整列表")
		}
		marker = next
	}
}

func (s *S3Client) listPage(req *http.Request) (*listBucketResult, error) {
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, s3ResponseError("列举", resp)
	}
	var res listBucketResult
	if err := xml.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("解析 S3 列表失败: %w", err)
	}
	return &res, nil
}

func (s *S3Client) Delete(ctx context.Context, remotePath string) error {
	if remotePath == "" {
		return fmt.Errorf("S3 对象名称不能为空")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.getURL(remotePath), nil)
	if err != nil {
		return err
	}
	s.signRequest(req, nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return s3ResponseError("删除", resp)
	}
	return nil
}

func s3ResponseError(operation string, resp *http.Response) error {
	var detail struct {
		Code string `xml:"Code"`
	}
	// Only expose the service error code, not a body that may echo credentials
	// or arbitrary backup contents. Bound error response reads.
	_ = xml.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&detail)
	if detail.Code != "" {
		return fmt.Errorf("S3 %s失败 (HTTP %d, %s)", operation, resp.StatusCode, detail.Code)
	}
	return fmt.Errorf("S3 %s失败 (HTTP %d)", operation, resp.StatusCode)
}

func (s *S3Client) signRequest(req *http.Request, payload []byte) {
	s.signRequestHash(req, sha256Hex(payload), time.Now().UTC())
}

// AWS SigV4 requires RFC 3986 encoding with uppercase hex, sorted encoded
// query pairs, an absolute '/' URI, and no S3 object-path normalization.
func (s *S3Client) signRequestHash(req *http.Request, payloadHash string, now time.Time) {
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if req.URL.Path == "" {
		req.URL.Path = "/"
	}
	canonicalURI := uriEncode(req.URL.Path, false)
	req.URL.RawPath = canonicalURI
	req.URL.RawQuery = canonicalQuery(req.URL.Query())
	headers := map[string]string{"host": req.URL.Host}
	for key, values := range req.Header {
		key = strings.ToLower(key)
		if strings.HasPrefix(key, "x-amz-") || key == "content-type" || key == "content-md5" || key == "range" {
			headers[key] = strings.Join(strings.Fields(strings.Join(values, ",")), " ")
		}
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var canonicalHeaders strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&canonicalHeaders, "%s:%s\n", key, headers[key])
	}
	signedHeaders := strings.Join(keys, ";")
	canonicalRequest := strings.Join([]string{req.Method, canonicalURI, req.URL.RawQuery, canonicalHeaders.String(), signedHeaders, payloadHash}, "\n")
	credentialScope := dateStamp + "/" + s.region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + sha256Hex([]byte(canonicalRequest))
	signature := hex.EncodeToString(hmacSHA256(getSignatureKey(s.secretKey, dateStamp, s.region, "s3"), []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", s.accessKey, credentialScope, signedHeaders, signature))
}

func uriEncode(value string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || c == '/' && !encodeSlash {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	return b.String()
}

func canonicalQuery(values url.Values) string {
	var pairs []string
	for key, vals := range values {
		for _, value := range vals {
			pairs = append(pairs, uriEncode(key, true)+"="+uriEncode(value, true))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func getSignatureKey(key, dateStamp, regionName, serviceName string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+key), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(regionName))
	kService := hmacSHA256(kRegion, []byte(serviceName))
	return hmacSHA256(kService, []byte("aws4_request"))
}
