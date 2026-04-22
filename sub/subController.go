package sub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/config"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

const geoCheckInterval = 5 * time.Minute

// SUBController handles HTTP requests for subscription links and JSON configurations.
type SUBController struct {
	subTitle         string
	subSupportUrl    string
	subProfileUrl    string
	subAnnounce      string
	subEnableRouting bool
	subRoutingRules  string
	subBaseURL       string
	subPath          string
	subJsonPath      string
	subClashPath     string
	jsonEnabled      bool
	clashEnabled     bool
	subEncrypt       bool
	updateInterval   string
	subCustomHeaders map[string]string

	settingSvc service.SettingService
	geoSvc     service.GeoFilterService

	geoMu          sync.Mutex
	geoLastChecked time.Time
	geoRefreshing  bool

	subService      *SubService
	subJsonService  *SubJsonService
	subClashService *SubClashService
}

// parseCustomHeaders parses a multiline string of "Name: Value" header pairs
// into a map. Lines that are blank or malformed (no colon) are silently skipped.
func parseCustomHeaders(raw string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if name != "" {
			result[name] = value
		}
	}
	return result
}

// NewSUBController creates a new subscription controller with the given configuration.
func NewSUBController(
	g *gin.RouterGroup,
	subPath string,
	jsonPath string,
	clashPath string,
	jsonEnabled bool,
	clashEnabled bool,
	encrypt bool,
	showInfo bool,
	rModel string,
	update string,
	jsonFragment string,
	jsonNoise string,
	jsonMux string,
	jsonRules string,
	subTitle string,
	subSupportUrl string,
	subProfileUrl string,
	subAnnounce string,
	subEnableRouting bool,
	subRoutingRules string,
	subCustomHeaders string,
	subBaseURL string,
) *SUBController {
	sub := NewSubService(showInfo, rModel)
	a := &SUBController{
		subTitle:         subTitle,
		subSupportUrl:    subSupportUrl,
		subProfileUrl:    subProfileUrl,
		subAnnounce:      subAnnounce,
		subEnableRouting: subEnableRouting,
		subRoutingRules:  subRoutingRules,
		subBaseURL:       subBaseURL,
		subPath:          subPath,
		subJsonPath:      jsonPath,
		subClashPath:     clashPath,
		jsonEnabled:      jsonEnabled,
		clashEnabled:     clashEnabled,
		subEncrypt:       encrypt,
		updateInterval:   update,
		subCustomHeaders: parseCustomHeaders(subCustomHeaders),

		subService:      sub,
		subJsonService:  NewSubJsonService(jsonFragment, jsonNoise, jsonMux, jsonRules, sub),
		subClashService: NewSubClashService(sub),
	}
	a.initRouter(g)
	return a
}

// initRouter registers HTTP routes for subscription links and JSON endpoints
// on the provided router group.
func (a *SUBController) initRouter(g *gin.RouterGroup) {
	gLink := g.Group(a.subPath)
	gLink.GET(":subid", a.subs)
	if a.jsonEnabled {
		gJson := g.Group(a.subJsonPath)
		gJson.GET(":subid", a.subJsons)
	}
	if a.clashEnabled {
		gClash := g.Group(a.subClashPath)
		gClash.GET(":subid", a.subClashs)
	}
}

// effectiveRoutingRules returns the routing URL to put in the Routing header.
// If local shrunken geo files exist, Geoipurl/Geositeurl are substituted with
// the sub server's /geodata/ paths. Returns empty string on any error.
func (a *SUBController) effectiveRoutingRules() string {
	if a.subRoutingRules == "" || a.subBaseURL == "" {
		return a.subRoutingRules
	}
	modified, err := a.geoSvc.BuildModifiedRoutingURL(a.subRoutingRules, a.subBaseURL)
	if err != nil {
		return ""
	}
	return modified
}

// maybeRefreshGeoFiles triggers an ETag-based background refresh at most once
// per geoCheckInterval. Safe to call on every subscription request.
func (a *SUBController) maybeRefreshGeoFiles() {
	if a.subRoutingRules == "" {
		return
	}
	if time.Since(a.geoLastChecked) < geoCheckInterval || a.geoRefreshing {
		return
	}
	a.geoMu.Lock()
	if time.Since(a.geoLastChecked) < geoCheckInterval || a.geoRefreshing {
		a.geoMu.Unlock()
		return
	}
	a.geoLastChecked = time.Now()
	a.geoRefreshing = true
	a.geoMu.Unlock()

	go func() {
		defer func() {
			a.geoMu.Lock()
			a.geoRefreshing = false
			a.geoMu.Unlock()
		}()
		a.doGeoRefresh()
	}()
}

// doGeoRefresh reads stored ETags from the DB, performs conditional downloads,
// and if either file changed, writes new files and updates the DB.
func (a *SUBController) doGeoRefresh() {
	knownEtags, err := a.settingSvc.GetSubGeoEtags()
	if err != nil {
		logger.Warning("geo refresh: read etags:", err)
		return
	}

	refreshed, info, newEtags, err := a.geoSvc.RefreshIfStale(a.subRoutingRules, knownEtags)
	if err != nil {
		logger.Warning("geo refresh:", err)
		return
	}
	if !refreshed {
		// ETags may have been returned even without content change; persist them.
		if newEtags != knownEtags {
			_ = a.settingSvc.SetSubGeoEtags(newEtags)
		}
		return
	}

	infoJSON, _ := json.Marshal(info)
	_ = a.settingSvc.SetSubRoutingGeoInfo(string(infoJSON))
	_ = a.settingSvc.SetSubGeoEtags(newEtags)
	logger.Info("geo files refreshed: geoip", info.GeoipSize, "B geosite", info.GeositeSize, "B")
}

// subs handles HTTP requests for subscription links, returning either HTML page or base64-encoded subscription data.
func (a *SUBController) subs(c *gin.Context) {
	subId := c.Param("subid")
	scheme, host, hostWithPort, hostHeader := a.subService.ResolveRequest(c)
	subs, lastOnline, traffic, err := a.subService.GetSubs(subId, host)
	if err != nil || len(subs) == 0 {
		c.String(400, "Error!")
	} else {
		result := ""
		for _, sub := range subs {
			result += sub + "\n"
		}

		// If the request expects HTML (e.g., browser) or explicitly asked (?html=1 or ?view=html), render the info page here
		accept := c.GetHeader("Accept")
		if strings.Contains(strings.ToLower(accept), "text/html") || c.Query("html") == "1" || strings.EqualFold(c.Query("view"), "html") {
			// Build page data in service
			subURL, subJsonURL, subClashURL := a.subService.BuildURLs(scheme, hostWithPort, a.subPath, a.subJsonPath, a.subClashPath, subId)
			if !a.jsonEnabled {
				subJsonURL = ""
			}
			if !a.clashEnabled {
				subClashURL = ""
			}
			// Get base_path from context (set by middleware)
			basePath, exists := c.Get("base_path")
			if !exists {
				basePath = "/"
			}
			basePathStr := basePath.(string)
			if basePathStr == "/" {
				basePathStr = "/" + subId + "/"
			} else {
				basePathStr = strings.TrimRight(basePathStr, "/") + "/" + subId + "/"
			}
			page := a.subService.BuildPageData(subId, hostHeader, traffic, lastOnline, subs, subURL, subJsonURL, subClashURL, basePathStr)
			c.HTML(200, "subpage.html", gin.H{
				"title":        "subscription.title",
				"cur_ver":      config.GetVersion(),
				"host":         page.Host,
				"base_path":    page.BasePath,
				"sId":          page.SId,
				"download":     page.Download,
				"upload":       page.Upload,
				"total":        page.Total,
				"used":         page.Used,
				"remained":     page.Remained,
				"expire":       page.Expire,
				"lastOnline":   page.LastOnline,
				"datepicker":   page.Datepicker,
				"downloadByte": page.DownloadByte,
				"uploadByte":   page.UploadByte,
				"totalByte":    page.TotalByte,
				"subUrl":       page.SubUrl,
				"subJsonUrl":   page.SubJsonUrl,
				"subClashUrl":  page.SubClashUrl,
				"result":       page.Result,
			})
			return
		}

		// Trigger background geo refresh (debounced, non-blocking)
		a.maybeRefreshGeoFiles()

		header := fmt.Sprintf("upload=%d; download=%d; total=%d; expire=%d", traffic.Up, traffic.Down, traffic.Total, traffic.ExpiryTime/1000)
		profileUrl := a.subProfileUrl
		if profileUrl == "" {
			profileUrl = fmt.Sprintf("%s://%s%s", scheme, hostWithPort, c.Request.RequestURI)
		}
		a.ApplyCommonHeaders(c, header, a.updateInterval, a.subTitle, a.subSupportUrl, profileUrl, a.subAnnounce, a.subEnableRouting, a.effectiveRoutingRules(), a.subCustomHeaders)

		if a.subEncrypt {
			c.String(200, base64.StdEncoding.EncodeToString([]byte(result)))
		} else {
			c.String(200, result)
		}
	}
}

// subJsons handles HTTP requests for JSON subscription configurations.
func (a *SUBController) subJsons(c *gin.Context) {
	subId := c.Param("subid")
	scheme, host, hostWithPort, _ := a.subService.ResolveRequest(c)
	jsonSub, header, err := a.subJsonService.GetJson(subId, host)
	if err != nil || len(jsonSub) == 0 {
		c.String(400, "Error!")
	} else {
		// Trigger background geo refresh (debounced, non-blocking)
		a.maybeRefreshGeoFiles()

		profileUrl := a.subProfileUrl
		if profileUrl == "" {
			profileUrl = fmt.Sprintf("%s://%s%s", scheme, hostWithPort, c.Request.RequestURI)
		}
		a.ApplyCommonHeaders(c, header, a.updateInterval, a.subTitle, a.subSupportUrl, profileUrl, a.subAnnounce, a.subEnableRouting, a.effectiveRoutingRules(), a.subCustomHeaders)

		c.String(200, jsonSub)
	}
}

func (a *SUBController) subClashs(c *gin.Context) {
	subId := c.Param("subid")
	scheme, host, hostWithPort, _ := a.subService.ResolveRequest(c)
	clashSub, header, err := a.subClashService.GetClash(subId, host)
	if err != nil || len(clashSub) == 0 {
		c.String(400, "Error!")
	} else {
		// Trigger background geo refresh (debounced, non-blocking)
		a.maybeRefreshGeoFiles()
		profileUrl := a.subProfileUrl
		if profileUrl == "" {
			profileUrl = fmt.Sprintf("%s://%s%s", scheme, hostWithPort, c.Request.RequestURI)
		}
		a.ApplyCommonHeaders(c, header, a.updateInterval, a.subTitle, a.subSupportUrl, profileUrl, a.subAnnounce, a.subEnableRouting, a.effectiveRoutingRules(), a.subCustomHeaders)
		c.Data(200, "application/yaml; charset=utf-8", []byte(clashSub))
	}
}

// ApplyCommonHeaders sets common HTTP headers for subscription responses including user info, update interval, and profile title.
func (a *SUBController) ApplyCommonHeaders(
	c *gin.Context,
	header,
	updateInterval,
	profileTitle string,
	profileSupportUrl string,
	profileUrl string,
	profileAnnounce string,
	profileEnableRouting bool,
	profileRoutingRules string,
	customHeaders map[string]string,
) {
	c.Writer.Header().Set("Subscription-Userinfo", header)
	c.Writer.Header().Set("Profile-Update-Interval", updateInterval)

	if profileTitle != "" {
		c.Writer.Header().Set("Profile-Title", "base64:"+base64.StdEncoding.EncodeToString([]byte(profileTitle)))
	}
	if profileSupportUrl != "" {
		c.Writer.Header().Set("Support-Url", profileSupportUrl)
	}
	if profileUrl != "" {
		c.Writer.Header().Set("Profile-Web-Page-Url", profileUrl)
	}
	if profileAnnounce != "" {
		c.Writer.Header().Set("Announce", "base64:"+base64.StdEncoding.EncodeToString([]byte(profileAnnounce)))
	}

	// Advanced (Happ)
	c.Writer.Header().Set("Routing-Enable", strconv.FormatBool(profileEnableRouting))
	if profileRoutingRules != "" {
		c.Writer.Header().Set("Routing", profileRoutingRules)
	}

	// Custom headers (user-defined, applied last so they can override defaults if needed)
	for name, value := range customHeaders {
		c.Writer.Header().Set(name, value)
	}
}
