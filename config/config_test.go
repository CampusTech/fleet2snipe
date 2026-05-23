package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFieldMappingEntry_AcceptsBareString(t *testing.T) {
	data := []byte(`field_mapping:
  _snipeit_host_id_1: id
  _snipeit_os_2: os_version
`)
	var s SyncConfig
	if err := yaml.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := s.FieldMapping["_snipeit_host_id_1"]; got.Path != "id" || got.Transform != "" {
		t.Errorf("bare string entry decoded wrong: %+v", got)
	}
	if got := s.FieldMapping["_snipeit_os_2"]; got.Path != "os_version" || got.Transform != "" {
		t.Errorf("bare string entry decoded wrong: %+v", got)
	}
}

func TestFieldMappingEntry_AcceptsObjectForm(t *testing.T) {
	data := []byte(`field_mapping:
  _snipeit_ram_2:
    path: memory
    transform: bytes_to_gb
  _snipeit_storage_3:
    path: gigs_total_disk_space
    transform: gib_to_gb
`)
	var s SyncConfig
	if err := yaml.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := s.FieldMapping["_snipeit_ram_2"]; got.Path != "memory" || got.Transform != "bytes_to_gb" {
		t.Errorf("object form decoded wrong: %+v", got)
	}
	if got := s.FieldMapping["_snipeit_storage_3"]; got.Path != "gigs_total_disk_space" || got.Transform != "gib_to_gb" {
		t.Errorf("object form decoded wrong: %+v", got)
	}
}

func TestFieldMappingEntry_MixedShapes(t *testing.T) {
	data := []byte(`field_mapping:
  _snipeit_host_id_1: id
  _snipeit_ram_2:
    path: memory
    transform: bytes_to_gb
`)
	var s SyncConfig
	if err := yaml.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.FieldMapping["_snipeit_host_id_1"].Path != "id" {
		t.Errorf("string-form entry: %+v", s.FieldMapping["_snipeit_host_id_1"])
	}
	if s.FieldMapping["_snipeit_ram_2"].Transform != "bytes_to_gb" {
		t.Errorf("object-form entry: %+v", s.FieldMapping["_snipeit_ram_2"])
	}
}

func TestMergedFieldMapping(t *testing.T) {
	cfg := SyncConfig{
		FieldMapping: map[string]FieldMappingEntry{
			"_snipeit_id_1": {Path: "id"},
			"_snipeit_os_2": {Path: "os_version"},
		},
		PerPlatform: map[string]PlatformMappings{
			"darwin": {
				FieldMapping: map[string]FieldMappingEntry{
					"_snipeit_fv_5": {Path: "host.disk_encryption_enabled", Transform: "bool_yes_no"},
					"_snipeit_os_2": {Path: "os_version", Transform: "uppercase"}, // overrides global
				},
			},
			"ios": {
				FieldMapping: map[string]FieldMappingEntry{
					"_snipeit_uuid_7": {Path: "uuid"},
				},
			},
		},
	}

	t.Run("unknown platform returns global only", func(t *testing.T) {
		m := cfg.MergedFieldMapping("freebsd")
		if len(m) != 2 || m["_snipeit_id_1"].Path != "id" {
			t.Errorf("got %+v", m)
		}
	})

	t.Run("platform adds and overrides", func(t *testing.T) {
		m := cfg.MergedFieldMapping("darwin")
		if len(m) != 3 {
			t.Errorf("expected 3 entries, got %d: %+v", len(m), m)
		}
		if m["_snipeit_os_2"].Transform != "uppercase" {
			t.Errorf("platform override didn't win: %+v", m["_snipeit_os_2"])
		}
		if m["_snipeit_fv_5"].Path != "host.disk_encryption_enabled" {
			t.Errorf("darwin field missing: %+v", m["_snipeit_fv_5"])
		}
	})

	t.Run("case-insensitive platform key", func(t *testing.T) {
		m := cfg.MergedFieldMapping("DARWIN")
		if _, ok := m["_snipeit_fv_5"]; !ok {
			t.Error("case-insensitive lookup failed")
		}
	})

	t.Run("ios only sees ios overrides plus globals", func(t *testing.T) {
		m := cfg.MergedFieldMapping("ios")
		if _, ok := m["_snipeit_fv_5"]; ok {
			t.Error("ios should not see darwin overrides")
		}
		if _, ok := m["_snipeit_uuid_7"]; !ok {
			t.Error("ios override missing")
		}
		if m["_snipeit_os_2"].Transform != "" {
			t.Error("ios should get global os_version, not darwin override")
		}
	})

	t.Run("returned map is independent", func(t *testing.T) {
		m := cfg.MergedFieldMapping("darwin")
		m["_snipeit_id_1"] = FieldMappingEntry{Path: "tampered"}
		if cfg.FieldMapping["_snipeit_id_1"].Path != "id" {
			t.Error("caller mutation leaked into source config")
		}
	})
}

func TestAllQueryNames(t *testing.T) {
	cfg := SyncConfig{
		QueryMapping: map[string]QueryFieldMap{
			"_a": {Query: "Global Query", Column: "x"},
		},
		PerPlatform: map[string]PlatformMappings{
			"darwin": {QueryMapping: map[string]QueryFieldMap{
				"_b": {Query: "Darwin Kernel", Column: "version"},
				"_c": {Query: "Global Query", Column: "y"}, // duplicate of global
			}},
			"linux": {QueryMapping: map[string]QueryFieldMap{
				"_d": {Query: "Linux Kernel", Column: "release"},
			}},
		},
	}
	names := cfg.AllQueryNames()
	got := make(map[string]bool)
	for _, n := range names {
		got[n] = true
	}
	want := map[string]bool{"Global Query": true, "Darwin Kernel": true, "Linux Kernel": true}
	if len(got) != len(want) {
		t.Errorf("len mismatch: got %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing %q in AllQueryNames", k)
		}
	}
}

func TestValidateFieldMappingTransforms_PerPlatform(t *testing.T) {
	bad := &Config{Sync: SyncConfig{
		PerPlatform: map[string]PlatformMappings{
			"darwin": {FieldMapping: map[string]FieldMappingEntry{
				"_snipeit_x_5": {Path: "p", Transform: "bogus_xform"},
			}},
		},
	}}
	err := bad.validateFieldMappingTransforms()
	if err == nil {
		t.Fatal("expected error for unknown per-platform transform")
	}
	if !strings.Contains(err.Error(), "bogus_xform") || !strings.Contains(err.Error(), "darwin") {
		t.Errorf("error should name both transform and platform, got: %v", err)
	}
}

func TestValidateFieldMappingTransforms(t *testing.T) {
	good := &Config{Sync: SyncConfig{FieldMapping: map[string]FieldMappingEntry{
		"_snipeit_a_1": {Path: "id"},
		"_snipeit_b_2": {Path: "memory", Transform: "bytes_to_gb"},
		"_snipeit_c_3": {Path: "gigs_total_disk_space", Transform: "gib_to_gb"},
	}}}
	if err := good.validateFieldMappingTransforms(); err != nil {
		t.Errorf("expected no error for valid transforms, got: %v", err)
	}

	bad := &Config{Sync: SyncConfig{FieldMapping: map[string]FieldMappingEntry{
		"_snipeit_x_5": {Path: "memory", Transform: "kilobytes_to_petabytes"},
	}}}
	err := bad.validateFieldMappingTransforms()
	if err == nil {
		t.Fatal("expected error for unknown transform")
	}
	if !strings.Contains(err.Error(), "kilobytes_to_petabytes") {
		t.Errorf("error should name the bad transform, got: %v", err)
	}
	if !strings.Contains(err.Error(), "_snipeit_x_5") {
		t.Errorf("error should name the field, got: %v", err)
	}
}
