package blob

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// This test talks to a real S3-compatible server, so it runs only when
// BLOB_TEST_ENDPOINT is set. It exists because the fake cannot tell you that the
// client library still agrees with R2-style servers after an update.
//
//	docker compose --profile s3 up -d --wait rustfs
//	curl -s --aws-sigv4 aws:amz:auto:s3 --user rustfsadmin:rustfsadmin -X PUT http://127.0.0.1:19000/goevent-test-images
//	BLOB_TEST_ENDPOINT=127.0.0.1:19000 go test ./internal/blob/
//
// BLOB_TEST_ACCESS_KEY and BLOB_TEST_SECRET_KEY override the RustFS defaults, and
// BLOB_TEST_BUCKET the bucket name. The bucket must already exist and is used only
// for keys under "contract-test/".
func contractConfig(t *testing.T) S3Config {
	t.Helper()
	endpoint := os.Getenv("BLOB_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("BLOB_TEST_ENDPOINT is not set; no S3-compatible server to test against")
	}
	getenv := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	bucket := getenv("BLOB_TEST_BUCKET", "goevent-test-images")
	return S3Config{
		Endpoint:      endpoint,
		Bucket:        bucket,
		AccessKey:     getenv("BLOB_TEST_ACCESS_KEY", "rustfsadmin"),
		SecretKey:     getenv("BLOB_TEST_SECRET_KEY", "rustfsadmin"),
		Region:        "auto",
		UseTLS:        false,
		PublicBaseURL: "http://" + endpoint + "/" + bucket,
	}
}

func TestS3_RealServerRoundTrip(t *testing.T) {
	store, err := NewS3(contractConfig(t))
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	const key = "contract-test/pixel.png"
	png := []byte("\x89PNG\r\n\x1a\n contract test")

	u, err := store.Put(t.Context(), key, bytes.NewReader(png), int64(len(png)), "image/png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if u != store.URL(key) {
		t.Errorf("Put returned %q, URL(key) is %q", u, store.URL(key))
	}
	// A bucket created by the recipe above is private, so the object is read
	// back through the S3 API rather than the public URL.
	got, err := readObject(t, store.client, store.bucket, key)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, png) {
		t.Errorf("read back %q, want %q", got, png)
	}
	if err := store.Delete(t.Context(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete(t.Context(), key); err != nil {
		t.Errorf("Delete of a missing key = %v, want nil", err)
	}
}

func readObject(t *testing.T, c *s3.Client, bucket, key string) ([]byte, error) {
	t.Helper()
	out, err := c.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}
