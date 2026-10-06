package blob

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 is Storage against any S3-compatible object store: Cloudflare R2, Google
// Cloud Storage in interoperability mode, or MinIO.
//
// The client is minio-go rather than the AWS SDK, and the reason is which
// backends this project actually targets. Since aws-sdk-go-v2's service/s3
// v1.73.0, every PutObject carries a CRC32 checksum by default, and that broke
// R2, GCS interop and older MinIO — the three stores here — while being correct
// for the one store this project is least likely to be pointed at. minio-go
// speaks the conservative subset all of them agree on, which is exactly the
// requirement.
type S3 struct {
	client *minio.Client
	bucket string

	// publicBase is the hostname images are actually served from — R2's
	// pub-*.r2.dev, a custom domain, or MinIO's own address in development. It is
	// configuration rather than something derived from the endpoint, because the
	// address a bucket is *written* through and the address it is *read* from are
	// routinely different, and only the operator knows the second one.
	publicBase string
}

// S3Config is what S3 needs. Every field is required; NewS3 says which are
// missing rather than failing at the first upload.
type S3Config struct {
	// Endpoint is host[:port], with no scheme — minio-go takes the scheme from
	// UseTLS. For R2 this is <account-id>.r2.cloudflarestorage.com.
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string

	// Region is "auto" for R2. GCS and MinIO ignore it; AWS would not.
	Region string
	UseTLS bool

	// PublicBaseURL is the origin, plus any path prefix, that objects are served
	// from. An object's URL is this joined to its key.
	PublicBaseURL string

	// PublicEndpoint is the host[:port] a *browser* reaches this bucket at, when
	// that differs from Endpoint. Used only for signing download URLs, and empty
	// means the two are the same — which they are in every real deployment.
	//
	// It exists for the same reason PublicBaseURL does, one layer down. A
	// presigned URL's signature covers the Host header, so a URL signed for the
	// address the server connects through cannot be rewritten to the address the
	// browser uses; it has to be *signed* for that one from the start. In compose
	// the server reaches MinIO at minio:9000 and a browser reaches it at
	// localhost:9000, and without this the redirect would point at a hostname only
	// the container can resolve.
	PublicEndpoint string
}

// NewS3 validates the configuration and returns Storage.
//
// It does not connect and does not check the bucket exists. A store whose object
// storage is misconfigured should still sell things — the catalog, cart, checkout
// and payment path do not touch this — so the failure belongs at the first upload,
// where exactly one operator sees it, and not at boot where it stops the shop.
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
	// minio-go's own error for it is opaque.
	if strings.Contains(cfg.Endpoint, "://") {
		return nil, fmt.Errorf("blob: endpoint %q must be host[:port] with no scheme; use BLOB_USE_TLS for the scheme", cfg.Endpoint)
	}
	base, err := url.Parse(cfg.PublicBaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("blob: public base URL %q must be an absolute URL", cfg.PublicBaseURL)
	}

	client, err := newMinioClient(cfg)
	if err != nil {
		return nil, err
	}

	return &S3{
		client:     client,
		bucket:     cfg.Bucket,
		publicBase: strings.TrimRight(cfg.PublicBaseURL, "/"),
	}, nil
}

// newMinioClient builds the client both the public and the private store use.
// Shared so that a change to how this project talks to a bucket — the region
// default, the credential type — cannot apply to images and not to downloads.
func newMinioClient(cfg S3Config) (*minio.Client, error) {
	region := cfg.Region
	if region == "" {
		// "auto" is what R2 wants and what GCS and MinIO ignore. AWS would not,
		// which is the one backend this project is least likely to be pointed at.
		region = "auto"
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseTLS,
		Region: region,
	})
	if err != nil {
		return nil, fmt.Errorf("blob: build client for %s: %w", cfg.Endpoint, err)
	}
	return client, nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (string, error) {
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{
		ContentType: contentType,
		// Immutable is honest because the key carries a random component: replacing
		// an image writes a new key, so a cached copy of this one can never be
		// stale.
		CacheControl: "public, max-age=31536000, immutable",
	})
	if err != nil {
		return "", fmt.Errorf("blob: put %s: %w", key, err)
	}
	return s.URL(key), nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("blob: delete %s: %w", key, err)
	}
	return nil
}

func (s *S3) URL(key string) string {
	return s.publicBase + "/" + key
}
