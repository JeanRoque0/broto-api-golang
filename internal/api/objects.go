package api

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type objectStore interface {
	Put(context.Context, string, []byte, string) error
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
}
type s3API interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}
type s3Objects struct {
	client s3API
	bucket string
}

func newS3Objects(ctx context.Context, c Config) (*s3Objects, error) {
	config, e := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.AWSRegion), awsconfig.WithRetryMaxAttempts(2))
	if e != nil {
		return nil, e
	}
	return &s3Objects{client: s3.NewFromConfig(config), bucket: c.StorageBucket}, nil
}
func (s *s3Objects) Put(ctx context.Context, path string, b []byte, mime string) error {
	_, e := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String("photos/" + path), Body: bytes.NewReader(b), ContentType: aws.String(mime), ServerSideEncryption: types.ServerSideEncryptionAes256})
	return e
}
func (s *s3Objects) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	v, e := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String("photos/" + path)})
	if e != nil {
		return nil, e
	}
	return v.Body, nil
}
func (s *s3Objects) Delete(ctx context.Context, path string) error {
	_, e := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String("photos/" + path)})
	return e
}
func (s *Server) putPhoto(ctx context.Context, path string, b []byte, mime string) error {
	if s.objects != nil {
		return s.objects.Put(ctx, path, b, mime)
	}
	if e := os.MkdirAll(filepath.Dir(s.filePath(path)), 0700); e != nil {
		return e
	}
	return os.WriteFile(s.filePath(path), b, 0600)
}
func (s *Server) openPhoto(ctx context.Context, path string) (io.ReadCloser, error) {
	if s.objects != nil {
		return s.objects.Open(ctx, path)
	}
	return os.Open(s.filePath(path))
}
func (s *Server) removePhoto(ctx context.Context, path string) error {
	if s.objects != nil {
		return s.objects.Delete(ctx, path)
	}
	e := os.Remove(s.filePath(path))
	if os.IsNotExist(e) {
		return nil
	}
	return e
}
func (s *Server) discardPhoto(path string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := s.removePhoto(ctx, path); e != nil {
		// Persist retry when the object store is temporarily unavailable.
		retry, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = s.DB.Exec(retry, "insert into file_deletions(path) values($1) on conflict do nothing", path)
	}
}
