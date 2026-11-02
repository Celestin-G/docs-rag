package kbpublish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	bodyContentType    = "text/markdown"
	sidecarContentType = "application/json"
	sidecarSuffix      = ".metadata.json"
	hashMetadataKey    = "sha256"
	deleteBatchSize    = 1000
	defaultConcurrency = 8
	// A publish that deletes more than this share of what is in the bucket is
	// almost always a wrong DOCS_PATH or a broken parse, not a real cleanup.
	massDeleteRatio = 0.20
)

// File is one knowledge-base document: the markdown body and its sidecar.
type File struct {
	Key     string // docs/<product>/.../<name>.md
	Body    []byte
	Sidecar []byte // content of <Key>.metadata.json
	Hash    string // sha256 of body + "\n" + sidecar
}

// NewFile assembles a File and computes its hash.
func NewFile(key, body string, attrs Attrs) File {
	f := File{Key: key, Body: []byte(body), Sidecar: attrs.Sidecar()}
	f.Hash = hashOf(f.Body, f.Sidecar)
	return f
}

func hashOf(body, sidecar []byte) string {
	h := sha256.New()
	h.Write(body)
	h.Write([]byte("\n"))
	h.Write(sidecar)
	return hex.EncodeToString(h.Sum(nil))
}

// SidecarKey is the object key of a body's metadata sidecar.
func SidecarKey(key string) string { return key + sidecarSuffix }

// Object is one entry of a bucket listing.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time // LastModified of the listing
}

// PlanHash identifies the desired state of the bucket: sha256 over the sorted
// "key:hash" lines of the files. A job started for another plan (or none) leaves
// the knowledge base out of date, even when no object was written since: a crash
// after the deletions leaves no LastModified behind.
func PlanHash(files []File) string {
	lines := make([]string, len(files))
	for i, f := range files {
		lines[i] = f.Key + ":" + f.Hash
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}

// ObjectStore is what the publisher needs from the bucket.
type ObjectStore interface {
	// List returns every object whose key starts with prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Hash returns the sha256 user metadata of an object, "" when it has none.
	Hash(ctx context.Context, key string) (string, error)
	Put(ctx context.Context, key string, body []byte, contentType, hash string) error
	// Delete removes the keys, in as many requests as the service needs.
	Delete(ctx context.Context, keys []string) error
}

// EmptyStore is the bucket of --offline: nothing exists, so everything is new.
type EmptyStore struct{}

func (EmptyStore) List(context.Context, string) ([]Object, error) { return nil, nil }
func (EmptyStore) Hash(context.Context, string) (string, error)   { return "", nil }
func (EmptyStore) Put(context.Context, string, []byte, string, string) error {
	return errors.New("offline store is read-only")
}
func (EmptyStore) Delete(context.Context, []string) error {
	return errors.New("offline store is read-only")
}

// Diff is what publishing would change in the bucket.
type Diff struct {
	New, Changed []string // body keys
	Unchanged    int
	// Deleted lists body keys present in the bucket and not wanted.
	Deleted []string
	// OrphanSidecars are sidecars whose body does not exist.
	OrphanSidecars []string
	// Others are other objects under the prefix (neither .md nor its sidecar).
	Others []string
	// Existing is the number of objects under docs/ before the publish.
	Existing int
	// Newest is the latest LastModified among them, zero when there are none.
	Newest time.Time
}

// DeleteKeys returns every object key to remove: stale bodies with their
// sidecars, orphan sidecars and strays, sorted.
func (d *Diff) DeleteKeys() []string {
	keys := make([]string, 0, 2*len(d.Deleted)+len(d.OrphanSidecars)+len(d.Others))
	for _, k := range d.Deleted {
		keys = append(keys, k, SidecarKey(k))
	}
	keys = append(keys, d.OrphanSidecars...)
	keys = append(keys, d.Others...)
	sort.Strings(keys)
	return keys
}

// Changes reports whether a publish would touch the bucket.
func (d *Diff) Changes() bool {
	return len(d.New)+len(d.Changed)+len(d.Deleted)+len(d.OrphanSidecars)+len(d.Others) > 0
}

// ComputeDiff compares the wanted files with the bucket. It lists everything
// under docs/ and reads the sha256 metadata of the bodies and sidecars that exist on both
// sides, a few at a time.
func ComputeDiff(ctx context.Context, store ObjectStore, files []File, concurrency int) (*Diff, error) {
	if concurrency < 1 {
		concurrency = defaultConcurrency
	}
	existing, err := store.List(ctx, keyPrefix)
	if err != nil {
		return nil, fmt.Errorf("list s3://…/%s: %w", keyPrefix, err)
	}
	have := make(map[string]bool, len(existing))
	var newest time.Time
	for _, o := range existing {
		have[o.Key] = true
		if o.Modified.After(newest) {
			newest = o.Modified
		}
	}
	want := make(map[string]File, len(files))
	for _, f := range files {
		want[f.Key] = f
	}

	d := &Diff{Existing: len(existing), Newest: newest}
	var toCheck []File
	for _, f := range files {
		switch {
		case !have[f.Key]:
			d.New = append(d.New, f.Key)
		case !have[SidecarKey(f.Key)]:
			d.Changed = append(d.Changed, f.Key) // half an upload: redo both
		default:
			toCheck = append(toCheck, f)
		}
	}

	hashes := make([][2]string, len(toCheck)) // body, sidecar
	errs := make([]error, len(toCheck))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, f := range toCheck {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			bodyHash, err1 := store.Hash(ctx, f.Key)
			sideHash, err2 := store.Hash(ctx, SidecarKey(f.Key))
			hashes[i], errs[i] = [2]string{bodyHash, sideHash}, errors.Join(err1, err2)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("read object metadata: %w", err)
	}
	for i, f := range toCheck {
		// A missing hash on either object counts as a mismatch.
		if hashes[i][0] == f.Hash && hashes[i][1] == f.Hash {
			d.Unchanged++
		} else {
			d.Changed = append(d.Changed, f.Key)
		}
	}

	for _, o := range existing {
		k := o.Key
		if _, ok := want[k]; ok {
			continue
		}
		if base, isSidecar := strings.CutSuffix(k, sidecarSuffix); isSidecar {
			if _, ok := want[base]; ok {
				continue
			}
			if have[base] {
				continue // goes away with its body, listed under Deleted
			}
			d.OrphanSidecars = append(d.OrphanSidecars, k)
			continue
		}
		if strings.HasSuffix(k, ".md") {
			d.Deleted = append(d.Deleted, k)
			continue
		}
		d.Others = append(d.Others, k)
	}
	for _, s := range [][]string{d.New, d.Changed, d.Deleted, d.OrphanSidecars, d.Others} {
		sort.Strings(s)
	}
	return d, nil
}

// CheckDeletes refuses a publish whose deletions look wrong: a key outside
// docs/, or more than 20% of the bucket's objects unless allowMass is set.
func (d *Diff) CheckDeletes(allowMass bool) error {
	keys := d.DeleteKeys()
	for _, k := range keys {
		if !strings.HasPrefix(k, keyPrefix) {
			return fmt.Errorf("refusing to delete %q: outside %s", k, keyPrefix)
		}
	}
	if len(keys) > 0 && d.Existing > 0 && float64(len(keys)) > massDeleteRatio*float64(d.Existing) && !allowMass {
		return fmt.Errorf("refusing to delete %d of %d objects (more than %.0f%%): check DOCS_PATH, or pass --allow-mass-delete",
			len(keys), d.Existing, massDeleteRatio*100)
	}
	return nil
}

// Apply uploads the new and changed files and deletes the stale objects. It
// stops at the first error rather than leaving the bucket further from a state
// the next run would reconcile anyway.
func Apply(ctx context.Context, store ObjectStore, d *Diff, files []File, allowMass bool, concurrency int) error {
	if err := d.CheckDeletes(allowMass); err != nil {
		return err
	}
	if concurrency < 1 {
		concurrency = defaultConcurrency
	}
	byKey := make(map[string]File, len(files))
	for _, f := range files {
		byKey[f.Key] = f
	}
	var uploads []File
	for _, k := range append(append([]string{}, d.New...), d.Changed...) {
		uploads = append(uploads, byKey[k])
	}

	errs := make([]error, len(uploads))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, f := range uploads {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			// Sidecar first: a body never exists without its metadata.
			if err := store.Put(ctx, SidecarKey(f.Key), f.Sidecar, sidecarContentType, f.Hash); err != nil {
				errs[i] = fmt.Errorf("upload %s: %w", SidecarKey(f.Key), err)
				return
			}
			if err := store.Put(ctx, f.Key, f.Body, bodyContentType, f.Hash); err != nil {
				errs[i] = fmt.Errorf("upload %s: %w", f.Key, err)
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if keys := d.DeleteKeys(); len(keys) > 0 {
		if err := store.Delete(ctx, keys); err != nil {
			return fmt.Errorf("delete stale objects: %w", err)
		}
	}
	return nil
}

// S3API is the subset of *s3.Client the publisher uses.
type S3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, opts ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// S3Store is an ObjectStore on one bucket.
type S3Store struct {
	API    S3API
	Bucket string
}

func (s *S3Store) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	var token *string
	for {
		res, err := s.API.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(s.Bucket), Prefix: aws.String(prefix), ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, o := range res.Contents {
			out = append(out, Object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), Modified: aws.ToTime(o.LastModified)})
		}
		if !aws.ToBool(res.IsTruncated) || res.NextContinuationToken == nil {
			return out, nil
		}
		token = res.NextContinuationToken
	}
}

func (s *S3Store) Hash(ctx context.Context, key string) (string, error) {
	res, err := s.API.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(key)})
	if err != nil {
		var nf *types.NotFound
		if errors.As(err, &nf) {
			return "", nil
		}
		var ae smithy.APIError
		if errors.As(err, &ae) && (ae.ErrorCode() == "NotFound" || ae.ErrorCode() == "NoSuchKey") {
			return "", nil
		}
		return "", err
	}
	return res.Metadata[hashMetadataKey], nil
}

func (s *S3Store) Put(ctx context.Context, key string, body []byte, contentType, hash string) error {
	_, err := s.API.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
		Metadata:    map[string]string{hashMetadataKey: hash},
	})
	return err
}

func (s *S3Store) Delete(ctx context.Context, keys []string) error {
	for start := 0; start < len(keys); start += deleteBatchSize {
		batch := keys[start:min(start+deleteBatchSize, len(keys))]
		ids := make([]types.ObjectIdentifier, len(batch))
		for i, k := range batch {
			ids[i] = types.ObjectIdentifier{Key: aws.String(k)}
		}
		res, err := s.API.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.Bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return err
		}
		if len(res.Errors) > 0 {
			e := res.Errors[0]
			return fmt.Errorf("%d of %d keys not deleted, first: %s: %s %s",
				len(res.Errors), len(batch), aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
		}
	}
	return nil
}
