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

// HappRoutingConfig represents the JSON payload inside a happ://routing/... URL.
// Only fields relevant to geo-file processing are parsed; all others are preserved
// via RawFields so the round-tripped JSON stays intact.
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

// GeoFilterService downloads, filters, and saves geo dat files for happ routing.
type GeoFilterService struct{}

// ParseHappRoutingURL extracts and decodes the base64 JSON payload from a
// happ://routing/<action>/<base64> URL.
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
	for _, enc := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
	} {
		data, err = enc(b64)
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

	// Preserve all original fields so the round-trip doesn't lose anything
	var all map[string]json.RawMessage
	_ = json.Unmarshal(data, &all)

	return cfg, all, nil
}

// extractCategories returns an uppercase set of category names from entries like
// "geoip:ru" or "geosite:TELEGRAM". Entries without the expected prefix are skipped.
func extractCategories(entries []string, prefix string) map[string]bool {
	out := make(map[string]bool)
	p := prefix + ":"
	for _, e := range entries {
		lower := strings.ToLower(e)
		if strings.HasPrefix(lower, p) {
			out[strings.ToUpper(e[len(p):])] = true
		}
	}
	return out
}

func downloadBytes(url string) ([]byte, error) {
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

func filterGeoIP(url string, cats map[string]bool) ([]byte, int, error) {
	raw, err := downloadBytes(url)
	if err != nil {
		return nil, 0, fmt.Errorf("download geoip: %w", err)
	}
	list := &xrayRouter.GeoIPList{}
	if err := proto.Unmarshal(raw, list); err != nil {
		return nil, 0, fmt.Errorf("parse geoip: %w", err)
	}
	filtered := &xrayRouter.GeoIPList{}
	for _, entry := range list.Entry {
		if cats[strings.ToUpper(entry.CountryCode)] {
			filtered.Entry = append(filtered.Entry, entry)
		}
	}
	out, err := proto.Marshal(filtered)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal geoip: %w", err)
	}
	return out, len(filtered.Entry), nil
}

func filterGeoSite(url string, cats map[string]bool) ([]byte, int, error) {
	raw, err := downloadBytes(url)
	if err != nil {
		return nil, 0, fmt.Errorf("download geosite: %w", err)
	}
	list := &xrayRouter.GeoSiteList{}
	if err := proto.Unmarshal(raw, list); err != nil {
		return nil, 0, fmt.Errorf("parse geosite: %w", err)
	}
	filtered := &xrayRouter.GeoSiteList{}
	for _, entry := range list.Entry {
		if cats[strings.ToUpper(entry.CountryCode)] {
			filtered.Entry = append(filtered.Entry, entry)
		}
	}
	out, err := proto.Marshal(filtered)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal geosite: %w", err)
	}
	return out, len(filtered.Entry), nil
}

// ProcessGeoFiles downloads and filters the geo dat files referenced by routingURL,
// saves them to the bin folder, updates the Geoipurl/Geositeurl in the JSON to point
// to subBaseURL/geodata/{file}, and returns the updated routing URL + file metadata.
// If subBaseURL is empty the URLs in the config are left unchanged.
func (s *GeoFilterService) ProcessGeoFiles(routingURL, subBaseURL string) (string, *SubGeoFileInfo, error) {
	cfg, allFields, err := s.ParseHappRoutingURL(routingURL)
	if err != nil {
		return routingURL, nil, err
	}

	// Collect all categories needed from every rule list
	geoipCats := extractCategories(cfg.DirectIp, "geoip")
	for k := range extractCategories(cfg.ProxyIp, "geoip") {
		geoipCats[k] = true
	}
	for k := range extractCategories(cfg.BlockIp, "geoip") {
		geoipCats[k] = true
	}

	geositeCats := extractCategories(cfg.DirectSites, "geosite")
	for k := range extractCategories(cfg.ProxySites, "geosite") {
		geositeCats[k] = true
	}
	for k := range extractCategories(cfg.BlockSites, "geosite") {
		geositeCats[k] = true
	}

	binPath := config.GetBinFolderPath()
	info := &SubGeoFileInfo{ProcessedAt: time.Now().UTC().Format(time.RFC3339)}

	// Filter geoip
	geoipData, geoipCount, err := filterGeoIP(cfg.Geoipurl, geoipCats)
	if err != nil {
		return routingURL, nil, err
	}
	if err := os.WriteFile(binPath+"/sub_geoip.dat", geoipData, 0o644); err != nil {
		return routingURL, nil, fmt.Errorf("write sub_geoip.dat: %w", err)
	}
	info.GeoipSize = int64(len(geoipData))
	info.GeoipCategories = geoipCount

	// Filter geosite
	geositeData, geositeCount, err := filterGeoSite(cfg.Geositeurl, geositeCats)
	if err != nil {
		return routingURL, nil, err
	}
	if err := os.WriteFile(binPath+"/sub_geosite.dat", geositeData, 0o644); err != nil {
		return routingURL, nil, fmt.Errorf("write sub_geosite.dat: %w", err)
	}
	info.GeositeSize = int64(len(geositeData))
	info.GeositeCategories = geositeCount

	// Rebuild the happ URL with updated geo file URLs
	if subBaseURL != "" && allFields != nil {
		newGeoip, _ := json.Marshal(subBaseURL + "/geodata/geoip.dat")
		newGeosite, _ := json.Marshal(subBaseURL + "/geodata/geosite.dat")
		newTs, _ := json.Marshal(fmt.Sprintf("%d", time.Now().Unix()))
		allFields["Geoipurl"] = json.RawMessage(newGeoip)
		allFields["Geositeurl"] = json.RawMessage(newGeosite)
		allFields["LastUpdated"] = json.RawMessage(newTs)

		jsonData, err := json.Marshal(allFields)
		if err == nil {
			b64 := base64.StdEncoding.EncodeToString(jsonData)
			idx := strings.LastIndex(routingURL, "/")
			routingURL = routingURL[:idx+1] + b64
		}
	}

	return routingURL, info, nil
}
