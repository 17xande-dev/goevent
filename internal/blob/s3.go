package blob

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3 is Storage against any S3-compatible object store: Cloudflare R2, Google
// Cloud Storage in interoperability mode, or a self-hosted server such as RustFS.
//
// The client is the AWS SDK for Go v2, configured for stores that are not AWS.
// Since service/s3 v1.73.0 every PutObject carries a CRC32 checksum by default,
// which R2, GCS interop and some self-hosted servers rejected. The SDK has since
// grown the switch that matters, and newS3Client sets it: checksums are
// calculated and validated only where an operation requires them. Path-style
// addressing is on because that is what a custom endpoint expects.
type S3 struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string

	// publicBase is the hostname images are actually served from — R2's
	// pub-*.r2.dev, a custom domain, or the object server's own address in
	// development. It is configuration rather than something derived from the
	// endpoint, because the address a bucket is *written* through and the address
	// it is *read* from are routinely different, and only the operator knows the
	// second one.
	publicBase string
}

// S3Config is what S3 needs. Every field is required; NewS3 says which are
// missing rather than failing at the first upload.
type S3Config struct {
	// Endpoint is host[:port], with no scheme — the scheme comes from UseTLS.
	// For R2 this is <account-id>.r2.cloudflarestorage.com.
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string

	// Region is "auto" for R2. GCS and RustFS ignore it; AWS would not.
	Region string
	UseTLS bool

	// PublicBaseURL is the origin, plus any path prefix, that objects are served
	// from. An object's URL is this joined to its key.
	PublicBaseURL string
}

// NewS3 validates the configuration and returns Storage.
//
// It does not connect and does not check the bucket exists. A deployment whose
// object storage is misconfigured should still take registrations — the event
// pages, registration and payment path do not touch this — so the failure belongs
// at the first upload, where exactly one operator sees it, and not at boot where it
// stops the site.
func NewS3(cfg S3Config) (*S3, error) {
	var missing []string
	for _, f := range []struct{ name, value string }{
		{"endpoint", cfg.Endpoint},
		{"bucket", cfg.Bucket},
		{"access key", cfg.AccessKey},
		{"secret key", cfg.SecretKey},
		{"public base URL", cfg.PublicBaseURL},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("blob: missing configuration: %s", strings.Join(missing, ", "))
	}

	// A scheme in the endpoint is the single easiest thing to get wrong here, and
	// the SDK's own error for it is opaque.
	if strings.Contains(cfg.Endpoint, "://") {
		return nil, fmt.Errorf("blob: endpoint %q must be host[:port] with no scheme; use BLOB_USE_TLS for the scheme", cfg.Endpoint)
	}
	base, err := url.Parse(cfg.PublicBaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("blob: public base URL %q must be an absolute URL", cfg.PublicBaseURL)
	}

	client, err := newS3Client(cfg)
	if err != nil {
		return nil, err
	}

	return &S3{
		client:     client,
		uploader:   manager.NewUploader(client),
		bucket:     cfg.Bucket,
		publicBase: strings.TrimRight(cfg.PublicBaseURL, "/"),
	}, nil
}

// newS3Client builds the bucket client NewS3 uses.
func newS3Client(cfg S3Config) (*s3.Client, error) {
	region := cfg.Region
	if region == "" {
		// "auto" is what R2 wants and what GCS and RustFS ignore. AWS would not,
		// which is the one backend this project is least likely to be pointed at.
		region = "auto"
	}
	scheme := "https"
	if !cfg.UseTLS {
		scheme = "http"
	}
	if strings.Contains(cfg.Endpoint, "://") {
		return nil, fmt.Errorf("blob: endpoint %q must be host[:port] with no scheme", cfg.Endpoint)
	}
	return s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(scheme + "://" + cfg.Endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		UsePathStyle: true,

		// See the comment on S3: the default CRC32 checksums are what broke
		// non-AWS stores.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}), nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (string, error) {
	// The uploader copes with a reader that is not seekable, which is what an
	// uploaded multipart file is, and which a plain PutObject over HTTP is not.
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        r,
		ContentType: aws.String(contentType),
		// Immutable is honest because the key carries a random component: replacing
		// an image writes a new key, so a cached copy of this one can never be
		// stale.
		CacheControl: aws.String("public, max-age=31536000, immutable"),
	})
	if err != nil {
		return "", fmt.Errorf("blob: put %s: %w", key, err)
	}
	return s.URL(key), nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("blob: delete %s: %w", key, err)
	}
	return nil
}

func (s *S3) URL(key string) string {
	return s.publicBase + "/" + key
}
