package storage

// Note: Use requires a ~/.aws/credentials file
// https://docs.aws.amazon.com/sdk-for-go/v1/developer-guide/configuring-sdk.html#specifying-credentials

import (
	"bytes"
	"log"
	"strings"

	"github.com/TheSnook/polyester/proto/resource"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

type S3Storage struct {
	svc    *s3.S3
	bucket string
}

func newS3(path string) Storage {
	region, bucket, ok := strings.Cut(path, ":")
	if !ok {
		log.Fatalf(`S3 path %q does not have expected format "<region>:<bucket>".`, path)
	}
	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(region),
	}))
	svc := s3.New(sess)
	return &S3Storage{
		svc:    svc,
		bucket: bucket,
	}
}

func (s *S3Storage) Write(k string, r *resource.Resource) error {
	obj := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(k),
	}
	if r.Redirect != "" {
		obj.SetWebsiteRedirectLocation(r.Redirect)
	} else {
		obj.SetBody(bytes.NewReader(r.Content))
		obj.SetContentType(r.ContentType)
	}
	_, err := s.svc.PutObject(obj)
	return err
}

func (s *S3Storage) Close() {}

func init() {
	register("s3", newS3)
}
