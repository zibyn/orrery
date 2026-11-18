package resourcefs

// The Volume target of docs/research/volume-bench (see its README); skipped
// unless ORRERY_BENCH_SCRIPT is set. The Volume reads and commits against a
// real S3 store (RustFS) the way the Agent does: seal → upload the pending
// objects through presigned PUTs, castransfer.MaxUploads in flight →
// PUT the Generation manifest → VolumeCommitted.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sync/errgroup"

	"github.com/zibyn/orrery/internal/castransfer"
	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
	"github.com/zibyn/orrery/internal/objectstore"
)

const benchOwner = "0198c7a2-0000-7000-8000-00000000bead"

type s3Presigner struct{ s *objectstore.Store }

func (p s3Presigner) Presign(ctx context.Context, objects, manifests []string) ([]string, error) {
	var urls []string
	for _, d := range objects {
		u, err := p.s.PresignGetURL(ctx, benchOwner, d)
		if err != nil {
			return nil, err
		}
		urls = append(urls, u)
	}
	for _, a := range manifests {
		u, err := p.s.PresignManifestGetURL(ctx, benchOwner, a)
		if err != nil {
			return nil, err
		}
		urls = append(urls, u)
	}
	return urls, nil
}

func (p s3Presigner) PresignGenerations(ctx context.Context, ids []string) ([]string, error) {
	var urls []string
	for _, id := range ids {
		u, err := p.s.PresignGenerationGetURL(ctx, benchOwner, id)
		if err != nil {
			return nil, err
		}
		urls = append(urls, u)
	}
	return urls, nil
}

func (p s3Presigner) putObject(ctx context.Context, path, digest string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close() //nolint:errcheck
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	u, err := p.s.PresignPutURL(ctx, benchOwner, digest)
	if err != nil {
		return 0, err
	}
	return info.Size(), castransfer.Put(ctx, http.DefaultClient, u, f, info.Size(), digest)
}

func (p s3Presigner) putGeneration(ctx context.Context, id string, data []byte) error {
	u, err := p.s.PresignGenerationPutURL(ctx, benchOwner, id)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close() //nolint:errcheck
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("PUT generation: %s", res.Status)
	}
	return nil
}

func TestVolumeLocalBench(t *testing.T) {
	script := os.Getenv("ORRERY_BENCH_SCRIPT")
	if script == "" {
		t.Skip("ORRERY_BENCH_SCRIPT unset")
	}
	requireFUSE(t)
	every, _ := time.ParseDuration(os.Getenv("ORRERY_BENCH_COMMIT"))
	ctx := context.Background()
	endpoint, bucket := os.Getenv("ORRERY_BENCH_S3"), fmt.Sprintf("vol%d", time.Now().Unix())
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("bench", "benchbench", "")))
	if err != nil {
		t.Fatal(err)
	}
	raw := s3.NewFromConfig(awsCfg, func(o *s3.Options) { o.BaseEndpoint, o.UsePathStyle = aws.String(endpoint), true })
	if _, err := raw.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	store, err := objectstore.New(ctx, objectstore.Config{Endpoint: endpoint, Bucket: bucket, AccessKey: "bench", SecretKey: "benchbench"})
	if err != nil {
		t.Fatal(err)
	}
	p := s3Presigner{store}

	root := t.TempDir()
	h := &harness{t: t, root: root, socket: filepath.Join(root, "control.sock")}
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() { _ = Run(runCtx, Config{DataRoot: root, ControlSocket: h.socket, Presigner: p, Log: testLog()}) }()
	h.waitServing()

	// The head the Volume mounts: one small file, in RustFS like any other.
	seed := filepath.Join(root, "seed")
	if err := os.WriteFile(seed, []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedDigest := digest([]byte("seed"))
	if _, err := p.putObject(ctx, seed, seedDigest); err != nil {
		t.Fatal(err)
	}
	size := int64(4)
	head, err := generation.Encode(generation.Manifest{Kind: generation.KindTree, Entries: []generation.Entry{
		{Entry: manifest.Entry{Path: "seed", Type: manifest.TypeFile, Size: &size, SHA256: seedDigest}, Mode: 0o644},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.putGeneration(ctx, "head-1", head); err != nil {
		t.Fatal(err)
	}
	if err := h.client().MountVolume(ctx, testVolume, "head-1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { detach(h.volumeDir()) })
	objects := filepath.Join(root, "volumes", testVolume, "objects")

	commit := func(i int) error {
		start := time.Now()
		s, err := h.client().SealVolume(ctx, testVolume)
		if err != nil {
			return fmt.Errorf("seal: %w", err)
		}
		sealed := time.Since(start)
		if !s.Dirty || s.Manifest == nil {
			return nil
		}
		var bytesUp atomic.Int64
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(castransfer.MaxUploads)
		for _, d := range s.Pending {
			g.Go(func() error {
				n, err := p.putObject(gctx, filepath.Join(objects, d), d)
				if err != nil {
					return fmt.Errorf("upload %s: %w", d, err)
				}
				bytesUp.Add(n)
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
		encoded, err := generation.Encode(*s.Manifest)
		if err != nil {
			return err
		}
		id := fmt.Sprintf("gen-%d", i)
		if err := p.putGeneration(ctx, id, encoded); err != nil {
			return err
		}
		if err := h.client().VolumeCommitted(ctx, testVolume, id, s.Seq); err != nil {
			return fmt.Errorf("committed: %w", err)
		}
		fmt.Printf("COMMIT seal=%v objects=%d MiB=%d total=%v\n", sealed.Round(time.Millisecond),
			len(s.Pending), bytesUp.Load()>>20, time.Since(start).Round(time.Millisecond))
		return nil
	}

	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		if every == 0 {
			return
		}
		for i := 2; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(every):
			}
			if err := commit(i); err != nil {
				t.Errorf("commit: %v", err)
				return
			}
		}
	}()
	cmd := exec.Command("bash", script, h.volumeDir())
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
}
