package cloud

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jun9100/savevault/internal/store"
)

// s3Provider stores snapshots in any S3-compatible object store (AWS S3,
// Cloudflare R2, MinIO, Backblaze B2, Aliyun OSS) signed with AWS Signature
// Version 4 (AWS4-HMAC-SHA256) using only the Go standard library.
type s3Provider struct {
	s   *Service
	cfg store.CloudConfig
}

func (p s3Provider) credentials() (accessKey, secretKey string, err error) {
	accessKey = strings.TrimSpace(p.cfg.Username)
	secretKey = strings.TrimSpace(p.cfg.Password)
	if accessKey == "" || secretKey == "" {
		return "", "", fmt.Errorf("no S3 / R2 access key configured")
	}
	return accessKey, secretKey, nil
}

func (p s3Provider) region() string {
	if r := strings.TrimSpace(p.cfg.S3Region); r != "" {
		return r
	}
	return "auto"
}

func (p s3Provider) prefix() string {
	trimmed := strings.Trim(strings.TrimSpace(p.cfg.S3Prefix), "/")
	if trimmed == "" {
		return ""
	}
	return trimmed + "/"
}

func (p s3Provider) usePathStyle(parsed *url.URL) bool {
	if p.cfg.S3PathStyle {
		return true
	}
	host := parsed.Hostname()
	return host == "localhost" || net.ParseIP(host) != nil
}

func normalizeSigV4Host(u *url.URL) string {
	host := strings.ToLower(u.Host)
	if u.Scheme == "https" && strings.HasSuffix(host, ":443") {
		return strings.TrimSuffix(host, ":443")
	}
	if u.Scheme == "http" && strings.HasSuffix(host, ":80") {
		return strings.TrimSuffix(host, ":80")
	}
	return host
}

func (p s3Provider) endpointURL(objectKey string, query url.Values) (*url.URL, error) {
	rawBase := strings.TrimSpace(p.cfg.URL)
	bucket := strings.Trim(strings.TrimSpace(p.cfg.S3Bucket), "/")
	if rawBase == "" || bucket == "" {
		return nil, fmt.Errorf("no S3 / R2 endpoint and bucket configured")
	}
	parsed, err := url.Parse(rawBase)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid S3 / R2 endpoint URL")
	}

	u := *parsed
	basePath := strings.TrimSuffix(u.Path, "/")
	key := strings.TrimPrefix(objectKey, "/")
	if p.usePathStyle(parsed) {
		if key == "" {
			u.Path = basePath + "/" + bucket
		} else {
			u.Path = basePath + "/" + bucket + "/" + key
		}
	} else {
		u.Host = bucket + "." + parsed.Host
		if key == "" {
			u.Path = basePath + "/"
		} else {
			u.Path = basePath + "/" + key
		}
	}
	// Ensure wire path & query match SigV4 canonical encoding exactly.
	u.RawPath = awsEncodePath(u.Path)
	u.RawQuery = awsCanonicalQuery(query)
	return &u, nil
}

func (p s3Provider) upload(f *os.File, size int64, fileName string) error {
	accessKey, secretKey, err := p.credentials()
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("S3: hash upload payload: %w", err)
	}
	payloadHash := hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	u, err := p.endpointURL(p.prefix()+fileName, nil)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, u.String(), f)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/zip")
	p.signV4(req, accessKey, secretKey, payloadHash, time.Now().UTC())

	resp, err := p.s.doTransfer(req)
	if err != nil {
		return fmt.Errorf("S3: %w", err)
	}
	defer resp.Body.Close()
	if err := transferOK(resp); err != nil {
		return fmt.Errorf("S3 upload: %w", err)
	}
	return nil
}

func (p s3Provider) list() ([]CloudFile, error) {
	accessKey, secretKey, err := p.credentials()
	if err != nil {
		return nil, err
	}
	prefix := p.prefix()
	var files []CloudFile
	token := ""
	for page := 0; page < maxListPages; page++ {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("delimiter", "/")
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		u, err := p.endpointURL("", q)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest(http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		p.signV4(req, accessKey, secretKey, emptySHA256Hex, time.Now().UTC())

		raw, err := p.s.fetchBytes(req)
		if err != nil {
			return nil, fmt.Errorf("S3 list: %w", err)
		}
		var result struct {
			Contents []struct {
				Key          string `xml:"Key"`
				LastModified string `xml:"LastModified"`
				Size         int64  `xml:"Size"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("S3 list parse xml: %w", err)
		}
		for _, item := range result.Contents {
			rel := strings.TrimPrefix(item.Key, prefix)
			if rel == "" || strings.Contains(rel, "/") || !strings.HasSuffix(strings.ToLower(rel), ".zip") {
				continue
			}
			created := strings.TrimSpace(item.LastModified)
			if t, parseErr := time.Parse(time.RFC3339Nano, created); parseErr == nil {
				created = t.UTC().Format(time.RFC3339)
			}
			files = append(files, CloudFile{
				ID:          item.Key,
				Name:        rel,
				SizeBytes:   item.Size,
				CreatedTime: created,
			})
		}
		if !result.IsTruncated {
			return files, nil
		}
		if result.NextContinuationToken == "" || result.NextContinuationToken == token {
			return nil, fmt.Errorf("S3 list: truncated response missing valid continuation token")
		}
		token = result.NextContinuationToken
	}
	return nil, errListTooLong("S3")
}

func (p s3Provider) download(fileName, localPath string) error {
	accessKey, secretKey, err := p.credentials()
	if err != nil {
		return err
	}
	u, err := p.endpointURL(p.prefix()+fileName, nil)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	p.signV4(req, accessKey, secretKey, emptySHA256Hex, time.Now().UTC())
	if err := p.s.fetchToFile(req, localPath); err != nil {
		return fmt.Errorf("S3: %w", err)
	}
	return nil
}

func (p s3Provider) remove(f CloudFile) error {
	accessKey, secretKey, err := p.credentials()
	if err != nil {
		return err
	}
	key := f.ID
	if key == "" {
		key = p.prefix() + f.Name
	}
	u, err := p.endpointURL(key, nil)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodDelete, u.String(), nil)
	if err != nil {
		return err
	}
	p.signV4(req, accessKey, secretKey, emptySHA256Hex, time.Now().UTC())
	if err := p.s.doOK(req); err != nil {
		return fmt.Errorf("S3 delete: %w", err)
	}
	return nil
}

const emptySHA256Hex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// signV4 signs an HTTP request with AWS Signature Version 4 (AWS4-HMAC-SHA256).
func (p s3Provider) signV4(req *http.Request, accessKey, secretKey, payloadHash string, now time.Time) {
	region := p.region()
	const service = "s3"

	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	host := normalizeSigV4Host(req.URL)
	req.Host = host
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	canonicalURI := awsEncodePath(req.URL.Path)
	canonicalQuery := req.URL.RawQuery
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", host, payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	reqHash := sha256.Sum256([]byte(canonicalRequest))
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, region, service)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(reqHash[:]),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature,
	))
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func awsURIEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func awsEncodePath(p string) string {
	if p == "" {
		return "/"
	}
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = awsURIEncode(seg)
	}
	out := strings.Join(parts, "/")
	if !strings.HasPrefix(out, "/") {
		out = "/" + out
	}
	return out
}

func awsCanonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		ek := awsURIEncode(k)
		for _, v := range vals {
			pairs = append(pairs, ek+"="+awsURIEncode(v))
		}
	}
	return strings.Join(pairs, "&")
}
