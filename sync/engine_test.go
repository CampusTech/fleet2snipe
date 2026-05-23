package sync

import (
	"testing"

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
		{"bytes 8 GiB", `{"v": 8589934592}`, "bytes_to_gb", "9"},      // 8.59 → 9
		{"bytes 16 GiB", `{"v": 17179869184}`, "bytes_to_gb", "17"},   // 17.18 → 17
		{"bytes 32 GiB", `{"v": 34359738368}`, "bytes_to_gb", "34"},   // 34.36 → 34
		{"bytes 64 GiB", `{"v": 68719476736}`, "bytes_to_gb", "69"},   // 68.72 → 69
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
