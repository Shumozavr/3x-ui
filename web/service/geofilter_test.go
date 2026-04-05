package service

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xrayRouter "github.com/xtls/xray-core/app/router"
	"google.golang.org/protobuf/proto"
)

// happURL encodes a map as base64 JSON and returns a happ routing URL.
func happURL(t *testing.T, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("happURL marshal: %v", err)
	}
	return "happ://routing/onadd/" + base64.StdEncoding.EncodeToString(b)
}

// makeGeoIPDat serialises a GeoIPList to protobuf bytes.
func makeGeoIPDat(t *testing.T, entries []*xrayRouter.GeoIP) []byte {
	t.Helper()
	data, err := proto.Marshal(&xrayRouter.GeoIPList{Entry: entries})
	if err != nil {
		t.Fatalf("makeGeoIPDat: %v", err)
	}
	return data
}

// makeGeoSiteDat serialises a GeoSiteList to protobuf bytes.
func makeGeoSiteDat(t *testing.T, entries []*xrayRouter.GeoSite) []byte {
	t.Helper()
	data, err := proto.Marshal(&xrayRouter.GeoSiteList{Entry: entries})
	if err != nil {
		t.Fatalf("makeGeoSiteDat: %v", err)
	}
	return data
}

// --- ParseHappRoutingURL ---

func TestParseHappRoutingURL_Valid(t *testing.T) {
	payload := map[string]any{
		"Geoipurl":    "https://example.com/geoip.dat",
		"Geositeurl":  "https://example.com/geosite.dat",
		"DirectSites": []string{"geosite:private"},
		"DirectIp":    []string{"geoip:ru"},
		"ProxySites":  []string{"geosite:TELEGRAM"},
		"ProxyIp":     []string{"geoip:TELEGRAM"},
		"BlockSites":  []string{},
		"BlockIp":     []string{},
	}
	raw := happURL(t, payload)

	svc := GeoFilterService{}
	cfg, allFields, err := svc.ParseHappRoutingURL(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Geoipurl != "https://shumozavr.ru:3426/geodata/geoip.dat" {
		t.Errorf("Geoipurl = %q", cfg.Geoipurl)
	}
	if cfg.Geositeurl != "https://shumozavr.ru:3426/geodata/geosite.dat" {
		t.Errorf("Geositeurl = %q", cfg.Geositeurl)
	}
	if len(cfg.DirectSites) != 1 || cfg.DirectSites[0] != "geosite:private" {
		t.Errorf("DirectSites = %v", cfg.DirectSites)
	}
	if allFields == nil {
		t.Error("allFields should not be nil")
	}
}

func TestParseHappRoutingURL_NotHapp(t *testing.T) {
	svc := GeoFilterService{}
	_, _, err := svc.ParseHappRoutingURL("https://example.com/not-happ")
	if err == nil {
		t.Error("expected error for non-happ URL")
	}
}

func TestParseHappRoutingURL_BadBase64(t *testing.T) {
	svc := GeoFilterService{}
	_, _, err := svc.ParseHappRoutingURL("happ://routing/onadd/!!!notbase64!!!")
	if err == nil {
		t.Error("expected error for invalid base64")
	}
}

func TestParseHappRoutingURL_BadJSON(t *testing.T) {
	bad := "happ://routing/onadd/" + base64.StdEncoding.EncodeToString([]byte("not-json"))
	svc := GeoFilterService{}
	_, _, err := svc.ParseHappRoutingURL(bad)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseHappRoutingURL_URLEncoding(t *testing.T) {
	payload := map[string]any{"Geoipurl": "https://x.com/g.dat", "Geositeurl": "https://x.com/gs.dat"}
	b, _ := json.Marshal(payload)
	raw := "happ://routing/onadd/" + base64.URLEncoding.EncodeToString(b)
	svc := GeoFilterService{}
	cfg, _, err := svc.ParseHappRoutingURL(raw)
	if err != nil {
		t.Fatalf("URLEncoding: %v", err)
	}
	if cfg.Geoipurl != "https://x.com/g.dat" {
		t.Errorf("Geoipurl = %q", cfg.Geoipurl)
	}
}

func TestParseHappRoutingURL_PreservesExtraFields(t *testing.T) {
	payload := map[string]any{
		"Geoipurl":   "https://x.com/g.dat",
		"Geositeurl": "https://x.com/gs.dat",
		"CustomKey":  "custom-value",
	}
	raw := happURL(t, payload)
	svc := GeoFilterService{}
	_, allFields, err := svc.ParseHappRoutingURL(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := allFields["CustomKey"]; !ok {
		t.Error("allFields should preserve extra keys")
	}
}

// --- extractCategories ---

func TestExtractCategories_Basic(t *testing.T) {
	entries := []string{"geoip:ru", "geoip:TELEGRAM", "geoip:private", "spinorama.org"}
	cats := extractCategories(entries, "geoip")
	want := map[string]bool{"RU": true, "TELEGRAM": true, "PRIVATE": true}
	for k := range want {
		if !cats[k] {
			t.Errorf("missing category %q", k)
		}
	}
	if cats["SPINORAMA.ORG"] {
		t.Error("plain domain should not be treated as geoip category")
	}
}

func TestExtractCategories_WrongPrefix(t *testing.T) {
	cats := extractCategories([]string{"geosite:private"}, "geoip")
	if len(cats) != 0 {
		t.Errorf("expected empty, got %v", cats)
	}
}

func TestExtractCategories_CaseInsensitiveMatch(t *testing.T) {
	cats := extractCategories([]string{"GEOIP:RU", "GeoIp:Cn"}, "geoip")
	if !cats["RU"] || !cats["CN"] {
		t.Errorf("case-insensitive match failed: %v", cats)
	}
}

func TestExtractCategories_Empty(t *testing.T) {
	cats := extractCategories(nil, "geoip")
	if len(cats) != 0 {
		t.Error("expected empty map for nil input")
	}
}

// --- filterGeoIP / filterGeoSite (via httptest server) ---

func TestFilterGeoIP(t *testing.T) {
	entries := []*xrayRouter.GeoIP{
		{CountryCode: "RU"},
		{CountryCode: "TELEGRAM"},
		{CountryCode: "CN"},
	}
	dat := makeGeoIPDat(t, entries)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(dat)
	}))
	defer srv.Close()

	cats := map[string]bool{"RU": true, "TELEGRAM": true}
	out, count, err := filterGeoIP(srv.URL+"/geoip.dat", cats)
	if err != nil {
		t.Fatalf("filterGeoIP: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 entries, got %d", count)
	}

	result := &xrayRouter.GeoIPList{}
	if err := proto.Unmarshal(out, result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	for _, e := range result.Entry {
		if e.CountryCode == "CN" {
			t.Error("CN should have been filtered out")
		}
	}
}

func TestFilterGeoSite(t *testing.T) {
	entries := []*xrayRouter.GeoSite{
		{CountryCode: "PRIVATE"},
		{CountryCode: "TELEGRAM"},
		{CountryCode: "CATEGORY-RU"},
	}
	dat := makeGeoSiteDat(t, entries)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(dat)
	}))
	defer srv.Close()

	cats := map[string]bool{"PRIVATE": true, "TELEGRAM": true}
	out, count, err := filterGeoSite(srv.URL+"/geosite.dat", cats)
	if err != nil {
		t.Fatalf("filterGeoSite: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 entries, got %d", count)
	}

	result := &xrayRouter.GeoSiteList{}
	if err := proto.Unmarshal(out, result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	for _, e := range result.Entry {
		if e.CountryCode == "CATEGORY-RU" {
			t.Error("CATEGORY-RU should have been filtered out")
		}
	}
}

func TestFilterGeoIP_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := filterGeoIP(srv.URL+"/geoip.dat", map[string]bool{"RU": true})
	if err == nil {
		t.Error("expected error for HTTP 404")
	}
}

func TestFilterGeoIP_InvalidProtobuf(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not-protobuf"))
	}))
	defer srv.Close()

	_, _, err := filterGeoIP(srv.URL+"/geoip.dat", map[string]bool{"RU": true})
	if err == nil {
		t.Error("expected error for invalid protobuf")
	}
}

// --- ProcessGeoFiles (integration, using httptest servers and temp dir) ---

func TestProcessGeoFiles_UpdatesURLsAndSavesFiles(t *testing.T) {
	// Build minimal dat files
	geoipDat := makeGeoIPDat(t, []*xrayRouter.GeoIP{
		{CountryCode: "RU"},
		{CountryCode: "TELEGRAM"},
		{CountryCode: "CN"},
	})
	geositeDat := makeGeoSiteDat(t, []*xrayRouter.GeoSite{
		{CountryCode: "PRIVATE"},
		{CountryCode: "TELEGRAM"},
		{CountryCode: "CATEGORY-RU"},
	})

	// Serve the dat files
	datSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "geoip.dat") {
			w.Write(geoipDat)
		} else {
			w.Write(geositeDat)
		}
	}))
	defer datSrv.Close()

	payload := map[string]any{
		"Geoipurl":    datSrv.URL + "/geoip.dat",
		"Geositeurl":  datSrv.URL + "/geosite.dat",
		"DirectSites": []string{"geosite:private", "geosite:CATEGORY-RU"},
		"DirectIp":    []string{"geoip:ru"},
		"ProxySites":  []string{"geosite:TELEGRAM"},
		"ProxyIp":     []string{"geoip:TELEGRAM"},
		"BlockSites":  []string{},
		"BlockIp":     []string{},
	}
	rawURL := happURL(t, payload)

	// Point bin folder to a temp dir so we don't write to real bin/
	tmpDir := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", tmpDir)

	subBase := "http://sub.example.com:2096"
	svc := GeoFilterService{}
	newURL, info, err := svc.ProcessGeoFiles(rawURL, subBase)
	if err != nil {
		t.Fatalf("ProcessGeoFiles: %v", err)
	}

	// Info sanity checks
	if info.GeoipCategories != 2 {
		t.Errorf("geoipCategories = %d, want 2", info.GeoipCategories)
	}
	if info.GeositeCategories != 3 {
		t.Errorf("geositeCategories = %d, want 3", info.GeositeCategories)
	}
	if info.GeoipSize <= 0 {
		t.Error("geoipSize should be > 0")
	}
	if info.GeositeSize <= 0 {
		t.Error("geositeSize should be > 0")
	}
	if info.ProcessedAt == "" {
		t.Error("processedAt should be set")
	}

	// Files must exist in bin dir
	if _, err := os.Stat(filepath.Join(tmpDir, "sub_geoip.dat")); err != nil {
		t.Errorf("sub_geoip.dat not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "sub_geosite.dat")); err != nil {
		t.Errorf("sub_geosite.dat not found: %v", err)
	}

	// Returned URL must contain updated geo file URLs
	idx := strings.LastIndex(newURL, "/")
	decoded, err := base64.StdEncoding.DecodeString(newURL[idx+1:])
	if err != nil {
		t.Fatalf("decode new URL base64: %v", err)
	}
	var updatedPayload map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &updatedPayload); err != nil {
		t.Fatalf("unmarshal updated payload: %v", err)
	}
	var geoipURL string
	json.Unmarshal(updatedPayload["Geoipurl"], &geoipURL)
	if geoipURL != subBase+"/geodata/geoip.dat" {
		t.Errorf("Geoipurl = %q, want %q", geoipURL, subBase+"/geodata/geoip.dat")
	}
}

func TestProcessGeoFiles_NoSubBaseURL_PreservesOriginalURLs(t *testing.T) {
	geoipDat := makeGeoIPDat(t, []*xrayRouter.GeoIP{{CountryCode: "RU"}})
	geositeDat := makeGeoSiteDat(t, []*xrayRouter.GeoSite{{CountryCode: "PRIVATE"}})

	datSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "geoip.dat") {
			w.Write(geoipDat)
		} else {
			w.Write(geositeDat)
		}
	}))
	defer datSrv.Close()

	origGeoipURL := datSrv.URL + "/geoip.dat"
	payload := map[string]any{
		"Geoipurl":    origGeoipURL,
		"Geositeurl":  datSrv.URL + "/geosite.dat",
		"DirectSites": []string{"geosite:private"},
		"DirectIp":    []string{"geoip:ru"},
		"ProxySites":  []string{},
		"ProxyIp":     []string{},
		"BlockSites":  []string{},
		"BlockIp":     []string{},
	}
	rawURL := happURL(t, payload)

	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	svc := GeoFilterService{}
	newURL, _, err := svc.ProcessGeoFiles(rawURL, "") // empty subBaseURL
	if err != nil {
		t.Fatalf("ProcessGeoFiles: %v", err)
	}

	// URL should be unchanged when subBaseURL is empty
	if newURL != rawURL {
		t.Error("routing URL should be unchanged when subBaseURL is empty")
	}
}

func TestProcessGeoFiles_InvalidURL(t *testing.T) {
	svc := GeoFilterService{}
	_, _, err := svc.ProcessGeoFiles("not-a-happ-url", "http://sub.example.com")
	if err == nil {
		t.Error("expected error for invalid happ URL")
	}
}
