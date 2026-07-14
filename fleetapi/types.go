// Package fleetapi is a minimal hand-rolled Fleet (fleetdm.com) REST client
// covering the endpoints fleet2snipe needs: list hosts (paged), get host by
// ID or identifier (uuid/serial/hostname), list labels, list teams, and a
// trivial /version ping. We intentionally avoid importing the upstream
// github.com/fleetdm/fleet/v4 module — it drags in hundreds of indirect deps.
package fleetapi

import (
	"encoding/json"
	"time"
)

// NeverTimestamp is Fleet's sentinel for timestamps that have never been set
// (fleet.NeverTimestamp upstream). Fleet reports it, e.g., as detail_updated_at
// for hosts whose details have never been fetched.
var NeverTimestamp = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Host is the subset of fields fleet2snipe consumes from /api/v1/fleet/hosts.
// Fields appear in both the list and detail responses unless noted otherwise.
// Unknown fields decode silently; gjson is used on the raw bytes for anything
// not covered here (custom mappings can reach into nested structures).
type Host struct {
	ID                        uint      `json:"id"`
	Hostname                  string    `json:"hostname"`
	ComputerName              string    `json:"computer_name"`
	DisplayName               string    `json:"display_name"`
	UUID                      string    `json:"uuid"`
	HardwareSerial            string    `json:"hardware_serial"`
	HardwareModel             string    `json:"hardware_model"`
	HardwareVendor            string    `json:"hardware_vendor"`
	HardwareVersion           string    `json:"hardware_version"`
	Platform                  string    `json:"platform"`
	OSVersion                 string    `json:"os_version"`
	Build                     string    `json:"build"`
	OsqueryVersion            string    `json:"osquery_version"`
	OrbitVersion              string    `json:"orbit_version"`
	LastEnrolledAt            time.Time `json:"last_enrolled_at"`
	DetailUpdatedAt           time.Time `json:"detail_updated_at"`
	SeenTime                  time.Time `json:"seen_time"`
	LastRestartedAt           time.Time `json:"last_restarted_at"`
	Status                    string    `json:"status"`
	PrimaryIP                 string    `json:"primary_ip"`
	PrimaryMAC                string    `json:"primary_mac"`
	PublicIP                  string    `json:"public_ip"`
	CPUBrand                  string    `json:"cpu_brand"`
	CPUType                   string    `json:"cpu_type"`
	CPUPhysicalCores          int       `json:"cpu_physical_cores"`
	CPULogicalCores           int       `json:"cpu_logical_cores"`
	Memory                    int64     `json:"memory"`
	GigsDiskSpaceAvailable    float64   `json:"gigs_disk_space_available"`
	PercentDiskSpaceAvailable float64   `json:"percent_disk_space_available"`
	GigsTotalDiskSpace        float64   `json:"gigs_total_disk_space"`
	DiskEncryptionEnabled     *bool     `json:"disk_encryption_enabled"` // detail only
	TeamID                    *uint     `json:"team_id"`
	TeamName                  string    `json:"team_name"`
	MDM                       MDM       `json:"mdm"`
	Issues                    Issues    `json:"issues"`
	Policies                  []Policy  `json:"policies"` // populated when populate_policies=true (list) or via GET /hosts/{id} (detail)
	Labels                    []Label   `json:"labels"`   // populated when populate_labels=true (list) or via GET /hosts/{id} (detail)

	// Raw is the full JSON object as returned by Fleet. The sync engine reads
	// custom field_mapping paths via gjson against this — keeps the Host struct
	// stable while still allowing arbitrary mappings.
	Raw json.RawMessage `json:"-"`
}

// DetailsFetched reports whether Fleet has ever refreshed this host's details.
// False when detail_updated_at is missing or is Fleet's NeverTimestamp sentinel.
func (h Host) DetailsFetched() bool {
	return !h.DetailUpdatedAt.IsZero() && !h.DetailUpdatedAt.Equal(NeverTimestamp)
}

// MDM is Fleet's mdm sub-object.
type MDM struct {
	EnrollmentStatus       string `json:"enrollment_status"`
	Name                   string `json:"name"`
	ServerURL              string `json:"server_url"`
	ConnectedToFleet       bool   `json:"connected_to_fleet"`
	EncryptionKeyAvailable bool   `json:"encryption_key_available"`
}

// Issues holds policy/issue counts surfaced in list responses.
type Issues struct {
	FailingPoliciesCount int `json:"failing_policies_count"`
	TotalIssuesCount     int `json:"total_issues_count"`
}

// listHostsResponse is the envelope returned by GET /hosts.
type listHostsResponse struct {
	Hosts []json.RawMessage `json:"hosts"`
}

// hostDetailResponse wraps GET /hosts/{id}.
type hostDetailResponse struct {
	Host json.RawMessage `json:"host"`
}

// Policy is a Fleet policy attached to a host. Response is "pass", "fail",
// or "" (never evaluated). Critical indicates a high-severity policy.
type Policy struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	Query       string `json:"query"`
	Description string `json:"description"`
	Response    string `json:"response"`
	Critical    bool   `json:"critical"`
	Resolution  string `json:"resolution"`
	Platform    string `json:"platform"`
}

// Query is a Fleet saved query.
type Query struct {
	ID                 uint   `json:"id"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	Query              string `json:"query"`
	TeamID             *uint  `json:"team_id"`
	Interval           int    `json:"interval"`
	Platform           string `json:"platform"`
	DiscardData        bool   `json:"discard_data"`
	AutomationsEnabled bool   `json:"automations_enabled"`
}

type listQueriesResponse struct {
	Queries []Query `json:"queries"`
}

// QueryReportRow is one host's result row for a saved query.
type QueryReportRow struct {
	HostID      uint              `json:"host_id"`
	HostName    string            `json:"host_name"`
	LastFetched time.Time         `json:"last_fetched"`
	Columns     map[string]string `json:"columns"`
}

type queryReportResponse struct {
	QueryID uint             `json:"query_id"`
	Results []QueryReportRow `json:"results"`
}

// Label is the subset of fields we read from /labels.
type Label struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	LabelType   string `json:"label_type"`
	Platform    string `json:"platform"`
}

type listLabelsResponse struct {
	Labels []Label `json:"labels"`
}

// Team mirrors Fleet's team object (Premium feature).
type Team struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

type listTeamsResponse struct {
	Teams []Team `json:"teams"`
}

// Version is the response from GET /api/latest/fleet/version.
type Version struct {
	Version  string `json:"version"`
	Branch   string `json:"branch"`
	Revision string `json:"revision"`
	GoVer    string `json:"go_version"`
}
