package images

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CampusTech/fleet2snipe/fleetapi"
)

// TestFetchAppleDBImage hits the live appledb.dev API. Skipped unless
// FLEET2SNIPE_LIVE_TESTS=1 so CI without network access doesn't break.
func TestFetchAppleDBImage(t *testing.T) {
	if os.Getenv("FLEET2SNIPE_LIVE_TESTS") != "1" {
		t.Skip("set FLEET2SNIPE_LIVE_TESTS=1 to run live appledb test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	f := NewFetcher()
	uri, err := f.ForHost(ctx, fleetapi.Host{
		HardwareVendor: "Apple Inc.",
		HardwareModel:  "MacBookPro17,1",
		Platform:       "darwin",
	})
	if err != nil {
		t.Fatalf("ForHost returned error: %v", err)
	}
	if uri == "" {
		t.Fatal("expected non-empty data URI for MacBookPro17,1")
	}
	if !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Errorf("expected data:image/png;base64, prefix, got %q", uri[:min(40, len(uri))])
	}
	if len(uri) < 1000 {
		t.Errorf("data URI is suspiciously short (%d bytes)", len(uri))
	}
}

func TestForHost_NonApple(t *testing.T) {
	f := NewFetcher()
	uri, err := f.ForHost(context.Background(), fleetapi.Host{
		HardwareVendor: "Dell Inc.",
		HardwareModel:  "Latitude 5530",
		Platform:       "windows",
	})
	if err != nil {
		t.Fatalf("unexpected error for non-Apple host: %v", err)
	}
	if uri != "" {
		t.Errorf("expected empty URI for Dell, got %q", uri)
	}
}

func TestForHost_EmptyModel(t *testing.T) {
	f := NewFetcher()
	uri, err := f.ForHost(context.Background(), fleetapi.Host{
		HardwareVendor: "Apple Inc.",
		Platform:       "darwin",
		// HardwareModel omitted
	})
	if err != nil {
		t.Fatalf("unexpected error for empty model: %v", err)
	}
	if uri != "" {
		t.Error("expected empty URI when HardwareModel is blank")
	}
}
