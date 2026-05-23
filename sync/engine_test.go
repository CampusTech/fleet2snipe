package sync

import (
	"testing"

	"github.com/CampusTech/fleet2snipe/config"
	"github.com/CampusTech/fleet2snipe/fleetapi"
)

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
