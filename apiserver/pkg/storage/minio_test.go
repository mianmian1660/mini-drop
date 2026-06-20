package storage

import (
	"context"
	"testing"
	"time"
)

func TestNewMinIOStorageWithPublicAndPresignedURL(t *testing.T) {
	store, err := NewMinIOStorageWithPublic("minio:9000", "localhost:9000", "ak", "sk", false)
	if err != nil {
		t.Fatalf("NewMinIOStorageWithPublic failed: %v", err)
	}

	url, err := store.PresignedGetURL(context.Background(), "drop-data", "tid-1/flamegraph.svg", 15*time.Minute)
	if err != nil {
		t.Fatalf("PresignedGetURL failed: %v", err)
	}
	if want := "http://localhost:9000/drop-data/tid-1/flamegraph.svg"; url != want {
		t.Fatalf("url = %q, want %q", url, want)
	}
	if store.Endpoint() != "minio:9000" {
		t.Fatalf("endpoint = %q", store.Endpoint())
	}
}

func TestNewMinIOStorageFallsBackToInternalEndpoint(t *testing.T) {
	store, err := NewMinIOStorage("minio:9000", "ak", "sk", true)
	if err != nil {
		t.Fatalf("NewMinIOStorage failed: %v", err)
	}

	url, err := store.PresignedGetURL(context.Background(), "drop-data", "tid-1/top.json", time.Minute)
	if err != nil {
		t.Fatalf("PresignedGetURL failed: %v", err)
	}
	if want := "https://minio:9000/drop-data/tid-1/top.json"; url != want {
		t.Fatalf("url = %q, want %q", url, want)
	}
}
