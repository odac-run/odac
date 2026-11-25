package engine

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/blob/s3"
	"github.com/kopia/kopia/repo/blob/webdav"
)

// Storage names where the repository's blobs live. Exactly one member is
// set. A local disk, SMB share or NFS export all arrive as a Filesystem
// path, because the helper container sees them as a mounted directory.
type Storage struct {
	Filesystem *FilesystemStorage `json:"filesystem,omitempty"`
	S3         *S3Storage         `json:"s3,omitempty"`
	WebDAV     *WebDAVStorage     `json:"webdav,omitempty"`
}

// FilesystemStorage is a directory the repository is written into.
type FilesystemStorage struct {
	Path string `json:"path"`
}

// S3Storage is any S3-compatible bucket (AWS, B2, R2, Wasabi, MinIO).
type S3Storage struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region,omitempty"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix,omitempty"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken,omitempty"`
	// DisableTLS is for a MinIO on a private network. Certificate checks
	// cannot be switched off: a target that fails them is not the target.
	DisableTLS bool `json:"disableTLS,omitempty"`
}

// WebDAVStorage is a WebDAV collection.
type WebDAVStorage struct {
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func (s Storage) validate() error {
	n := 0
	if s.Filesystem != nil {
		n++
		if !filepath.IsAbs(s.Filesystem.Path) {
			return errors.New("engine: filesystem storage needs an absolute path")
		}
	}
	if s.S3 != nil {
		n++
		if s.S3.Endpoint == "" || s.S3.Bucket == "" {
			return errors.New("engine: s3 storage needs an endpoint and a bucket")
		}
	}
	if s.WebDAV != nil {
		n++
		u, err := url.Parse(s.WebDAV.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("engine: webdav storage needs an http(s) url")
		}
		// Credentials go in their own fields, which are masked; inside the
		// URL they would reach every log line that names the target.
		if u.User != nil {
			return errors.New("engine: webdav url must not carry credentials")
		}
	}
	if n != 1 {
		return errors.New("engine: exactly one storage kind must be set")
	}
	return nil
}

func (s Storage) connect(ctx context.Context, create bool) (blob.Storage, error) {
	var (
		st  blob.Storage
		err error
	)
	switch {
	case s.Filesystem != nil:
		if !create {
			if _, serr := os.Stat(s.Filesystem.Path); errors.Is(serr, os.ErrNotExist) {
				return nil, ErrNotInitialized
			}
		}
		st, err = filesystem.New(ctx, &filesystem.Options{
			Path:          s.Filesystem.Path,
			FileMode:      0o600,
			DirectoryMode: 0o700,
		}, create)
	case s.S3 != nil:
		st, err = s3.New(ctx, &s3.Options{
			Endpoint:        s.S3.Endpoint,
			Region:          s.S3.Region,
			BucketName:      s.S3.Bucket,
			Prefix:          s.S3.Prefix,
			AccessKeyID:     s.S3.AccessKeyID,
			SecretAccessKey: s.S3.SecretAccessKey,
			SessionToken:    s.S3.SessionToken,
			DoNotUseTLS:     s.S3.DisableTLS,
		}, create)
	case s.WebDAV != nil:
		st, err = webdav.New(ctx, &webdav.Options{
			URL:      s.WebDAV.URL,
			Username: s.WebDAV.Username,
			Password: s.WebDAV.Password,
			// false makes Kopia write each blob to a temporary name and
			// move it into place, so a crash never leaves a short blob.
			AtomicWrites: false,
		}, create)
	}
	if err != nil {
		return nil, fmt.Errorf("engine: storage: %w", err)
	}
	return st, nil
}
