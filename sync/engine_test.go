package sync

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	snipeit "github.com/michellepellon/go-snipeit"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/CampusTech/fleet2snipe/config"
	"github.com/CampusTech/fleet2snipe/fleetapi"
)

func TestTransformValue(t *testing.T) {
	cases := []struct {
		name      string
		json      string // wrapped as {"v": ...}
		transform string
		want      string
	}{
		// No transform — raw stringifyGJSON path.
		{"no transform, integer", `{"v": 42}`, "", "42"},
		{"no transform, string", `{"v": "abc"}`, "", "abc"},

		// bytes_to_gb: divide by 1e9, round.
		{"bytes 1 GB exact", `{"v": 1000000000}`, "bytes_to_gb", "1"},
		{"bytes 8 GiB", `{"v": 8589934592}`, "bytes_to_gb", "9"},    // 8.59 → 9
		{"bytes 16 GiB", `{"v": 17179869184}`, "bytes_to_gb", "17"}, // 17.18 → 17
		{"bytes 32 GiB", `{"v": 34359738368}`, "bytes_to_gb", "34"}, // 34.36 → 34
		{"bytes 64 GiB", `{"v": 68719476736}`, "bytes_to_gb", "69"}, // 68.72 → 69
		{"bytes 500 GB exact", `{"v": 500000000000}`, "bytes_to_gb", "500"},
		{"bytes zero", `{"v": 0}`, "bytes_to_gb", ""},
		{"bytes missing", `{"missing": 1}`, "bytes_to_gb", ""},
		{"bytes unparseable string", `{"v": "abc"}`, "bytes_to_gb", ""},

		// gib_to_gb: multiply by 1.073741824, round.
		{"gib 1 GiB → 1 GB", `{"v": 1.0}`, "gib_to_gb", "1"},
		{"gib 1.5 → 2", `{"v": 1.5}`, "gib_to_gb", "2"},
		{"gib 100 → 107", `{"v": 100.0}`, "gib_to_gb", "107"},
		{"gib 465.5 → 500", `{"v": 465.5}`, "gib_to_gb", "500"},
		{"gib 1000 → 1074", `{"v": 1000.0}`, "gib_to_gb", "1074"},
		{"gib zero", `{"v": 0}`, "gib_to_gb", ""},
		{"gib missing", `{"missing": 1.0}`, "gib_to_gb", ""},
		{"gib unparseable string", `{"v": "abc"}`, "gib_to_gb", ""},

		// bytes_to_gib (binary GiB, matches About This Mac convention)
		{"bytes_to_gib 1 GiB exact", `{"v": 1073741824}`, "bytes_to_gib", "1"},
		{"bytes_to_gib 16 GiB", `{"v": 17179869184}`, "bytes_to_gib", "16"},
		{"bytes_to_gib 48 GiB", `{"v": 51539607552}`, "bytes_to_gib", "48"},
		{"bytes_to_gib 64 GiB", `{"v": 68719476736}`, "bytes_to_gib", "64"},
		{"bytes_to_gib zero", `{"v": 0}`, "bytes_to_gib", ""},
		{"bytes_to_gib missing", `{"x": 1}`, "bytes_to_gib", ""},

		// bytes_to_mb
		{"bytes_to_mb 1 MB exact", `{"v": 1000000}`, "bytes_to_mb", "1"},
		{"bytes_to_mb 512 MiB", `{"v": 536870912}`, "bytes_to_mb", "537"},
		{"bytes_to_mb zero", `{"v": 0}`, "bytes_to_mb", ""},
		{"bytes_to_mb missing", `{"x": 1}`, "bytes_to_mb", ""},

		// bytes_to_tb
		{"bytes_to_tb 1 TB exact", `{"v": 1000000000000}`, "bytes_to_tb", "1"},
		{"bytes_to_tb 2 TiB", `{"v": 2199023255552}`, "bytes_to_tb", "2"},
		{"bytes_to_tb zero", `{"v": 0}`, "bytes_to_tb", ""},

		// unix_to_iso
		{"unix_to_iso epoch+1", `{"v": 1}`, "unix_to_iso", "1970-01-01 00:00:01"},
		{"unix_to_iso known time", `{"v": 1700000000}`, "unix_to_iso", "2023-11-14 22:13:20"},
		{"unix_to_iso zero", `{"v": 0}`, "unix_to_iso", ""},
		{"unix_to_iso missing", `{"x": 1}`, "unix_to_iso", ""},

		// uppercase / lowercase
		{"uppercase", `{"v": "Hello-World"}`, "uppercase", "HELLO-WORLD"},
		{"uppercase empty", `{"v": ""}`, "uppercase", ""},
		{"uppercase missing", `{"x": "y"}`, "uppercase", ""},
		{"lowercase", `{"v": "Hello-World"}`, "lowercase", "hello-world"},
		{"lowercase non-string number", `{"v": 42}`, "lowercase", "42"},

		// base64_to_mac (Fleet ioreg IOMACAddress plist <data>)
		{"base64_to_mac real macOS sample", `{"v": "cIzyxNK1"}`, "base64_to_mac", "70:8c:f2:c4:d2:b5"},
		{"base64_to_mac all zeros", `{"v": "AAAAAAAA"}`, "base64_to_mac", "00:00:00:00:00:00"},
		{"base64_to_mac empty", `{"v": ""}`, "base64_to_mac", ""},
		{"base64_to_mac too few bytes", `{"v": "AAAA"}`, "base64_to_mac", ""},          // 3 bytes
		{"base64_to_mac too many bytes", `{"v": "AAAAAAAAAAAA"}`, "base64_to_mac", ""}, // 9 bytes
		{"base64_to_mac not base64", `{"v": "not!base64@@"}`, "base64_to_mac", ""},
		{"base64_to_mac missing", `{"x": "cIzyxNK1"}`, "base64_to_mac", ""},
		{"base64_to_mac whitespace tolerated", `{"v": "  cIzyxNK1  "}`, "base64_to_mac", "70:8c:f2:c4:d2:b5"},

		// mac_colons / mac_dashes
		{"mac_colons from dashes", `{"v": "AA-BB-CC-DD-EE-FF"}`, "mac_colons", "aa:bb:cc:dd:ee:ff"},
		{"mac_colons from colons", `{"v": "aa:bb:cc:dd:ee:ff"}`, "mac_colons", "aa:bb:cc:dd:ee:ff"},
		{"mac_colons from cisco dots", `{"v": "aabb.ccdd.eeff"}`, "mac_colons", "aa:bb:cc:dd:ee:ff"},
		{"mac_colons from bare hex", `{"v": "AABBCCDDEEFF"}`, "mac_colons", "aa:bb:cc:dd:ee:ff"},
		{"mac_dashes from colons", `{"v": "aa:bb:cc:dd:ee:ff"}`, "mac_dashes", "aa-bb-cc-dd-ee-ff"},
		{"mac invalid length", `{"v": "aabbccdd"}`, "mac_colons", ""},
		{"mac garbage", `{"v": "not a mac"}`, "mac_colons", ""},
		{"mac empty", `{"v": ""}`, "mac_colons", ""},

		// comma_thousands
		{"comma_thousands int", `{"v": 1234567}`, "comma_thousands", "1,234,567"},
		{"comma_thousands small int", `{"v": 42}`, "comma_thousands", "42"},
		{"comma_thousands zero passes through", `{"v": 0}`, "comma_thousands", "0"},
		{"comma_thousands negative", `{"v": -1234567}`, "comma_thousands", "-1,234,567"},
		{"comma_thousands string number", `{"v": "9876543"}`, "comma_thousands", "9,876,543"},
		{"comma_thousands float passes through", `{"v": 1.5}`, "comma_thousands", "1.5"},
		{"comma_thousands unparseable", `{"v": "abc"}`, "comma_thousands", ""},

		// bool_yes_no
		{"bool_yes_no native true", `{"v": true}`, "bool_yes_no", "Yes"},
		{"bool_yes_no native false", `{"v": false}`, "bool_yes_no", "No"},
		{"bool_yes_no string true", `{"v": "true"}`, "bool_yes_no", "Yes"},
		{"bool_yes_no string yes", `{"v": "yes"}`, "bool_yes_no", "Yes"},
		{"bool_yes_no string Y", `{"v": "Y"}`, "bool_yes_no", "Yes"},
		{"bool_yes_no string false", `{"v": "false"}`, "bool_yes_no", "No"},
		{"bool_yes_no string no", `{"v": "no"}`, "bool_yes_no", "No"},
		{"bool_yes_no number 1", `{"v": 1}`, "bool_yes_no", "Yes"},
		{"bool_yes_no number 0", `{"v": 0}`, "bool_yes_no", "No"},
		{"bool_yes_no unknown string", `{"v": "maybe"}`, "bool_yes_no", ""},

		// Unknown transform name — degrades to raw rather than dropping data.
		// Real usage rejects this at config load; this just covers the safety net.
		{"unknown transform falls back to raw", `{"v": 42}`, "wat", "42"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := gjson.Get(c.json, "v")
			got := transformValue(res, c.transform)
			if got != c.want {
				t.Errorf("transformValue(%q, %q) = %q, want %q", c.json, c.transform, got, c.want)
			}
		})
	}
}

// TestTransformString exercises the string-input adaptor used by query_mapping.
// Saved query columns always arrive as strings; the adaptor reparses numeric
// strings as JSON numbers so numeric transforms still work.
func TestTransformString(t *testing.T) {
	cases := []struct {
		name, in, transform, want string
	}{
		{"no transform passes through", "anything", "", "anything"},
		{"base64_to_mac on string", "cIzyxNK1", "base64_to_mac", "70:8c:f2:c4:d2:b5"},
		{"uppercase on string", "abc", "uppercase", "ABC"},
		{"bytes_to_gib on numeric string", "17179869184", "bytes_to_gib", "16"},
		{"unix_to_iso on numeric string", "1700000000", "unix_to_iso", "2023-11-14 22:13:20"},
		{"bool_yes_no on string true", "true", "bool_yes_no", "Yes"},
		{"comma_thousands on numeric string", "1234567", "comma_thousands", "1,234,567"},
		{"mac_colons on string", "AA-BB-CC-DD-EE-FF", "mac_colons", "aa:bb:cc:dd:ee:ff"},
		{"empty input", "", "uppercase", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := transformString(c.in, c.transform)
			if got != c.want {
				t.Errorf("transformString(%q, %q) = %q, want %q", c.in, c.transform, got, c.want)
			}
		})
	}
}

func TestRenderAssetTag(t *testing.T) {
	h := fleetapi.Host{
		ID:             42,
		HardwareSerial: "C02XK1JJJG5J",
		HardwareModel:  "MacBookPro17,1",
		Platform:       "darwin",
	}

	cases := []struct {
		name, template, want string
	}{
		{"literal only", "STATIC", "STATIC"},
		{"id placeholder", "fleet-{id}", "fleet-42"},
		{"serial placeholder", "CG-{hardware_serial}", "CG-C02XK1JJJG5J"},
		{"multiple placeholders", "{platform}-{id}", "darwin-42"},
		{"empty template returns empty", "", ""},
		{"unknown path is empty", "X-{nonexistent}-Y", "X--Y"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderAssetTag(c.template, h)
			if got != c.want {
				t.Errorf("renderAssetTag(%q) = %q, want %q", c.template, got, c.want)
			}
		})
	}
}

func TestAssetTagResolution(t *testing.T) {
	host := fleetapi.Host{ID: 7, HardwareSerial: "ABC123", Platform: "ios"}

	cases := []struct {
		name string
		cfg  config.SyncConfig
		want string
	}{
		{
			name: "default when nothing configured",
			cfg:  config.SyncConfig{},
			want: "fleet-7",
		},
		{
			name: "legacy asset_tag_prefix is honored",
			cfg:  config.SyncConfig{AssetTagPrefix: "cg-"},
			want: "cg-7",
		},
		{
			name: "global template overrides legacy prefix",
			cfg: config.SyncConfig{
				AssetTagPrefix: "ignored-",
				AssetTag:       config.AssetTagConfig{Template: "G-{id}"},
			},
			want: "G-7",
		},
		{
			name: "per-platform template wins",
			cfg: config.SyncConfig{
				AssetTag: config.AssetTagConfig{
					Template: "default-{id}",
					PlatformTemplates: map[string]string{
						"ios": "iPhone-{hardware_serial}",
					},
				},
			},
			want: "iPhone-ABC123",
		},
		{
			name: "explicit empty template means auto-assign",
			cfg: config.SyncConfig{
				AssetTag: config.AssetTagConfig{
					PlatformTemplates: map[string]string{"ios": ""},
				},
			},
			want: "",
		},
		{
			name: "platform match is case-insensitive",
			cfg: config.SyncConfig{
				AssetTag: config.AssetTagConfig{
					PlatformTemplates: map[string]string{"IOS": "X-{id}"},
				},
			},
			want: "X-7",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := NewEngine(nil, nil, &config.Config{Sync: c.cfg})
			got := e.assetTag(host)
			if got != c.want {
				t.Errorf("assetTag = %q, want %q", got, c.want)
			}
		})
	}
}

// Reproduces: batch sync builds Host.Raw from the list endpoint, which never
// includes end_users (IdP mapping) — that only appears in the host detail
// response. With checkout configured on end_users.0.idp_username, the engine
// must fall back to fetching the host detail instead of skipping checkout.
func TestApplyCheckoutFetchesDetailWhenUserFieldMissingFromListResponse(t *testing.T) {
	detailCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/fleet/hosts/773" {
			t.Errorf("unexpected fleet request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		detailCalls++
		_, _ = fmt.Fprint(w, `{"host":{"id":773,"hardware_serial":"FJPVJPGQTJ","platform":"darwin","end_users":[{"idp_username":"clare.ostroski@campus.edu","idp_full_name":"Clare Ostroski"}]}}`)
	}))
	defer srv.Close()

	fc, err := fleetapi.NewClient(srv.URL, "test-token", false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	cfg := &config.Config{}
	cfg.Sync.DryRun = true // dry-run counts the checkout without needing Snipe-IT
	cfg.Sync.Checkout.Enabled = true
	cfg.Sync.Checkout.UserField = "end_users.0.idp_username"
	cfg.Sync.Checkout.MatchField = "email"
	cfg.Sync.Checkout.Mode = "sync"

	e := NewEngine(fc, nil, cfg)
	e.usersByKey = map[string]int{"clare.ostroski@campus.edu": 42}

	// Host as it arrives from GET /api/v1/fleet/hosts — no end_users key.
	listJSON := `{"id":773,"hardware_serial":"FJPVJPGQTJ","platform":"darwin","computer_name":"Neo-FJPVJP"}`
	h := fleetapi.Host{ID: 773, HardwareSerial: "FJPVJPGQTJ", Platform: "darwin", Raw: []byte(listJSON)}

	logger := logrus.NewEntry(logrus.New())
	e.applyCheckout(context.Background(), h, snipeit.Asset{CommonFields: snipeit.CommonFields{ID: 1452}}, 0, logger)

	if e.stats.CheckoutsApplied != 1 {
		t.Errorf("CheckoutsApplied = %d, want 1 (skipped=%d); engine should fetch host detail for end_users", e.stats.CheckoutsApplied, e.stats.CheckoutsSkipped)
	}
	if detailCalls == 0 {
		t.Error("expected a fallback GET /hosts/773 detail request, got none")
	}
}
