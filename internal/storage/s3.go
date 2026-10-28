package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
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
	endpoint := t.Endpoint
	if endpoint == "" {
		endpoint = "https://s3.amazonaws.com"
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}

	region := t.Region
	if region == "" {
		region = "auto"
	}

	return &S3Client{
		endpoint:  endpoint,
		bucket:    t.Bucket,
		accessKey: t.AccessKey,
		secretKey: t.SecretKey,
		region:    region,
		client:    &http.Client{Timeout: 30 * time.Minute},
	}, nil
}

func (s *S3Client) getURL(objectKey string) string {
	u, _ := url.Parse(s.endpoint)
	cleanKey := strings.TrimPrefix(objectKey, "/")

	// If bucket is not already in host (like bucket.r2... or bucket.s3...)
	if !strings.Contains(u.Host, s.bucket+".") && s.bucket != "" {
		u.Path = path.Join(s.bucket, cleanKey)
	} else {
		u.Path = path.Join(u.Path, cleanKey)
	}
	return u.String()
}

func (s *S3Client) Upload(ctx context.Context, remotePath string, reader io.Reader, size int64) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}

	reqURL := s.getURL(remotePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, bytes.NewReader(data))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/octet-stream")
	s.signRequest(req, data)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("上传至 S3/R2 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("S3/R2 返回异常状态码 %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func (s *S3Client) Download(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	reqURL := s.getURL(remotePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	s.signRequest(req, nil)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("下载失败 (HTTP %d)", resp.StatusCode)
	}

	return resp.Body, nil
}

type listBucketResult struct {
	Contents []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
}

func (s *S3Client) List(ctx context.Context, prefix string) ([]FileInfo, error) {
	u, _ := url.Parse(s.endpoint)
	if !strings.Contains(u.Host, s.bucket+".") && s.bucket != "" {
		u.Path = "/" + s.bucket
	}
	q := u.Query()
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	s.signRequest(req, nil)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("S3 List 失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var res listBucketResult
	if err := xml.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	var list []FileInfo
	for _, c := range res.Contents {
		list = append(list, FileInfo{
			Path:         c.Key,
			Size:         c.Size,
			LastModified: c.LastModified,
		})
	}

	return list, nil
}

func (s *S3Client) Delete(ctx context.Context, remotePath string) error {
	reqURL := s.getURL(remotePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
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
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("S3 Delete 失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	return nil
}

// AWS V4 Signer implementation
func (s *S3Client) signRequest(req *http.Request, payload []byte) {
	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	payloadHash := sha256Hex(payload)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	// Canonical headers
	headers := map[string]string{
		"host":                 req.URL.Host,
		"x-amz-date":           amzDate,
		"x-amz-content-sha256": payloadHash,
	}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		headers["content-type"] = ct
	}

	var headerKeys []string
	for k := range headers {
		headerKeys = append(headerKeys, k)
	}
	sort.Strings(headerKeys)

	var canonicalHeaders strings.Builder
	for _, k := range headerKeys {
		canonicalHeaders.WriteString(fmt.Sprintf("%s:%s\n", k, headers[k]))
	}
	signedHeaders := strings.Join(headerKeys, ";")

	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		req.Method,
		req.URL.Path,
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	)

	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, s.region)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	)

	signingKey := getSignatureKey(s.secretKey, dateStamp, s.region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey,
		credentialScope,
		signedHeaders,
		signature,
	)

	req.Header.Set("Authorization", authHeader)
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
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}
