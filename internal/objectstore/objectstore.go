// Package objectstore is the Control Plane's S3-compatible object store client
// (docs/spec/defaults.md "对象存储布局", ADR-0021). Nodes never touch it
// directly: they receive presigned URLs through the Presign RPC.
package objectstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// PresignTTL is the presigned URL lifetime (defaults.md, 1 hour, constant).
const PresignTTL = time.Hour

// ErrNotFound means the object is not in the store.
var ErrNotFound = errors.New("object not found")

// Config is the deployment's object store configuration. Endpoint is what the
// Control Plane reaches; PublicEndpoint is what Nodes reach, because presigned
// URLs are signed for the Node-facing address.
type Config struct {
	Endpoint       string
	PublicEndpoint string
	Bucket         string
	Region         string
	AccessKey      string
	SecretKey      string
}

// Store holds two clients over the same credentials: one for Control Plane
// operations on the internal endpoint, one for signing Node-reachable URLs.
type Store struct {
	bucket  string
	ops     *s3.Client
	presign *s3.PresignClient
}

// New builds a Store. A zero Config is not a valid deployment configuration;
// callers that treat object storage as optional check for an empty endpoint
// before calling.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("object store endpoint and bucket are required")
	}
	if cfg.PublicEndpoint == "" {
		cfg.PublicEndpoint = cfg.Endpoint
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load object store configuration: %w", err)
	}
	client := func(endpoint string) *s3.Client {
		return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		})
	}
	return &Store{
		bucket:  cfg.Bucket,
		ops:     client(cfg.Endpoint),
		presign: s3.NewPresignClient(client(cfg.PublicEndpoint)),
	}, nil
}

// CASKey is the owner-scoped content address (ADR-0021).
func CASKey(ownerID, sha256hex string) string {
	return "cas/" + ownerID + "/sha256/" + sha256hex
}

// ManifestKey is where a ResourceVersion manifest is stored.
func ManifestKey(ownerID, address string) string {
	return "manifests/" + ownerID + "/" + address
}

// PresignPutURL signs an upload with the declared checksum, so the store
// rejects bytes that do not match it (ADR-0021).
func (s *Store) PresignPutURL(ctx context.Context, ownerID, sha256hex string) (string, error) {
	raw, err := hex.DecodeString(sha256hex)
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("sha256 must be 64 hex characters")
	}
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:         aws.String(s.bucket),
		Key:            aws.String(CASKey(ownerID, sha256hex)),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(raw)),
	}, s3.WithPresignExpires(PresignTTL))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// PresignGetURL signs a download of a CAS object.
func (s *Store) PresignGetURL(ctx context.Context, ownerID, sha256hex string) (string, error) {
	return s.presignGet(ctx, CASKey(ownerID, sha256hex))
}

// PresignManifestGetURL signs a download of a version manifest.
func (s *Store) PresignManifestGetURL(ctx context.Context, ownerID, address string) (string, error) {
	return s.presignGet(ctx, ManifestKey(ownerID, address))
}

// DeleteManifest removes a version's manifest. A missing object is success:
// the sweep is retryable and only needs the end state.
func (s *Store) DeleteManifest(ctx context.Context, ownerID, address string) error {
	return s.delete(ctx, ManifestKey(ownerID, address))
}

// DeleteCASObject removes one owner-scoped object. The GC deletes the object
// before its row, so a crash in between retries cleanly (ADR-0012).
func (s *Store) DeleteCASObject(ctx context.Context, ownerID, sha256hex string) error {
	return s.delete(ctx, CASKey(ownerID, sha256hex))
}

func (s *Store) delete(ctx context.Context, key string) error {
	_, err := s.ops.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}

// PutManifest stores a version's canonical manifest bytes. The Control Plane
// writes these when a version is published; clients read them through the API.
func (s *Store) PutManifest(ctx context.Context, ownerID, address string, data []byte) error {
	_, err := s.ops.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(ManifestKey(ownerID, address)),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}

// GetManifest reads a version's stored canonical manifest bytes.
func (s *Store) GetManifest(ctx context.Context, ownerID, address string) ([]byte, error) {
	out, err := s.ops.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(ManifestKey(ownerID, address)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (s *Store) presignGet(ctx context.Context, key string) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(PresignTTL))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// HeadSHA256 reads an object's checksum in checksum mode. It returns the
// lowercase hex digest, or ErrNotFound.
func (s *Store) HeadSHA256(ctx context.Context, ownerID, sha256hex string) (string, error) {
	out, err := s.ops.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(CASKey(ownerID, sha256hex)),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		if isNotFound(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	if out.ChecksumSHA256 == nil {
		return "", fmt.Errorf("object has no SHA-256 checksum")
	}
	raw, err := base64.StdEncoding.DecodeString(*out.ChecksumSHA256)
	if err != nil {
		return "", fmt.Errorf("decode object checksum: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func isNotFound(err error) bool {
	var notFound *types.NotFound
	var apiErr smithy.APIError
	return errors.As(err, &notFound) ||
		(errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey"))
}
