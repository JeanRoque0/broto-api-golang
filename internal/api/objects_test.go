package api

import (
	"bytes"
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"io"
	"strings"
	"testing"
)

type fakeS3 struct {
	objects   map[string][]byte
	deleteErr error
}

func (f *fakeS3) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if aws.ToString(in.Bucket) != "private-photos" || !strings.HasPrefix(aws.ToString(in.Key), "photos/") || in.ServerSideEncryption != types.ServerSideEncryptionAes256 {
		return nil, errors.New("unsafe S3 write")
	}
	b, e := io.ReadAll(in.Body)
	f.objects[aws.ToString(in.Key)] = b
	return &s3.PutObjectOutput{}, e
}
func (f *fakeS3) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	b, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, errors.New("missing")
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b))}, nil
}
func (f *fakeS3) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	delete(f.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}
func TestS3AcrossReplicasAndDeletionRetry(t *testing.T) {
	f := newFrontendHarness(t)
	client := &fakeS3{objects: map[string][]byte{}}
	store := &s3Objects{client: client, bucket: "private-photos"}
	f.s.objects = store
	_, token := f.account()
	path := f.photo(token)
	replica := &Server{objects: &s3Objects{client: client, bucket: "private-photos"}}
	body, e := replica.openPhoto(context.Background(), path)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(body)
	body.Close()
	if !bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}) {
		t.Fatal("cross-replica image corrupted")
	}
	client.deleteErr = errors.New("S3 temporarily unavailable")
	f.call("DELETE", "/v1/photos", token, map[string]string{"path": path}, 500)
	var count int
	if e = f.s.DB.QueryRow(context.Background(), "select count(*) from file_deletions where path=$1", path).Scan(&count); e != nil || count != 1 {
		t.Fatalf("missing durable retry: %v %d", e, count)
	}
	client.deleteErr = nil
	if e = f.s.drainFiles(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(client.objects) != 0 {
		t.Fatal("retry left object behind")
	}
}
