package lsvd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/hashicorp/go-hclog"
	"github.com/oklog/ulid/v2"
	"github.com/pierrec/lz4/v4"
)

type S3Access struct {
	sc       *s3.Client
	uploader *manager.Uploader
	bucket   string
}

func NewS3Access(log hclog.Logger, host, bucket string, cfg aws.Config) (*S3Access, error) {
	sc := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.BaseEndpoint = &host
	})

	up := manager.NewUploader(sc)
	return &S3Access{
		sc:       sc,
		bucket:   bucket,
		uploader: up,
	}, nil
}

type S3ObjectReader struct {
	ctx context.Context
	sc  *s3.Client
	buk string
	key string
	seg SegmentId
}

func (s *S3ObjectReader) Close() error {
	return nil
}

func (s *S3ObjectReader) ReadAt(dest []byte, off int64) (int, error) {
	rng := fmt.Sprintf("bytes=%d-%d", off, int(off)+len(dest)-1)

	r, err := s.sc.GetObject(s.ctx, &s3.GetObjectInput{
		Bucket: &s.buk,
		Key:    &s.key,
		Range:  &rng,
	})
	if err != nil {
		return 0, err
	}

	defer r.Body.Close()

	n, err := io.ReadFull(r.Body, dest)
	if err != nil {
		if n > 0 {
			return n, nil
		}
	}

	return n, err
}

func (s *S3ObjectReader) ReadAtCompressed(dest []byte, off, compSize int64) (int, error) {
	buf := make([]byte, compSize)

	_, err := s.ReadAt(buf, off)
	if err != nil {
		return 0, err
	}

	sz, err := lz4.UncompressBlock(buf, dest)
	if err != nil {
		return 0, err
	}

	if sz != BlockSize {
		return 0, fmt.Errorf("compressed block uncompressed wrong size (%d != %d)", sz, BlockSize)
	}

	return len(dest), nil
}

func (s *S3Access) OpenSegment(ctx context.Context, seg SegmentId) (ObjectReader, error) {
	key := "objects/object." + ulid.ULID(seg).String()

	// Validate the object exists.
	_, err := s.sc.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, err
	}

	return &S3ObjectReader{
		sc:  s.sc,
		ctx: ctx,
		seg: seg,
		buk: s.bucket,
		key: key,
	}, nil
}

func (s *S3Access) ListSegments(ctx context.Context, vol string) ([]SegmentId, error) {
	name := filepath.Join("volumes", vol, "objects")

	out, err := s.sc.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &name,
	})
	if err != nil {
		if s.isNoSuchKey(err) {
			return nil, nil
		}
		return nil, err
	}

	defer out.Body.Close()

	return ReadSegments(out.Body)
}

type mdWriter struct {
	ctx    context.Context
	sc     *manager.Uploader
	bucket string
	key    string

	buf bytes.Buffer
}

func (m *mdWriter) Write(b []byte) (int, error) {
	return m.buf.Write(b)
}

func (m *mdWriter) Close() error {
	_, err := m.sc.Upload(m.ctx, &s3.PutObjectInput{
		Bucket: &m.bucket,
		Key:    &m.key,
		Body:   &m.buf,
	})

	return err
}

type bgWriter struct {
	io.Writer

	bw  *bufio.Writer
	w   *io.PipeWriter
	ctx context.Context
	err error

	sc     *manager.Uploader
	bucket string
	key    string
}

func (b *bgWriter) Close() error {
	b.bw.Flush()
	b.w.Close()

	<-b.ctx.Done()

	return b.err
}

func (s *S3Access) WriteSegment(ctx context.Context, seg SegmentId) (io.WriteCloser, error) {
	r, w := io.Pipe()

	bw := bufio.NewWriter(w)

	ctx, cancel := context.WithCancel(ctx)

	bg := &bgWriter{
		Writer: bw,
		bw:     bw,
		w:      w,
		ctx:    ctx,
	}

	key := "objects/object." + ulid.ULID(seg).String()

	go func() {
		defer cancel()
		_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: &s.bucket,
			Key:    &key,
			Body:   r,
		})
		bg.err = err
	}()

	return bg, nil
}

func (s *S3Access) WriteMetadata(ctx context.Context, volName, name string) (io.WriteCloser, error) {
	var mw mdWriter
	mw.ctx = ctx
	mw.sc = s.uploader
	mw.bucket = s.bucket
	mw.key = filepath.Join("volumes", volName, name)

	return &mw, nil
}

func (s *S3Access) isNoSuchKey(err error) bool {
	var serr smithy.APIError
	return errors.As(err, &serr) && serr.ErrorCode() == "NoSuchKey"
}

func (s *S3Access) ReadMetadata(ctx context.Context, volName, name string) (io.ReadCloser, error) {
	key := filepath.Join("volumes", volName, name)

	out, err := s.sc.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})

	if err != nil {
		if s.isNoSuchKey(err) {
			return nil, os.ErrNotExist
		}

		return nil, err
	}

	return out.Body, nil
}

func (s *S3Access) RemoveSegment(ctx context.Context, seg SegmentId) error {
	key := "objects/object." + ulid.ULID(seg).String()

	_, err := s.sc.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})

	return err
}

func (s *S3Access) RemoveSegmentFromVolume(ctx context.Context, vol string, seg SegmentId) error {
	segments, err := s.ListSegments(ctx, vol)
	if err != nil {
		return err
	}

	slices.DeleteFunc(segments, func(si SegmentId) bool { return si == seg })

	var buf bytes.Buffer

	for _, seg := range segments {
		buf.Write(seg[:])
	}

	name := filepath.Join("volumes", vol, "objects")

	_, err = s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket,
		Key:    &name,
		Body:   &buf,
	})
	return err
}

func (s *S3Access) AppendToObjects(ctx context.Context, vol string, seg SegmentId) error {
	segments, err := s.ListSegments(ctx, vol)
	if err != nil {
		return err
	}

	segments = append(segments, seg)

	var buf bytes.Buffer

	for _, seg := range segments {
		buf.Write(seg[:])
	}

	name := filepath.Join("volumes", vol, "objects")

	_, err = s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket,
		Key:    &name,
		Body:   &buf,
	})
	return err
}

func (s *S3Access) InitContainer(ctx context.Context) error {
	return nil
}

func (s *S3Access) InitVolume(ctx context.Context, vol *VolumeInfo) error {
	return nil
}

func (s *S3Access) ListVolumes(ctx context.Context) ([]string, error) {
	prefix := "volumes/"

	var (
		token   *string
		volumes []string
	)

	for {
		out, err := s.sc.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            &s.bucket,
			Prefix:            &prefix,
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}

		for _, obj := range out.Contents {
			key := *obj.Key

			key = key[len(prefix):]

			if idx := strings.IndexByte(key, '/'); idx != -1 {
				volumes = append(volumes, key[:idx])
			} else {
				volumes = append(volumes, key)
			}
		}
		if out.IsTruncated != nil && *out.IsTruncated {
			token = out.NextContinuationToken
		} else {
			break
		}
	}

	return volumes, nil
}

var _ SegmentAccess = (*S3Access)(nil)
