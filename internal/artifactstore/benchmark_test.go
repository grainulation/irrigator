package artifactstore_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
)

func BenchmarkLocalArtifactStore(b *testing.B) {
	data := bytes.Repeat([]byte("a"), 64*1024)
	b.Run("Put64KiB", func(b *testing.B) {
		store, err := artifactstore.NewLocal(b.TempDir(), "run", 4*1024*1024)
		if err != nil {
			b.Fatal(err)
		}
		defer store.Close()
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, err := artifactstore.Describe("run", fmt.Sprintf("artifact-%d", i), data)
			if err != nil {
				b.Fatal(err)
			}
			if err = store.Put(context.Background(), ref, bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Get64KiB", func(b *testing.B) {
		store, err := artifactstore.NewLocal(b.TempDir(), "run", 4*1024*1024)
		if err != nil {
			b.Fatal(err)
		}
		defer store.Close()
		ref, _ := artifactstore.Describe("run", "artifact", data)
		if err = store.Put(context.Background(), ref, bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err = store.Get(context.Background(), ref); err != nil {
				b.Fatal(err)
			}
		}
	})
}
