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
