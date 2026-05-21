// Package images fetches device images for use as Snipe-IT model images.
//
// Each device source decides which identifier to key off based on what works
// best for it — AppleDB takes the model_identifier (e.g. "MacBookPro17,1");
// future vendor sources could parse hardware_serial (Dell service tag) or
// uuid. The Fetcher accepts the full Fleet host so it has every key handy.
package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/CampusTech/fleet2snipe/fleetapi"
)

var log = logrus.New()

// SetLogLevel sets the package logger level.
func SetLogLevel(level logrus.Level) { log.SetLevel(level) }

// SetLogFormatter sets the package logger formatter.
func SetLogFormatter(f logrus.Formatter) { log.SetFormatter(f) }

// SetLogOutput sets the package logger output.
func SetLogOutput(w io.Writer) { log.SetOutput(w) }

// Fetcher resolves a Snipe-IT-ready model image (base64 data URI) for a Fleet
// host. It caches per model identifier so a single sync run only hits each
// upstream once — and even caches negative lookups so a 404 doesn't repeat.
type Fetcher struct {
	hc *http.Client

	mu    sync.Mutex
	cache map[string]string // key -> data URI (empty string = looked up, no image)
}

// NewFetcher constructs a Fetcher with sane defaults. The HTTP client has a
// short timeout so a slow upstream can't drag down a sync.
func NewFetcher() *Fetcher {
	return &Fetcher{
		hc:    &http.Client{Timeout: 10 * time.Second},
		cache: make(map[string]string),
	}
}

// ForHost returns a base64 data URI for the host's model, or "" if no source
// covers this device. Errors are returned but the caller should treat them as
// non-fatal (continue creating the model without an image).
func (f *Fetcher) ForHost(ctx context.Context, h fleetapi.Host) (string, error) {
	if isAppleVendor(h.HardwareVendor) || isApplePlatform(h.Platform) {
		key := strings.TrimSpace(h.HardwareModel)
		if key == "" {
			return "", nil
		}
		return f.cached(key, func() (string, error) { return f.fromAppleDB(ctx, key) })
	}
	// Other vendors: no source wired up yet. Return cleanly so the engine
	// can carry on without warning noise.
	return "", nil
}

// cached fetches lazily and memoizes the result (including the empty-string
// "no image" outcome).
func (f *Fetcher) cached(key string, fn func() (string, error)) (string, error) {
	f.mu.Lock()
	if v, ok := f.cache[key]; ok {
		f.mu.Unlock()
		return v, nil
	}
	f.mu.Unlock()

	v, err := fn()
	if err != nil {
		// Cache the empty result too so we don't retry on every host using
		// this model in the current run.
		f.mu.Lock()
		f.cache[key] = ""
		f.mu.Unlock()
		return "", err
	}
	f.mu.Lock()
	f.cache[key] = v
	f.mu.Unlock()
	return v, nil
}

// fromAppleDB queries https://api.appledb.dev for an Apple device by its
// model identifier (e.g. "MacBookPro17,1"). Image URL pattern (matches the
// upstream jamf2snipe convention):
//
//	https://img.appledb.dev/device@main/{imageKey}/{color}.png
//
// where imageKey is from the API response (usually identical to the identifier)
// and color is the `key` of the first entry in `colors`. AppleDB falls back to
// the identifier when imageKey is absent for older or stub entries.
func (f *Fetcher) fromAppleDB(ctx context.Context, modelID string) (string, error) {
	apiURL := "https://api.appledb.dev/device/" + url.PathEscape(modelID) + ".json"

	var info struct {
		ImageKey string `json:"imageKey"`
		Colors   []struct {
			Key string `json:"key"`
		} `json:"colors"`
	}
	if err := f.getJSON(ctx, apiURL, &info); err != nil {
		return "", fmt.Errorf("appledb lookup %s: %w", modelID, err)
	}

	imageKey := firstNonEmpty(info.ImageKey, modelID)
	var color string
	if len(info.Colors) > 0 {
		color = info.Colors[0].Key
	}
	if color == "" {
		// Some entries omit colors; fall back to "0" which appledb uses for
		// devices with no color variant.
		color = "0"
	}

	imgURL := "https://img.appledb.dev/device@main/" + url.PathEscape(imageKey) + "/" + url.PathEscape(color) + ".png"
	dataURI, err := f.downloadAsDataURI(ctx, imgURL)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", imgURL, err)
	}
	if dataURI == "" {
		return "", nil
	}
	log.WithFields(logrus.Fields{"model": modelID, "src": imgURL, "bytes": len(dataURI)}).Debug("fetched appledb image")
	return dataURI, nil
}

// getJSON performs a GET request and decodes the body. 404s return nil error
// so the cache can record "no image" instead of bubbling up.
func (f *Fetcher) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// downloadAsDataURI fetches a binary and returns a "data:<mime>;base64,..." URI.
// Snipe-IT accepts this form on Model.Image at create-time.
func (f *Fetcher) downloadAsDataURI(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := f.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5 MiB cap
	if err != nil {
		return "", err
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = mimeFromURL(u)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(body), nil
}

func isApplePlatform(p string) bool {
	switch strings.ToLower(p) {
	case "darwin", "ios", "ipados", "watchos", "visionos":
		return true
	}
	return false
}

func isAppleVendor(v string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "apple")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// mimeFromURL provides a best-guess MIME type when the upstream omits one.
func mimeFromURL(u string) string {
	switch {
	case strings.HasSuffix(u, ".png"):
		return "image/png"
	case strings.HasSuffix(u, ".jpg"), strings.HasSuffix(u, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(u, ".webp"):
		return "image/webp"
	case strings.HasSuffix(u, ".heic"):
		return "image/heic"
	}
	return "application/octet-stream"
}
