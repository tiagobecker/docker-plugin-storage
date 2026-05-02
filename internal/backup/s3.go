package backup

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const multipartPartSize = 8 * 1024 * 1024

type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Prefix          string
	PathStyle       bool
}

func LoadS3FromEnv() S3Config {
	return S3Config{
		Endpoint:        getenv("DPS_S3_ENDPOINT", "https://s3.amazonaws.com"),
		Region:          getenv("DPS_S3_REGION", "us-east-1"),
		Bucket:          os.Getenv("DPS_S3_BUCKET"),
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
		Prefix:          os.Getenv("DPS_S3_PREFIX"),
		PathStyle:       getenv("DPS_S3_PATH_STYLE", "") == "true",
	}
}

type S3Client struct {
	cfg    S3Config
	client *http.Client
}

type completedPart struct {
	PartNumber int
	ETag       string
}

func NewS3Client(cfg S3Config) (*S3Client, error) {
	if cfg.Endpoint == "" || cfg.Region == "" || cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("s3 endpoint, region, bucket, access key, and secret key are required")
	}
	return &S3Client{cfg: cfg, client: http.DefaultClient}, nil
}

func (c *S3Client) PutObject(key string, body []byte) error {
	req, err := c.newRequest(http.MethodPut, key, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("s3 put failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func (c *S3Client) PutObjectFile(key, filePath string) (string, int64, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	body := io.TeeReader(f, h)
	req, err := c.newRequest(http.MethodPut, key, body, st.Size())
	if err != nil {
		return "", 0, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, fmt.Errorf("s3 put failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return hex.EncodeToString(h.Sum(nil)), st.Size(), nil
}

func (c *S3Client) PutObjectStreamMultipart(key string, src io.Reader) (string, int64, error) {
	uploadID, err := c.createMultipartUpload(key)
	if err != nil {
		return "", 0, err
	}
	abort := true
	defer func() {
		if abort {
			_ = c.abortMultipartUpload(key, uploadID)
		}
	}()

	h := sha256.New()
	var total int64
	parts := []completedPart{}
	buf := make([]byte, multipartPartSize)
	for partNumber := 1; ; partNumber++ {
		n, readErr := io.ReadFull(src, buf)
		if readErr == io.EOF {
			break
		}
		if readErr == io.ErrUnexpectedEOF {
			// Last short part.
		} else if readErr != nil {
			return "", 0, readErr
		}
		if n == 0 {
			break
		}
		part := make([]byte, n)
		copy(part, buf[:n])
		if _, err := h.Write(part); err != nil {
			return "", 0, err
		}
		total += int64(n)
		etag, err := c.uploadPart(key, uploadID, partNumber, part)
		if err != nil {
			return "", 0, err
		}
		parts = append(parts, completedPart{PartNumber: partNumber, ETag: etag})
		if readErr == io.ErrUnexpectedEOF {
			break
		}
	}
	if len(parts) == 0 {
		etag, err := c.uploadPart(key, uploadID, 1, []byte{})
		if err != nil {
			return "", 0, err
		}
		parts = append(parts, completedPart{PartNumber: 1, ETag: etag})
	}
	if err := c.completeMultipartUpload(key, uploadID, parts); err != nil {
		return "", 0, err
	}
	abort = false
	return hex.EncodeToString(h.Sum(nil)), total, nil
}

func (c *S3Client) GetObject(key string) ([]byte, error) {
	req, err := c.newRequest(http.MethodGet, key, nil, 0)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("s3 get failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return io.ReadAll(resp.Body)
}

func (c *S3Client) HashObject(key string) (string, int64, error) {
	req, err := c.newRequest(http.MethodGet, key, nil, 0)
	if err != nil {
		return "", 0, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, fmt.Errorf("s3 get failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	h := sha256.New()
	bytes, err := io.Copy(h, resp.Body)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), bytes, nil
}

func (c *S3Client) GetObjectToFile(key, filePath string) (string, int64, error) {
	req, err := c.newRequest(http.MethodGet, key, nil, 0)
	if err != nil {
		return "", 0, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, fmt.Errorf("s3 get failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return "", 0, err
	}
	tmp := filePath + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	bytes, copyErr := io.Copy(io.MultiWriter(out, h), resp.Body)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return "", 0, copyErr
	}
	if syncErr != nil {
		return "", 0, syncErr
	}
	if closeErr != nil {
		return "", 0, closeErr
	}
	if err := os.Rename(tmp, filePath); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), bytes, nil
}

func (c *S3Client) newRequest(method, key string, body io.Reader, size int64) (*http.Request, error) {
	u, err := url.Parse(c.cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	objectKey := path.Join(c.cfg.Prefix, key)
	if c.cfg.PathStyle {
		u.Path = path.Join(u.Path, c.cfg.Bucket, objectKey)
	} else {
		u.Host = c.cfg.Bucket + "." + u.Host
		u.Path = path.Join(u.Path, objectKey)
	}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}

	req, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	payloadHash := "UNSIGNED-PAYLOAD"
	req.Header.Set("host", req.URL.Host)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", now.Format("20060102T150405Z"))
	if c.cfg.SessionToken != "" {
		req.Header.Set("x-amz-security-token", c.cfg.SessionToken)
	}
	if size > 0 {
		req.ContentLength = size
	}
	c.sign(req, now, payloadHash)
	return req, nil
}

func (c *S3Client) createMultipartUpload(key string) (string, error) {
	req, err := c.newRequest(http.MethodPost, key, nil, 0)
	if err != nil {
		return "", err
	}
	req.URL.RawQuery = "uploads="
	c.signRequest(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("s3 multipart create failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.UploadID == "" {
		return "", fmt.Errorf("s3 multipart create returned empty upload id")
	}
	return out.UploadID, nil
}

func (c *S3Client) uploadPart(key, uploadID string, partNumber int, body []byte) (string, error) {
	req, err := c.newRequest(http.MethodPut, key, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("partNumber", fmt.Sprintf("%d", partNumber))
	q.Set("uploadId", uploadID)
	req.URL.RawQuery = q.Encode()
	c.signRequest(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("s3 multipart part failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("s3 multipart part returned empty etag")
	}
	return etag, nil
}

func (c *S3Client) completeMultipartUpload(key, uploadID string, parts []completedPart) error {
	var body struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		} `xml:"Part"`
	}
	for _, part := range parts {
		body.Parts = append(body.Parts, struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		}{PartNumber: part.PartNumber, ETag: part.ETag})
	}
	payload, err := xml.Marshal(body)
	if err != nil {
		return err
	}
	req, err := c.newRequest(http.MethodPost, key, bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("uploadId", uploadID)
	req.URL.RawQuery = q.Encode()
	c.signRequest(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("s3 multipart complete failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func (c *S3Client) abortMultipartUpload(key, uploadID string) error {
	req, err := c.newRequest(http.MethodDelete, key, nil, 0)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("uploadId", uploadID)
	req.URL.RawQuery = q.Encode()
	c.signRequest(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("s3 multipart abort failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func (c *S3Client) signRequest(req *http.Request) {
	now := time.Now().UTC()
	req.Header.Del("Authorization")
	req.Header.Set("x-amz-date", now.Format("20060102T150405Z"))
	c.sign(req, now, req.Header.Get("x-amz-content-sha256"))
}

func (c *S3Client) sign(req *http.Request, now time.Time, payloadHash string) {
	date := now.Format("20060102")
	scope := date + "/" + c.cfg.Region + "/s3/aws4_request"
	signedHeaders := signedHeaderNames(req.Header)
	canonicalHeaders := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		uriEncodePath(req.URL.EscapedPath()),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		req.Header.Get("x-amz-date"),
		scope,
		hex.EncodeToString(hash[:]),
	}, "\n")
	signingKey := hmacSHA256([]byte("AWS4"+c.cfg.SecretAccessKey), date)
	signingKey = hmacSHA256(signingKey, c.cfg.Region)
	signingKey = hmacSHA256(signingKey, "s3")
	signingKey = hmacSHA256(signingKey, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", c.cfg.AccessKeyID, scope, signedHeaders, signature))
}

func canonicalHeaders(req *http.Request) string {
	keys := []string{}
	for k := range req.Header {
		keys = append(keys, strings.ToLower(k))
	}
	keys = append(keys, "host")
	sort.Strings(keys)
	seen := map[string]bool{}
	var b strings.Builder
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		value := req.Header.Get(k)
		if k == "host" {
			value = req.URL.Host
		}
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(strings.Join(strings.Fields(value), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

func signedHeaderNames(h http.Header) string {
	keys := []string{"host"}
	for k := range h {
		keys = append(keys, strings.ToLower(k))
	}
	sort.Strings(keys)
	out := []string{}
	seen := map[string]bool{}
	for _, k := range keys {
		if !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	return strings.Join(out, ";")
}

func hmacSHA256(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))
	return mac.Sum(nil)
}

func uriEncodePath(p string) string {
	if p == "" {
		return "/"
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
