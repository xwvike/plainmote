package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Config points the client at a bucket. R2 speaks the S3 API, which is the
// interface Cloudflare documents for object traffic - its own REST API is for
// management and carries a global rate limit that data would blow through.
type S3Config struct {
	Endpoint  string // e.g. https://<account>.r2.cloudflarestorage.com
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string // R2 ignores it but the signature needs one
}

func (c S3Config) validate() error {
	switch {
	case strings.TrimSpace(c.Endpoint) == "":
		return errors.New("blob endpoint must not be empty")
	case strings.TrimSpace(c.Bucket) == "":
		return errors.New("blob bucket must not be empty")
	case strings.TrimSpace(c.AccessKey) == "" || strings.TrimSpace(c.SecretKey) == "":
		return errors.New("blob credentials must not be empty")
	}
	return nil
}

// S3 stores objects in an S3-compatible bucket.
type S3 struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
}

func NewS3(cfg S3Config) (*S3, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 0,
		WriteBufferSize:       128 << 10,
		ReadBufferSize:        128 << 10,
	}
	client := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(strings.TrimRight(cfg.Endpoint, "/")),
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		HTTPClient:   &http.Client{Transport: transport},
		// R2 buckets are addressed by path, not by a host prefix.
		UsePathStyle: true,
	})
	return &S3{client: client, presign: s3.NewPresignClient(client), bucket: cfg.Bucket}, nil
}

// PresignPut issues a short-lived presigned PUT URL that binds the exact object
// key and Content-Length so browsers can upload directly to object storage.
func (s *S3) PresignPut(ctx context.Context, key string, size int64, ttl time.Duration) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", errors.New("blob key must not be empty")
	}
	if size <= 0 {
		return "", errors.New("blob size must be positive")
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign put: %w", err)
	}
	return req.URL, nil
}

// Head checks that key exists and returns its stored byte length.
func (s *S3) Head(ctx context.Context, key string) (int64, error) {
	if strings.TrimSpace(key) == "" {
		return 0, errors.New("blob key must not be empty")
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("head blob: %w", err)
	}
	if out.ContentLength == nil {
		return 0, nil
	}
	return *out.ContentLength, nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("blob key must not be empty")
	}
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   r,
	}
	if size >= 0 {
		// Given a length the SDK can send a plain request instead of falling
		// back to chunked transfer, which some S3 implementations dislike.
		input.ContentLength = aws.Int64(size)
	}
	if _, err := s.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("write blob: %w", err)
	}
	return nil
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	if strings.TrimSpace(key) == "" {
		return nil, 0, errors.New("blob key must not be empty")
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("open blob: %w", err)
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

func (s *S3) OpenRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, int64, error) {
	if strings.TrimSpace(key) == "" {
		return nil, 0, errors.New("blob key must not be empty")
	}
	if start < 0 || end < start {
		return nil, 0, errors.New("invalid blob byte range")
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("open blob range: %w", err)
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("blob key must not be empty")
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete blob: %w", err)
	}
	return nil
}

// isNotFound recognises a missing object across the shapes the API uses for
// it, so callers only ever see ErrNotFound.
func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var notFound *types.NotFound
	return errors.As(err, &notFound)
}

// Ping checks that the bucket answers, with one metadata request that reads
// no object.
func (s *S3) Ping(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	return err
}
