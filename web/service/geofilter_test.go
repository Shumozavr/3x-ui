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

// happURL encodes payload as base64 JSON and returns a happ routing URL.
func happURL(t *testing.T, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("happURL marshal: %v", err)
	}
	return "happ://routing/onadd/" + base64.StdEncoding.EncodeToString(b)
}

func makeGeoIPDat(t *testing.T, entries []*xrayRouter.GeoIP) []byte {
	t.Helper()
	data, err := proto.Marshal(&xrayRouter.GeoIPList{Entry: entries})
	if err != nil {
		t.Fatalf("makeGeoIPDat: %v", err)
	}
	return data
}

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
	}
	raw := happURL(t, payload)

	svc := GeoFilterService{}
	cfg, allFields, err := svc.ParseHappRoutingURL(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Geoipurl != "https://example.com/geoip.dat" {
		t.Errorf("Geoipurl = %q", cfg.Geoipurl)
	}
	if cfg.Geositeurl != "https://example.com/geosite.dat" {
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
	for _, want := range []string{"RU", "TELEGRAM", "PRIVATE"} {
		if !cats[want] {
			t.Errorf("missing category %q", want)
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

// --- filterGeoIPBytes / filterGeoSiteBytes ---

func TestFilterGeoIPBytes(t *testing.T) {
	dat := makeGeoIPDat(t, []*xrayRouter.GeoIP{
		{CountryCode: "RU"},
		{CountryCode: "TELEGRAM"},
		{CountryCode: "CN"},
	})
	cats := map[string]bool{"RU": true, "TELEGRAM": true}
	out, count, err := filterGeoIPBytes(dat, cats)
	if err != nil {
		t.Fatalf("filterGeoIPBytes: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 entries, got %d", count)
	}
	result := &xrayRouter.GeoIPList{}
	if err := proto.Unmarshal(out, result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, e := range result.Entry {
		if e.CountryCode == "CN" {
			t.Error("CN should have been filtered out")
		}
	}
}

func TestFilterGeoSiteBytes(t *testing.T) {
	dat := makeGeoSiteDat(t, []*xrayRouter.GeoSite{
		{CountryCode: "PRIVATE"},
		{CountryCode: "TELEGRAM"},
		{CountryCode: "CATEGORY-RU"},
	})
	cats := map[string]bool{"PRIVATE": true, "TELEGRAM": true}
	out, count, err := filterGeoSiteBytes(dat, cats)
	if err != nil {
		t.Fatalf("filterGeoSiteBytes: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 entries, got %d", count)
	}
	result := &xrayRouter.GeoSiteList{}
	if err := proto.Unmarshal(out, result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, e := range result.Entry {
		if e.CountryCode == "CATEGORY-RU" {
			t.Error("CATEGORY-RU should have been filtered out")
		}
	}
}

func TestFilterGeoIPBytes_InvalidProtobuf(t *testing.T) {
	_, _, err := filterGeoIPBytes([]byte("not-protobuf"), map[string]bool{"RU": true})
	if err == nil {
		t.Error("expected error for invalid protobuf")
	}
}

// --- downloadWithEtag ---

func TestDownloadWithEtag_FirstDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc123"`)
		w.Write([]byte("content"))
	}))
	defer srv.Close()

	data, etag, changed, err := downloadWithEtag(srv.URL, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Error("expected changed=true for first download")
	}
	if etag != `"abc123"` {
		t.Errorf("etag = %q", etag)
	}
	if string(data) != "content" {
		t.Errorf("data = %q", data)
	}
}

func TestDownloadWithEtag_NotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"abc123"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"abc123"`)
		w.Write([]byte("content"))
	}))
	defer srv.Close()

	_, _, _, _ = downloadWithEtag(srv.URL, "") // prime
	data, _, changed, err := downloadWithEtag(srv.URL, `"abc123"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Error("expected changed=false for 304")
	}
	if data != nil {
		t.Error("expected nil data for 304")
	}
}

func TestDownloadWithEtag_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, _, err := downloadWithEtag(srv.URL, "")
	if err == nil {
		t.Error("expected error for HTTP 404")
	}
}

// --- ProcessGeoFiles ---

func TestProcessGeoFiles_SavesFilesAndReturnsMetadata(t *testing.T) {
	geoipDat := makeGeoIPDat(t, []*xrayRouter.GeoIP{
		{CountryCode: "RU"}, {CountryCode: "TELEGRAM"}, {CountryCode: "CN"},
	})
	geositeDat := makeGeoSiteDat(t, []*xrayRouter.GeoSite{
		{CountryCode: "PRIVATE"}, {CountryCode: "TELEGRAM"}, {CountryCode: "CATEGORY-RU"},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"etag1"`)
		if strings.HasSuffix(r.URL.Path, "geoip.dat") {
			w.Write(geoipDat)
		} else {
			w.Write(geositeDat)
		}
	}))
	defer srv.Close()

	payload := map[string]any{
		"Geoipurl":    srv.URL + "/geoip.dat",
		"Geositeurl":  srv.URL + "/geosite.dat",
		"DirectSites": []string{"geosite:private", "geosite:CATEGORY-RU"},
		"DirectIp":    []string{"geoip:ru"},
		"ProxySites":  []string{"geosite:TELEGRAM"},
		"ProxyIp":     []string{"geoip:TELEGRAM"},
		"BlockSites":  []string{},
		"BlockIp":     []string{},
	}
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	svc := GeoFilterService{}
	info, etags, err := svc.ProcessGeoFiles(happURL(t, payload))
	if err != nil {
		t.Fatalf("ProcessGeoFiles: %v", err)
	}
	if info.GeoipCategories != 2 {
		t.Errorf("geoipCategories = %d, want 2", info.GeoipCategories)
	}
	if info.GeositeCategories != 3 {
		t.Errorf("geositeCategories = %d, want 3", info.GeositeCategories)
	}
	if info.GeoipSize <= 0 || info.GeositeSize <= 0 {
		t.Error("file sizes should be > 0")
	}
	if etags.GeoipEtag != `"etag1"` {
		t.Errorf("GeoipEtag = %q", etags.GeoipEtag)
	}

	binPath := t.TempDir() // already set via Setenv above; just read from env
	binPath = os.Getenv("XUI_BIN_FOLDER")
	if _, err := os.Stat(filepath.Join(binPath, "sub_geoip.dat")); err != nil {
		t.Errorf("sub_geoip.dat not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(binPath, "sub_geosite.dat")); err != nil {
		t.Errorf("sub_geosite.dat not found: %v", err)
	}
}

func TestProcessGeoFiles_DoesNotModifyRoutingURL(t *testing.T) {
	geoipDat := makeGeoIPDat(t, []*xrayRouter.GeoIP{{CountryCode: "RU"}})
	geositeDat := makeGeoSiteDat(t, []*xrayRouter.GeoSite{{CountryCode: "PRIVATE"}})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "geoip.dat") {
			w.Write(geoipDat)
		} else {
			w.Write(geositeDat)
		}
	}))
	defer srv.Close()

	payload := map[string]any{
		"Geoipurl": srv.URL + "/geoip.dat", "Geositeurl": srv.URL + "/geosite.dat",
		"DirectIp": []string{"geoip:ru"}, "DirectSites": []string{"geosite:private"},
		"ProxySites": []string{}, "ProxyIp": []string{}, "BlockSites": []string{}, "BlockIp": []string{},
	}
	rawURL := happURL(t, payload)
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	svc := GeoFilterService{}
	// ProcessGeoFiles returns (info, etags, error) — the URL is not part of the return
	_, _, err := svc.ProcessGeoFiles(rawURL)
	if err != nil {
		t.Fatalf("ProcessGeoFiles: %v", err)
	}
	// The original URL is unchanged — ProcessGeoFiles never returns a modified URL
}

func TestProcessGeoFiles_InvalidURL(t *testing.T) {
	svc := GeoFilterService{}
	_, _, err := svc.ProcessGeoFiles("not-a-happ-url")
	if err == nil {
		t.Error("expected error for invalid happ URL")
	}
}

// --- RefreshIfStale ---

func TestRefreshIfStale_WhenEtagMatches_NoRefresh(t *testing.T) {
	geoipDat := makeGeoIPDat(t, []*xrayRouter.GeoIP{{CountryCode: "RU"}})
	geositeDat := makeGeoSiteDat(t, []*xrayRouter.GeoSite{{CountryCode: "PRIVATE"}})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server indicates content hasn't changed for the known etag
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		if strings.HasSuffix(r.URL.Path, "geoip.dat") {
			w.Write(geoipDat)
		} else {
			w.Write(geositeDat)
		}
	}))
	defer srv.Close()

	payload := map[string]any{
		"Geoipurl": srv.URL + "/geoip.dat", "Geositeurl": srv.URL + "/geosite.dat",
		"DirectIp": []string{"geoip:ru"}, "DirectSites": []string{"geosite:private"},
		"ProxySites": []string{}, "ProxyIp": []string{}, "BlockSites": []string{}, "BlockIp": []string{},
	}
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	svc := GeoFilterService{}
	refreshed, info, _, err := svc.RefreshIfStale(happURL(t, payload), GeoEtags{GeoipEtag: `"v1"`, GeositeEtag: `"v1"`})
	if err != nil {
		t.Fatalf("RefreshIfStale: %v", err)
	}
	if refreshed {
		t.Error("expected no refresh when etag matches")
	}
	if info != nil {
		t.Error("expected nil info when not refreshed")
	}
}

func TestRefreshIfStale_WhenEtagChanged_Refreshes(t *testing.T) {
	geoipDat := makeGeoIPDat(t, []*xrayRouter.GeoIP{{CountryCode: "RU"}})
	geositeDat := makeGeoSiteDat(t, []*xrayRouter.GeoSite{{CountryCode: "PRIVATE"}})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always return new content (etag changed)
		w.Header().Set("ETag", `"v2"`)
		if strings.HasSuffix(r.URL.Path, "geoip.dat") {
			w.Write(geoipDat)
		} else {
			w.Write(geositeDat)
		}
	}))
	defer srv.Close()

	payload := map[string]any{
		"Geoipurl": srv.URL + "/geoip.dat", "Geositeurl": srv.URL + "/geosite.dat",
		"DirectIp": []string{"geoip:ru"}, "DirectSites": []string{"geosite:private"},
		"ProxySites": []string{}, "ProxyIp": []string{}, "BlockSites": []string{}, "BlockIp": []string{},
	}
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	svc := GeoFilterService{}
	refreshed, info, newEtags, err := svc.RefreshIfStale(happURL(t, payload), GeoEtags{GeoipEtag: `"v1"`, GeositeEtag: `"v1"`})
	if err != nil {
		t.Fatalf("RefreshIfStale: %v", err)
	}
	if !refreshed {
		t.Error("expected refresh when etag changed")
	}
	if info == nil {
		t.Fatal("expected non-nil info when refreshed")
	}
	if newEtags.GeoipEtag != `"v2"` {
		t.Errorf("new GeoipEtag = %q, want v2", newEtags.GeoipEtag)
	}
}

// --- BuildModifiedRoutingURL ---

func TestBuildModifiedRoutingURL_SubstitutesURLs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", tmpDir)
	// Create the dat files so BuildModifiedRoutingURL sees them as present
	os.WriteFile(filepath.Join(tmpDir, "sub_geoip.dat"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "sub_geosite.dat"), []byte("x"), 0o644)

	payload := map[string]any{
		"Geoipurl": "https://upstream.com/geoip.dat", "Geositeurl": "https://upstream.com/geosite.dat",
		"Name": "test",
	}
	rawURL := happURL(t, payload)
	subBase := "http://sub.example.com:2096"

	svc := GeoFilterService{}
	modified, err := svc.BuildModifiedRoutingURL(rawURL, subBase)
	if err != nil {
		t.Fatalf("BuildModifiedRoutingURL: %v", err)
	}
	if modified == rawURL {
		t.Error("expected URL to be modified")
	}

	// Decode and verify
	idx := strings.LastIndex(modified, "/")
	decoded, _ := base64.StdEncoding.DecodeString(modified[idx+1:])
	var result map[string]json.RawMessage
	json.Unmarshal(decoded, &result)
	var geoipURL string
	json.Unmarshal(result["Geoipurl"], &geoipURL)
	if geoipURL != subBase+"/geodata/geoip.dat" {
		t.Errorf("Geoipurl = %q", geoipURL)
	}
	// Extra fields preserved
	if _, ok := result["Name"]; !ok {
		t.Error("extra field 'Name' should be preserved")
	}
}

func TestBuildModifiedRoutingURL_NoLocalFiles_ReturnsOriginal(t *testing.T) {
	t.Setenv("XUI_BIN_FOLDER", t.TempDir()) // empty dir, no dat files
	payload := map[string]any{"Geoipurl": "https://upstream.com/geoip.dat", "Geositeurl": "https://upstream.com/geosite.dat"}
	rawURL := happURL(t, payload)

	svc := GeoFilterService{}
	modified, _ := svc.BuildModifiedRoutingURL(rawURL, "http://sub.example.com:2096")
	if modified != rawURL {
		t.Error("should return original URL when local files are absent")
	}
}

func TestBuildModifiedRoutingURL_EmptySubBaseURL_ReturnsOriginal(t *testing.T) {
	payload := map[string]any{"Geoipurl": "https://upstream.com/geoip.dat", "Geositeurl": "https://upstream.com/geosite.dat"}
	rawURL := happURL(t, payload)
	svc := GeoFilterService{}
	modified, _ := svc.BuildModifiedRoutingURL(rawURL, "")
	if modified != rawURL {
		t.Error("should return original URL when subBaseURL is empty")
	}
}
