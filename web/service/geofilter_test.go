package service

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	xrayRouter "github.com/xtls/xray-core/app/router"
	"google.golang.org/protobuf/proto"
)

func happURL(t *testing.T, payload map[string]any) string {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal happ payload: %v", err)
	}
	return "happ://routing/onadd/" + base64.StdEncoding.EncodeToString(data)
}

func makeGeoIPDat(t *testing.T, entries []*xrayRouter.GeoIP) []byte {
	t.Helper()
	data, err := proto.Marshal(&xrayRouter.GeoIPList{Entry: entries})
	if err != nil {
		t.Fatalf("marshal geoip dat: %v", err)
	}
	return data
}

func makeGeoSiteDat(t *testing.T, entries []*xrayRouter.GeoSite) []byte {
	t.Helper()
	data, err := proto.Marshal(&xrayRouter.GeoSiteList{Entry: entries})
	if err != nil {
		t.Fatalf("marshal geosite dat: %v", err)
	}
	return data
}

func TestParseHappRoutingURLValid(t *testing.T) {
	raw := happURL(t, map[string]any{
		"Geoipurl":    "https://example.com/geoip.dat",
		"Geositeurl":  "https://example.com/geosite.dat",
		"DirectSites": []string{"geosite:private"},
		"DirectIp":    []string{"geoip:ru"},
		"CustomKey":   "custom-value",
	})

	cfg, fields, err := (&GeoFilterService{}).ParseHappRoutingURL(raw)
	if err != nil {
		t.Fatalf("ParseHappRoutingURL: %v", err)
	}
	if cfg.Geoipurl != "https://example.com/geoip.dat" || cfg.Geositeurl != "https://example.com/geosite.dat" {
		t.Fatalf("unexpected parsed URLs: %+v", cfg)
	}
	if _, ok := fields["CustomKey"]; !ok {
		t.Fatal("custom field was not preserved")
	}
}

func TestFilterGeoBytes(t *testing.T) {
	geoipOut, count, err := filterGeoIPBytes(makeGeoIPDat(t, []*xrayRouter.GeoIP{
		{CountryCode: "RU"},
		{CountryCode: "CN"},
	}), map[string]bool{"RU": true})
	if err != nil {
		t.Fatalf("filterGeoIPBytes: %v", err)
	}
	if count != 1 || len(geoipOut) == 0 {
		t.Fatalf("unexpected geoip result count=%d len=%d", count, len(geoipOut))
	}

	geositeOut, count, err := filterGeoSiteBytes(makeGeoSiteDat(t, []*xrayRouter.GeoSite{
		{CountryCode: "PRIVATE"},
		{CountryCode: "GOOGLE"},
	}), map[string]bool{"PRIVATE": true})
	if err != nil {
		t.Fatalf("filterGeoSiteBytes: %v", err)
	}
	if count != 1 || len(geositeOut) == 0 {
		t.Fatalf("unexpected geosite result count=%d len=%d", count, len(geositeOut))
	}
}

func TestProcessGeoFilesAndRefresh(t *testing.T) {
	geoipV1 := makeGeoIPDat(t, []*xrayRouter.GeoIP{{CountryCode: "RU"}, {CountryCode: "CN"}})
	geoipV2 := makeGeoIPDat(t, []*xrayRouter.GeoIP{{CountryCode: "RU"}, {CountryCode: "CN"}, {CountryCode: "US"}})
	geositeV1 := makeGeoSiteDat(t, []*xrayRouter.GeoSite{{CountryCode: "PRIVATE"}, {CountryCode: "GOOGLE"}})
	geositeV2 := makeGeoSiteDat(t, []*xrayRouter.GeoSite{{CountryCode: "PRIVATE"}, {CountryCode: "GOOGLE"}, {CountryCode: "TELEGRAM"}})

	currentVersion := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := `"v1"`
		if currentVersion == 2 {
			etag = `"v2"`
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		switch {
		case strings.HasSuffix(r.URL.Path, "geoip.dat"):
			if currentVersion == 1 {
				_, _ = w.Write(geoipV1)
			} else {
				_, _ = w.Write(geoipV2)
			}
		default:
			if currentVersion == 1 {
				_, _ = w.Write(geositeV1)
			} else {
				_, _ = w.Write(geositeV2)
			}
		}
	}))
	defer srv.Close()

	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	raw := happURL(t, map[string]any{
		"Geoipurl":    srv.URL + "/geoip.dat",
		"Geositeurl":  srv.URL + "/geosite.dat",
		"DirectSites": []string{"geosite:private"},
		"DirectIp":    []string{"geoip:ru"},
		"ProxySites":  []string{"geosite:google"},
		"ProxyIp":     []string{"geoip:cn"},
	})

	svc := &GeoFilterService{}
	info, etags, err := svc.ProcessGeoFiles(raw)
	if err != nil {
		t.Fatalf("ProcessGeoFiles: %v", err)
	}
	if info.GeoipCategories != 2 || info.GeositeCategories != 2 {
		t.Fatalf("unexpected initial category counts: %+v", info)
	}
	if etags.GeoipEtag != `"v1"` || etags.GeositeEtag != `"v1"` {
		t.Fatalf("unexpected initial etags: %+v", etags)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XUI_BIN_FOLDER"), "sub_geoip.dat")); err != nil {
		t.Fatalf("missing sub_geoip.dat: %v", err)
	}

	refreshed, refreshInfo, refreshEtags, err := svc.RefreshIfStale(raw, etags)
	if err != nil {
		t.Fatalf("RefreshIfStale no-op: %v", err)
	}
	if refreshed || refreshInfo != nil {
		t.Fatalf("expected no refresh when etags match")
	}
	if refreshEtags != etags {
		t.Fatalf("unexpected etag mutation on no-op: %+v", refreshEtags)
	}

	currentVersion = 2
	refreshed, refreshInfo, refreshEtags, err = svc.RefreshIfStale(raw, etags)
	if err != nil {
		t.Fatalf("RefreshIfStale refresh: %v", err)
	}
	if !refreshed || refreshInfo == nil {
		t.Fatalf("expected refresh after upstream change")
	}
	if refreshEtags.GeoipEtag != `"v2"` || refreshEtags.GeositeEtag != `"v2"` {
		t.Fatalf("unexpected refreshed etags: %+v", refreshEtags)
	}
}

func TestBuildModifiedRoutingURL(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", tmpDir)
	if err := os.WriteFile(filepath.Join(tmpDir, "sub_geoip.dat"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write sub_geoip.dat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "sub_geosite.dat"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write sub_geosite.dat: %v", err)
	}

	raw := happURL(t, map[string]any{
		"Geoipurl":   "https://upstream.example/geoip.dat",
		"Geositeurl": "https://upstream.example/geosite.dat",
		"Name":       "test",
	})
	modified, err := (&GeoFilterService{}).BuildModifiedRoutingURL(raw, "https://sub.example:2096")
	if err != nil {
		t.Fatalf("BuildModifiedRoutingURL: %v", err)
	}
	if modified == raw {
		t.Fatal("expected modified routing URL")
	}

	idx := strings.LastIndex(modified, "/")
	decoded, err := base64.StdEncoding.DecodeString(modified[idx+1:])
	if err != nil {
		t.Fatalf("decode modified routing URL: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("unmarshal modified payload: %v", err)
	}

	var geoipURL string
	if err := json.Unmarshal(payload["Geoipurl"], &geoipURL); err != nil {
		t.Fatalf("unmarshal Geoipurl: %v", err)
	}
	if geoipURL != "https://sub.example:2096/geodata/geoip.dat" {
		t.Fatalf("unexpected Geoipurl: %s", geoipURL)
	}

	var lastUpdated string
	if err := json.Unmarshal(payload["LastUpdated"], &lastUpdated); err != nil {
		t.Fatalf("unmarshal LastUpdated: %v", err)
	}
	if _, err := strconv.ParseInt(lastUpdated, 10, 64); err != nil {
		t.Fatalf("LastUpdated is not a unix timestamp: %v", err)
	}
}
