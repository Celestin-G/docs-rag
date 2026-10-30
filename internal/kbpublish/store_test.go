package kbpublish

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// memStore is an in-memory bucket.
type memStore struct {
	mu      sync.Mutex
	objects map[string]string // key -> sha256 metadata
	puts    []string
	deletes []string
	failPut string
}

func newMem(objs map[string]string) *memStore { return &memStore{objects: objs} }

func (m *memStore) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Object{Key: k})
		}
	}
	return out, nil
}
func (m *memStore) Hash(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.objects[key], nil
}
func (m *memStore) Put(_ context.Context, key string, _ []byte, _, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if key == m.failPut {
		return errors.New("boom")
	}
	m.objects[key] = hash
	m.puts = append(m.puts, key)
	return nil
}
func (m *memStore) Delete(_ context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.objects, k)
	}
	m.deletes = append(m.deletes, keys...)
	return nil
}

func file(key, body string) File { return NewFile(key, body, validAttrs()) }

func TestNewFileHash(t *testing.T) {
	a, b := file("docs/a.md", "x"), file("docs/a.md", "x")
	if a.Hash != b.Hash || len(a.Hash) != 64 {
		t.Errorf("hash not stable: %q %q", a.Hash, b.Hash)
	}
	if file("docs/a.md", "y").Hash == a.Hash {
		t.Error("body change must change the hash")
	}
	attrs := validAttrs()
	attrs["updated_at"] = "2020-01-01"
	if NewFile("docs/a.md", "x", attrs).Hash == a.Hash {
		t.Error("sidecar change must change the hash")
	}
}

func TestComputeDiff(t *testing.T) {
	same, changed, fresh := file("docs/s.md", "same"), file("docs/c.md", "new text"), file("docs/n.md", "n")
	half := file("docs/h.md", "half")
	store := newMem(map[string]string{
		"docs/s.md": same.Hash, "docs/s.md" + sidecarSuffix: same.Hash,
		"docs/c.md": "stale", "docs/c.md" + sidecarSuffix: "stale",
		"docs/h.md":    half.Hash, // sidecar missing: half an upload
		"docs/gone.md": "x", "docs/gone.md" + sidecarSuffix: "x",
		"docs/orphan.md" + sidecarSuffix: "x",
		"docs/notes.txt":                 "x",
	})
	d, err := ComputeDiff(t.Context(), store, []File{same, changed, fresh, half}, 2)
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("new", d.New, []string{"docs/n.md"})
	check("changed", d.Changed, []string{"docs/c.md", "docs/h.md"})
	check("deleted", d.Deleted, []string{"docs/gone.md"})
	check("orphans", d.OrphanSidecars, []string{"docs/orphan.md" + sidecarSuffix})
	check("others", d.Others, []string{"docs/notes.txt"})
	check("delete keys", d.DeleteKeys(), []string{"docs/gone.md", "docs/gone.md" + sidecarSuffix, "docs/notes.txt", "docs/orphan.md" + sidecarSuffix})
	if d.Unchanged != 1 || d.Existing != 9 || !d.Changes() {
		t.Errorf("unchanged %d existing %d", d.Unchanged, d.Existing)
	}
}

func TestComputeDiffEmptyBucketAndNoChange(t *testing.T) {
	f := file("docs/a.md", "a")
	d, _ := ComputeDiff(t.Context(), EmptyStore{}, []File{f}, 0)
	if len(d.New) != 1 || d.Existing != 0 || d.CheckDeletes(false) != nil {
		t.Errorf("empty bucket diff = %+v", d)
	}
	d, _ = ComputeDiff(t.Context(), newMem(map[string]string{"docs/a.md": f.Hash, "docs/a.md" + sidecarSuffix: f.Hash}), []File{f}, 0)
	if d.Changes() {
		t.Errorf("identical bucket reports changes: %+v", d)
	}
}

func TestCheckDeletes(t *testing.T) {
	mk := func(existing, deleted int) *Diff {
		d := &Diff{Existing: existing}
		for i := range deleted {
			d.Others = append(d.Others, fmt.Sprintf("docs/x%d.txt", i))
		}
		return d
	}
	for _, tc := range []struct {
		name  string
		d     *Diff
		allow bool
		fail  string
	}{
		{"nothing to delete", mk(10, 0), false, ""},
		{"exactly 20 percent", mk(10, 2), false, ""},
		{"over 20 percent", mk(10, 3), false, "refusing to delete 3 of 10"},
		{"over 20 percent allowed", mk(10, 3), true, ""},
		{"outside docs is never allowed", &Diff{Existing: 100, Others: []string{"other/x.txt"}}, true, "outside docs/"},
		{"outside docs via a body", &Diff{Existing: 100, Deleted: []string{"private/a.md"}}, true, "outside docs/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.d.CheckDeletes(tc.allow)
			if tc.fail == "" && err != nil || tc.fail != "" && (err == nil || !strings.Contains(err.Error(), tc.fail)) {
				t.Errorf("err = %v, want %q", err, tc.fail)
			}
		})
	}
}

func TestApply(t *testing.T) {
	a, b := file("docs/a.md", "a"), file("docs/b.md", "b")
	store := newMem(map[string]string{"docs/old.md": "x", "docs/old.md" + sidecarSuffix: "x", "docs/k1": "", "docs/k2": "", "docs/k3": "", "docs/k4": "", "docs/k5": ""})
	d := &Diff{New: []string{"docs/a.md"}, Changed: []string{"docs/b.md"}, Deleted: []string{"docs/old.md"}, Existing: 20}
	if err := Apply(t.Context(), store, d, []File{a, b}, false, 2); err != nil {
		t.Fatal(err)
	}
	sort.Strings(store.puts)
	if want := []string{"docs/a.md", "docs/a.md" + sidecarSuffix, "docs/b.md", "docs/b.md" + sidecarSuffix}; !slices.Equal(store.puts, want) {
		t.Errorf("puts = %v", store.puts)
	}
	if store.objects["docs/a.md"] != a.Hash || store.objects["docs/a.md"+sidecarSuffix] != a.Hash {
		t.Error("both objects must carry the sha256 metadata")
	}
	if _, still := store.objects["docs/old.md"]; still {
		t.Error("stale object not deleted")
	}

	// A refused delete uploads nothing.
	store = newMem(map[string]string{"docs/o1.md": "", "docs/o2.md": ""})
	d = &Diff{New: []string{"docs/a.md"}, Deleted: []string{"docs/o1.md", "docs/o2.md"}, Existing: 2}
	if err := Apply(t.Context(), store, d, []File{a}, false, 1); err == nil || len(store.puts) != 0 {
		t.Errorf("mass delete not refused before uploading: %v, puts %v", err, store.puts)
	}

	// An upload failure stops before any delete.
	store = newMem(map[string]string{"docs/old.md": "x"})
	store.failPut = "docs/a.md"
	d = &Diff{New: []string{"docs/a.md"}, Deleted: []string{"docs/old.md"}, Existing: 20}
	if err := Apply(t.Context(), store, d, []File{a}, false, 1); err == nil || len(store.deletes) != 0 {
		t.Errorf("err %v, deletes %v", err, store.deletes)
	}
}

// fakeS3 implements S3API.
type fakeS3 struct {
	pages       [][]string
	deleteCalls [][]string
	deleteErrs  []types.Error
	head        map[string]map[string]string
	put         *s3.PutObjectInput
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	i := 0
	if in.ContinuationToken != nil {
		fmt.Sscanf(*in.ContinuationToken, "%d", &i)
	}
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(i+1 < len(f.pages))}
	for _, k := range f.pages[i] {
		out.Contents = append(out.Contents, types.Object{Key: aws.String(k), Size: aws.Int64(1)})
	}
	if i+1 < len(f.pages) {
		out.NextContinuationToken = aws.String(fmt.Sprint(i + 1))
	}
	return out, nil
}
func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	m, ok := f.head[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NotFound{}
	}
	return &s3.HeadObjectOutput{Metadata: m}, nil
}
func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.put = in
	return &s3.PutObjectOutput{}, nil
}
func (f *fakeS3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	var keys []string
	for _, o := range in.Delete.Objects {
		keys = append(keys, aws.ToString(o.Key))
	}
	f.deleteCalls = append(f.deleteCalls, keys)
	return &s3.DeleteObjectsOutput{Errors: f.deleteErrs}, nil
}

func TestS3Store(t *testing.T) {
	api := &fakeS3{
		pages: [][]string{{"docs/a.md", "docs/b.md"}, {"docs/c.md"}},
		head:  map[string]map[string]string{"docs/a.md": {"sha256": "abc"}},
	}
	s := &S3Store{API: api, Bucket: "bkt"}
	objs, err := s.List(t.Context(), "docs/")
	if err != nil || len(objs) != 3 {
		t.Fatalf("list across pages = %v, %v", objs, err)
	}
	if h, err := s.Hash(t.Context(), "docs/a.md"); h != "abc" || err != nil {
		t.Errorf("hash = %q, %v", h, err)
	}
	if h, err := s.Hash(t.Context(), "docs/missing.md"); h != "" || err != nil {
		t.Errorf("a missing object must hash to \"\", got %q, %v", h, err)
	}
	if err := s.Put(t.Context(), "docs/a.md", []byte("x"), bodyContentType, "h1"); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(api.put.ContentType) != "text/markdown" || api.put.Metadata["sha256"] != "h1" || aws.ToString(api.put.Bucket) != "bkt" {
		t.Errorf("put = %+v", api.put)
	}

	keys := make([]string, 2500)
	for i := range keys {
		keys[i] = fmt.Sprintf("docs/k%04d.md", i)
	}
	if err := s.Delete(t.Context(), keys); err != nil {
		t.Fatal(err)
	}
	if len(api.deleteCalls) != 3 || len(api.deleteCalls[0]) != 1000 || len(api.deleteCalls[2]) != 500 {
		t.Errorf("delete batches = %d", len(api.deleteCalls))
	}
	api.deleteErrs = []types.Error{{Key: aws.String("docs/k"), Code: aws.String("AccessDenied")}}
	if err := s.Delete(t.Context(), keys[:1]); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("per-key delete errors must surface: %v", err)
	}
}
