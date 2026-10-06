package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrNotConfigured is what Unconfigured returns. It is a distinct error so a
// handler can tell "object storage is switched off" — an operator's message —
// apart from "the upload failed", which is a server fault.
var ErrNotConfigured = errors.New("blob: object storage is not configured")

// Unconfigured is the Storage for a deployment with no object storage. Every
// method refuses.
//
// Refusing is right here, unlike email.Discard which reports success: an upload is
// a thing an operator is doing right now and watching the result of, so it should
// fail visibly and say why. A dropped receipt has nobody looking at it, which is
// why that one is logged instead.
type Unconfigured struct{}

func (Unconfigured) Put(context.Context, string, io.Reader, int64, string) (string, error) {
	return "", ErrNotConfigured
}

func (Unconfigured) Delete(context.Context, string) error { return ErrNotConfigured }

func (Unconfigured) URL(string) string { return "" }

// Fake keeps objects in memory. It backs the handler tests, so an upload can be
// exercised end to end — including that the bytes stored are the bytes sent, and
// that a replaced image's old object is actually deleted — without MinIO running.
type Fake struct {
	// Err, when set, is returned by Put and Delete, standing in for a bucket that
	// is unreachable or credentials that have been revoked.
	Err error

	// Base is the public base URL the fake claims objects are served from.
	Base string

	mu      sync.Mutex
	objects map[string]Object
	deleted []string
}

// Object is one stored object.
type Object struct {
	Body        []byte
	ContentType string
}

func NewFake() *Fake {
	return &Fake{Base: "https://images.example", objects: map[string]Object{}}
}

func (f *Fake) Put(_ context.Context, key string, r io.Reader, _ int64, contentType string) (string, error) {
	if f.Err != nil {
		return "", f.Err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("blob: fake put %s: %w", key, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = Object{Body: body, ContentType: contentType}
	return f.url(key), nil
}

func (f *Fake) Delete(_ context.Context, key string) error {
	if f.Err != nil {
		return f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	// Recorded separately from the map, so a test can assert a *particular* object
	// was deleted rather than merely that it is absent — which it would also be if
	// it had never been stored.
	f.deleted = append(f.deleted, key)
	return nil
}

func (f *Fake) URL(key string) string { return f.url(key) }

func (f *Fake) url(key string) string {
	base := f.Base
	if base == "" {
		base = "https://images.example"
	}
	return base + "/" + key
}

// Get returns a stored object.
func (f *Fake) Get(key string) (Object, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	return o, ok
}

// Keys returns every stored key.
func (f *Fake) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	return keys
}

// Deleted returns the keys Delete was called with, in order.
func (f *Fake) Deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}
