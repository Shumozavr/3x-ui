package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mhsanaei/3x-ui/v2/config"
	xrayRouter "github.com/xtls/xray-core/app/router"
	"google.golang.org/protobuf/proto"
)

// HappRoutingConfig holds the geo-relevant fields from a happ://routing/... JSON payload.
type HappRoutingConfig struct {
	Geoipurl    string   `json:"Geoipurl"`
	Geositeurl  string   `json:"Geositeurl"`
	LastUpdated string   `json:"LastUpdated"`
	DirectSites []string `json:"DirectSites"`
	DirectIp    []string `json:"DirectIp"`
	ProxySites  []string `json:"ProxySites"`
	ProxyIp     []string `json:"ProxyIp"`
	BlockSites  []string `json:"BlockSites"`
	BlockIp     []string `json:"BlockIp"`
}

// SubGeoFileInfo holds metadata about the most recent geo-file processing run.
type SubGeoFileInfo struct {
	GeoipSize         int64  `json:"geoipSize"`
	GeoipCategories   int    `json:"geoipCategories"`
	GeositeSize       int64  `json:"geositeSize"`
	GeositeCategories int    `json:"geositeCategories"`
	ProcessedAt       string `json:"processedAt"`
}

// GeoEtags stores the ETags received from upstream geo-file sources so that
// subsequent requests can use If-None-Match for conditional downloads.
type GeoEtags struct {
	GeoipEtag   string `json:"geoipEtag"`
	GeositeEtag string `json:"geositeEtag"`
}

// GeoFilterService downloads, filters, and serves geo dat files for happ routing.
type GeoFilterService struct{}

// ParseHappRoutingURL decodes the base64 JSON payload from a
// happ://routing/<action>/<base64> URL.
// Returns the typed config, a raw field map for round-trip preservation, and any error.
func (s *GeoFilterService) ParseHappRoutingURL(raw string) (*HappRoutingConfig, map[string]json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "happ://routing/") {
		return nil, nil, fmt.Errorf("not a happ routing URL")
	}
	idx := strings.LastIndex(raw, "/")
	if idx < 0 {
		return nil, nil, fmt.Errorf("invalid happ routing URL")
	}
	b64 := raw[idx+1:]

	var data []byte
	var err error
	for _, dec := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
	} {
		data, err = dec(b64)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("base64 decode: %w", err)
	}

	cfg := &HappRoutingConfig{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, nil, fmt.Errorf("JSON parse: %w", err)
	}

	var allFields map[string]json.RawMessage
	_ = json.Unmarshal(data, &allFields)

	return cfg, allFields, nil
}

// extractCategories returns an uppercase set of category codes from entries like
// "geoip:ru" or "geosite:TELEGRAM". Entries without the expected prefix are skipped.
func extractCategories(entries []string, prefix string) map[string]bool {
	out := make(map[string]bool)
	p := prefix + ":"
	for _, e := range entries {
		if strings.HasPrefix(strings.ToLower(e), p) {
			out[strings.ToUpper(e[len(p):])] = true
		}
	}
	return out
}

// collectAllCats merges categories from all geo rule lists for a given prefix.
func collectAllCats(cfg *HappRoutingConfig, prefix string) map[string]bool {
	var all []string
	if prefix == "geoip" {
		all = append(all, cfg.DirectIp...)
		all = append(all, cfg.ProxyIp...)
		all = append(all, cfg.BlockIp...)
	} else {
		all = append(all, cfg.DirectSites...)
		all = append(all, cfg.ProxySites...)
		all = append(all, cfg.BlockSites...)
	}
	return extractCategories(all, prefix)
}

// downloadWithEtag performs a GET request with an optional If-None-Match header.
// Returns (body, etag, changed). body is nil and changed is false on HTTP 304.
func downloadWithEtag(url, knownEtag string) ([]byte, string, bool, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", false, err
	}
	if knownEtag != "" {
		req.Header.Set("If-None-Match", knownEtag)
	}
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()

	etag := resp.Header.Get("ETag")
	if resp.StatusCode == http.StatusNotModified {
		return nil, etag, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", false, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(resp.Body)
	return body, etag, true, err
}

// filterGeoIPBytes parses a GeoIPList protobuf and keeps only entries whose
// CountryCode is in cats.
func filterGeoIPBytes(data []byte, cats map[string]bool) ([]byte, int, error) {
	list := &xrayRouter.GeoIPList{}
	if err := proto.Unmarshal(data, list); err != nil {
		return nil, 0, fmt.Errorf("parse geoip: %w", err)
	}
	filtered := &xrayRouter.GeoIPList{}
	for _, e := range list.Entry {
		if cats[strings.ToUpper(e.CountryCode)] {
			filtered.Entry = append(filtered.Entry, e)
		}
	}
	out, err := proto.Marshal(filtered)
	return out, len(filtered.Entry), err
}

// filterGeoSiteBytes parses a GeoSiteList protobuf and keeps only entries whose
// CountryCode is in cats.
func filterGeoSiteBytes(data []byte, cats map[string]bool) ([]byte, int, error) {
	list := &xrayRouter.GeoSiteList{}
	if err := proto.Unmarshal(data, list); err != nil {
		return nil, 0, fmt.Errorf("parse geosite: %w", err)
	}
	filtered := &xrayRouter.GeoSiteList{}
	for _, e := range list.Entry {
		if cats[strings.ToUpper(e.CountryCode)] {
			filtered.Entry = append(filtered.Entry, e)
		}
	}
	out, err := proto.Marshal(filtered)
	return out, len(filtered.Entry), err
}

// ProcessGeoFiles unconditionally downloads both geo files, filters them to the
// categories referenced by routingURL, and saves them to the bin folder.
// The original routingURL is never modified.
// Returns file metadata and the ETags returned by the upstream servers.
func (s *GeoFilterService) ProcessGeoFiles(routingURL string) (*SubGeoFileInfo, GeoEtags, error) {
	cfg, _, err := s.ParseHappRoutingURL(routingURL)
	if err != nil {
		return nil, GeoEtags{}, err
	}

	binPath := config.GetBinFolderPath()
	info := &SubGeoFileInfo{ProcessedAt: time.Now().UTC().Format(time.RFC3339)}
	var etags GeoEtags

	// geoip
	geoipData, geoipEtag, _, err := downloadWithEtag(cfg.Geoipurl, "")
	if err != nil {
		return nil, GeoEtags{}, err
	}
	filteredGeoip, geoipCount, err := filterGeoIPBytes(geoipData, collectAllCats(cfg, "geoip"))
	if err != nil {
		return nil, GeoEtags{}, err
	}
	if err := os.WriteFile(binPath+"/sub_geoip.dat", filteredGeoip, 0o644); err != nil {
		return nil, GeoEtags{}, fmt.Errorf("write sub_geoip.dat: %w", err)
	}
	info.GeoipSize = int64(len(filteredGeoip))
	info.GeoipCategories = geoipCount
	etags.GeoipEtag = geoipEtag

	// geosite
	geositeData, geositeEtag, _, err := downloadWithEtag(cfg.Geositeurl, "")
	if err != nil {
		return nil, GeoEtags{}, err
	}
	filteredGeosite, geositeCount, err := filterGeoSiteBytes(geositeData, collectAllCats(cfg, "geosite"))
	if err != nil {
		return nil, GeoEtags{}, err
	}
	if err := os.WriteFile(binPath+"/sub_geosite.dat", filteredGeosite, 0o644); err != nil {
		return nil, GeoEtags{}, fmt.Errorf("write sub_geosite.dat: %w", err)
	}
	info.GeositeSize = int64(len(filteredGeosite))
	info.GeositeCategories = geositeCount
	etags.GeositeEtag = geositeEtag

	return info, etags, nil
}

// RefreshIfStale performs conditional downloads using knownEtags. If either
// upstream file changed it re-filters and saves. Returns whether any file was
// refreshed, the new metadata (nil if unchanged), and updated ETags.
func (s *GeoFilterService) RefreshIfStale(routingURL string, knownEtags GeoEtags) (bool, *SubGeoFileInfo, GeoEtags, error) {
	cfg, _, err := s.ParseHappRoutingURL(routingURL)
	if err != nil {
		return false, nil, knownEtags, err
	}

	binPath := config.GetBinFolderPath()
	newEtags := knownEtags
	refreshed := false
	info := &SubGeoFileInfo{ProcessedAt: time.Now().UTC().Format(time.RFC3339)}

	// Conditional download for geoip
	geoipData, geoipEtag, geoipChanged, err := downloadWithEtag(cfg.Geoipurl, knownEtags.GeoipEtag)
	if err != nil {
		return false, nil, knownEtags, err
	}
	if geoipEtag != "" {
		newEtags.GeoipEtag = geoipEtag
	}
	if geoipChanged {
		filtered, count, err := filterGeoIPBytes(geoipData, collectAllCats(cfg, "geoip"))
		if err != nil {
			return false, nil, knownEtags, err
		}
		if err := os.WriteFile(binPath+"/sub_geoip.dat", filtered, 0o644); err != nil {
			return false, nil, knownEtags, fmt.Errorf("write sub_geoip.dat: %w", err)
		}
		info.GeoipSize = int64(len(filtered))
		info.GeoipCategories = count
		refreshed = true
	} else if fi, err := os.Stat(binPath + "/sub_geoip.dat"); err == nil {
		info.GeoipSize = fi.Size()
	}

	// Conditional download for geosite
	geositeData, geositeEtag, geositeChanged, err := downloadWithEtag(cfg.Geositeurl, knownEtags.GeositeEtag)
	if err != nil {
		return false, nil, knownEtags, err
	}
	if geositeEtag != "" {
		newEtags.GeositeEtag = geositeEtag
	}
	if geositeChanged {
		filtered, count, err := filterGeoSiteBytes(geositeData, collectAllCats(cfg, "geosite"))
		if err != nil {
			return false, nil, knownEtags, err
		}
		if err := os.WriteFile(binPath+"/sub_geosite.dat", filtered, 0o644); err != nil {
			return false, nil, knownEtags, fmt.Errorf("write sub_geosite.dat: %w", err)
		}
		info.GeositeSize = int64(len(filtered))
		info.GeositeCategories = count
		refreshed = true
	} else if fi, err := os.Stat(binPath + "/sub_geosite.dat"); err == nil {
		info.GeositeSize = fi.Size()
	}

	if !refreshed {
		return false, nil, newEtags, nil
	}
	return true, info, newEtags, nil
}

// BuildModifiedRoutingURL returns a copy of rawURL with Geoipurl and Geositeurl
// replaced by the sub server's local /geodata/ paths. Returns rawURL unchanged
// if subBaseURL is empty or the local dat files do not exist yet.
func (s *GeoFilterService) BuildModifiedRoutingURL(rawURL, subBaseURL string) (string, error) {
	if subBaseURL == "" || rawURL == "" {
		return rawURL, nil
	}
	binPath := config.GetBinFolderPath()
	if _, err := os.Stat(binPath + "/sub_geoip.dat"); err != nil {
		return rawURL, nil
	}
	if _, err := os.Stat(binPath + "/sub_geosite.dat"); err != nil {
		return rawURL, nil
	}

	_, allFields, err := s.ParseHappRoutingURL(rawURL)
	if err != nil || allFields == nil {
		return rawURL, nil
	}

	newGeoip, _ := json.Marshal(subBaseURL + "/geodata/geoip.dat")
	newGeosite, _ := json.Marshal(subBaseURL + "/geodata/geosite.dat")
	allFields["Geoipurl"] = json.RawMessage(newGeoip)
	allFields["Geositeurl"] = json.RawMessage(newGeosite)

	jsonData, err := json.Marshal(allFields)
	if err != nil {
		return rawURL, nil
	}
	b64 := base64.StdEncoding.EncodeToString(jsonData)
	idx := strings.LastIndex(rawURL, "/")
	return rawURL[:idx+1] + b64, nil
}
