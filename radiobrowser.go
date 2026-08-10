package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"
)

const (
	radioBrowserBaseURL = "https://all.api.radio-browser.info/json"
)

type RadioBrowserStation struct {
	StationUUID     string  `json:"stationuuid"`
	Name            string  `json:"name"`
	URL             string  `json:"url"`
	Favicon         string  `json:"favicon"`
	Country         string  `json:"country"`
	State           string  `json:"state"`
	Language        string  `json:"language"`
	Codec           string  `json:"codec"`
	Bitrate         int     `json:"bitrate"`
	LastCheckOK     int     `json:"lastcheckok"`
	Tags            string  `json:"tags"`
	Votes           int     `json:"votes"`
}

type RadioBrowserTag struct {
	Name string `json:"name"`
}

type RadioBrowserCountry struct {
	Name string `json:"name"`
	Code string `json:"iso_3166_1"`
}

func radioBrowserRequest(endpoint string) ([]byte, error) {
	client := newHTTPClient(10 * time.Second)
	reqURL := radioBrowserBaseURL + endpoint
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "LogitechSqueezebox/9.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	return ioutil.ReadAll(resp.Body)
}

func fetchRadioBrowserTopStations(limit int) ([]RadioBrowserStation, error) {
	data, err := radioBrowserRequest(fmt.Sprintf("/stations/topvote/%d?hidebroken=true", limit))
	if err != nil {
		return nil, err
	}
	var stations []RadioBrowserStation
	if err := json.Unmarshal(data, &stations); err != nil {
		return nil, err
	}
	return stations, nil
}

func fetchRadioBrowserStationsByTag(tag string, limit int) ([]RadioBrowserStation, error) {
	data, err := radioBrowserRequest(fmt.Sprintf("/stations/bytag/%s/%d?hidebroken=true", urlPathEscape(tag), limit))
	if err != nil {
		return nil, err
	}
	var stations []RadioBrowserStation
	if err := json.Unmarshal(data, &stations); err != nil {
		return nil, err
	}
	return stations, nil
}

func fetchRadioBrowserStationsByCountry(countryCode string, limit int) ([]RadioBrowserStation, error) {
	data, err := radioBrowserRequest(fmt.Sprintf("/stations/bycountrycode/%s/%d?hidebroken=true", urlPathEscape(countryCode), limit))
	if err != nil {
		return nil, err
	}
	var stations []RadioBrowserStation
	if err := json.Unmarshal(data, &stations); err != nil {
		return nil, err
	}
	return stations, nil
}

func fetchRadioBrowserStationsByName(name string, limit int) ([]RadioBrowserStation, error) {
	data, err := radioBrowserRequest(fmt.Sprintf("/stations/byname/%s/%d?hidebroken=true", urlPathEscape(name), limit))
	if err != nil {
		return nil, err
	}
	var stations []RadioBrowserStation
	if err := json.Unmarshal(data, &stations); err != nil {
		return nil, err
	}
	return stations, nil
}

func fetchRadioBrowserTags() ([]RadioBrowserTag, error) {
	data, err := radioBrowserRequest("/tags")
	if err != nil {
		return nil, err
	}
	var tags []RadioBrowserTag
	if err := json.Unmarshal(data, &tags); err != nil {
		return nil, err
	}
	return tags, nil
}

func fetchRadioBrowserCountries() ([]RadioBrowserCountry, error) {
	data, err := radioBrowserRequest("/countries")
	if err != nil {
		return nil, err
	}
	var countries []RadioBrowserCountry
	if err := json.Unmarshal(data, &countries); err != nil {
		return nil, err
	}
	return countries, nil
}

func fetchRadioBrowserStreamURL(stationUUID string) (string, error) {
	LogInfo("RadioBrowser", "Fetching stream URL for stationUUID=%s", stationUUID)
	data, err := radioBrowserRequest(fmt.Sprintf("/url/%s", urlPathEscape(stationUUID)))
	if err != nil {
		return "", err
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("failed to parse stream URL response: %v", err)
	}
	url := strings.TrimSpace(result.URL)
	if url == "" {
		return "", fmt.Errorf("empty stream URL in response")
	}
	return url, nil
}

func urlPathEscape(s string) string {
	return strings.ReplaceAll(s, " ", "%20")
}

func handleRadioBrowserMenu(respClid, playerId string) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		return
	}
	session := getClientSession(respClid)
	session.Reset()

	items := []map[string]interface{}{
		{
			"id":    "topvote",
			"text":  "Top Voted",
			"icon":  "/plugins/TuneIn/html/images/radio.png",
			"actions": map[string]interface{}{
				"go": map[string]interface{}{
					"cmd": []interface{}{"radiobrowser", "topvote"},
				},
			},
		},
		{
			"id":   "search",
			"text": "Search",
			"icon": "/plugins/TuneIn/html/images/radiosearch.png",
			"actions": map[string]interface{}{
				"go": map[string]interface{}{
					"cmd": []interface{}{"radiobrowser", "search"},
				},
			},
		},
		{
			"id":   "tags",
			"text": "By Tag",
			"icon": "/plugins/TuneIn/html/images/radiomusic.png",
			"actions": map[string]interface{}{
				"go": map[string]interface{}{
					"cmd": []interface{}{"radiobrowser", "tags"},
				},
			},
		},
		{
			"id":   "countries",
			"text": "By Country",
			"icon": "/plugins/TuneIn/html/images/radioworld.png",
			"actions": map[string]interface{}{
				"go": map[string]interface{}{
					"cmd": []interface{}{"radiobrowser", "countries"},
				},
			},
		},
	}

	data := map[string]interface{}{
		"count":     len(items),
		"offset":    0,
		"item_loop": items,
	}

	sc.pendingMu.Lock()
	sc.pendingResponse = data
	sc.pendingMu.Unlock()
}

func handleRadioBrowserSearch(respClid, playerId string, query string) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		return
	}
	session := getClientSession(respClid)

	if query == "" || query == "__TAGGEDINPUT__" {
		data := map[string]interface{}{
			"count": 1,
			"offset": 0,
			"item_loop": []map[string]interface{}{
				{
					"id":    "search",
					"text":  "Search stations",
					"type":  "search",
					"icon":  "/plugins/TuneIn/html/images/radiosearch.png",
					"input": map[string]interface{}{
						"len":  3,
						"help": map[string]interface{}{"text": "Search"},
					},
					"actions": map[string]interface{}{
						"go": map[string]interface{}{
							"cmd": []interface{}{"radiobrowser", "search"},
							"params": map[string]interface{}{
								"query": "__TAGGEDINPUT__",
							},
						},
					},
				},
			},
		}
		sc.pendingMu.Lock()
		sc.pendingResponse = data
		sc.pendingMu.Unlock()
		return
	}

	stations, err := fetchRadioBrowserStationsByName(query, 30)
	if err != nil {
		LogError("RadioBrowser", "Search failed: %v", err)
		return
	}

	session.Reset()
	session.Mu.Lock()
	items := buildStationItems(stations, session)
	session.Mu.Unlock()

	data := map[string]interface{}{
		"count":     len(items),
		"offset":    0,
		"item_loop": items,
	}

	sc.pendingMu.Lock()
	sc.pendingResponse = data
	sc.pendingMu.Unlock()
}

func handleRadioBrowserTopVote(respClid, playerId string, limit int) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		LogInfo("RadioBrowser", "TopVote: no streaming client for %s", respClid)
		return
	}
	LogInfo("RadioBrowser", "TopVote: fetching top stations for client=%s limit=%d", respClid, limit)
	session := getClientSession(respClid)

	stations, err := fetchRadioBrowserTopStations(limit)
	if err != nil {
		LogError("RadioBrowser", "Top vote fetch failed: %v", err)
		return
	}
	LogInfo("RadioBrowser", "TopVote: fetched %d stations", len(stations))

	session.Reset()
	session.Mu.Lock()
	items := buildStationItems(stations, session)
	session.Mu.Unlock()

	data := map[string]interface{}{
		"count":     len(items),
		"offset":    0,
		"item_loop": items,
	}

	sc.pendingMu.Lock()
	sc.pendingResponse = data
	sc.pendingMu.Unlock()
	LogInfo("RadioBrowser", "TopVote: set pendingResponse with %d items", len(items))
}

func handleRadioBrowserTags(respClid, playerId string) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		return
	}
	session := getClientSession(respClid)
	session.Reset()

	tags, err := fetchRadioBrowserTags()
	if err != nil {
		LogError("RadioBrowser", "Tags fetch failed: %v", err)
		return
	}

	items := make([]map[string]interface{}, 0, len(tags))
	for _, tag := range tags {
		if tag.Name == "" {
			continue
		}
		itemID := fmt.Sprintf("tag_%s", tag.Name)
		session.ItemURLs[itemID] = tag.Name
		items = append(items, map[string]interface{}{
			"id":     itemID,
			"text":   tag.Name,
			"weight": 0,
			"icon":   "/plugins/TuneIn/html/images/radiomusic.png",
			"actions": map[string]interface{}{
				"go": map[string]interface{}{
					"cmd": []interface{}{"radiobrowser", "tagstations"},
					"params": map[string]interface{}{
						"tag": tag.Name,
					},
				},
			},
		})
	}

	data := map[string]interface{}{
		"count":     len(items),
		"offset":    0,
		"item_loop": items,
	}

	sc.pendingMu.Lock()
	sc.pendingResponse = data
	sc.pendingMu.Unlock()
}

func handleRadioBrowserTagStations(respClid, playerId string, tag string) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		return
	}
	session := getClientSession(respClid)

	stations, err := fetchRadioBrowserStationsByTag(tag, 30)
	if err != nil {
		LogError("RadioBrowser", "Tag stations fetch failed: %v", err)
		return
	}

	session.Reset()
	session.Mu.Lock()
	items := buildStationItems(stations, session)
	session.Mu.Unlock()

	data := map[string]interface{}{
		"count":     len(items),
		"offset":    0,
		"item_loop": items,
	}

	sc.pendingMu.Lock()
	sc.pendingResponse = data
	sc.pendingMu.Unlock()
}

func handleRadioBrowserCountries(respClid, playerId string) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		return
	}
	session := getClientSession(respClid)
	session.Reset()

	countries, err := fetchRadioBrowserCountries()
	if err != nil {
		LogError("RadioBrowser", "Countries fetch failed: %v", err)
		return
	}

	items := make([]map[string]interface{}, 0, len(countries))
	for _, country := range countries {
		if country.Name == "" {
			continue
		}
		itemID := fmt.Sprintf("country_%s", country.Code)
		session.ItemURLs[itemID] = country.Code
		items = append(items, map[string]interface{}{
			"id":     itemID,
			"text":   country.Name,
			"weight": 0,
			"icon":   "/plugins/TuneIn/html/images/radioworld.png",
			"actions": map[string]interface{}{
				"go": map[string]interface{}{
					"cmd": []interface{}{"radiobrowser", "countrystations"},
					"params": map[string]interface{}{
						"country": country.Code,
					},
				},
			},
		})
	}

	data := map[string]interface{}{
		"count":     len(items),
		"offset":    0,
		"item_loop": items,
	}

	sc.pendingMu.Lock()
	sc.pendingResponse = data
	sc.pendingMu.Unlock()
}

func handleRadioBrowserCountryStations(respClid, playerId string, countryCode string) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		return
	}
	session := getClientSession(respClid)

	stations, err := fetchRadioBrowserStationsByCountry(countryCode, 30)
	if err != nil {
		LogError("RadioBrowser", "Country stations fetch failed: %v", err)
		return
	}

	session.Reset()
	session.Mu.Lock()
	items := buildStationItems(stations, session)
	session.Mu.Unlock()

	data := map[string]interface{}{
		"count":     len(items),
		"offset":    0,
		"item_loop": items,
	}

	sc.pendingMu.Lock()
	sc.pendingResponse = data
	sc.pendingMu.Unlock()
}

func buildStationItems(stations []RadioBrowserStation, session *ClientSession) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(stations))
	for _, station := range stations {
		if station.URL == "" || station.LastCheckOK != 1 {
			continue
		}
		itemID := fmt.Sprintf("station_%s", station.StationUUID)
		session.ItemURLs[itemID] = station.StationUUID
		title := station.Name
		if title == "" {
			title = "Unknown Station"
		}
		subtext := ""
		if station.Country != "" {
			subtext = station.Country
		}
		session.ItemMetadata[itemID] = PlaybackItem{
			Title:   title,
			Subtext: subtext,
			Icon:    station.Favicon,
			URL:     station.StationUUID,
		}
		icon := "/plugins/TuneIn/html/images/radio.png"
		if station.Favicon != "" {
			icon = station.Favicon
		}
		items = append(items, map[string]interface{}{
			"id":     itemID,
			"text":   title,
			"weight": 0,
			"icon":   icon,
			"actions": map[string]interface{}{
				"go": map[string]interface{}{
					"cmd": []interface{}{"radiobrowser", "play"},
					"params": map[string]interface{}{
						"stationuuid": station.StationUUID,
					},
				},
			},
		})
	}
	return items
}

func playRadioBrowserStation(respClid, playerId, stationUUID string) {
	if stationUUID == "" {
		return
	}

	streamURL, err := fetchRadioBrowserStreamURL(stationUUID)
	if err != nil {
		LogError("RadioBrowser", "Failed to resolve stream URL for %s: %v", stationUUID, err)
		publishStreamError(playerId, nil, err)
		return
	}
	LogInfo("RadioBrowser", "Resolved stream URL for %s: %s", stationUUID, streamURL)

	pb := getPlayerPlayback(playerId)
	pb.Mu.Lock()
	pb.Playing = true
	pb.PlayingURL = streamURL
	pb.PlayingTitle = ""
	pb.PlayingDesc = ""
	pb.PlayingIcon = ""
	pb.StationName = "Radio Browser"
	pb.Mu.Unlock()

	session := getClientSession(respClid)
	session.Mu.Lock()
	if meta, ok := session.ItemMetadata["station_"+stationUUID]; ok {
		pb.PlayingTitle = meta.Title
		pb.PlayingIcon = meta.Icon
		pb.StationName = meta.Title
	}
	session.Mu.Unlock()

	slimConnMu.Lock()
	conn := slimConn
	slimConnMu.Unlock()

	if conn != nil {
		LogInfo("RadioBrowser", "Tuning to station UUID=%s URL=%s", stationUUID, streamURL)
		go runURLStream(conn, streamURL, pb, playerId)
		buildStatusResponse(respClid, playerId, nil, globalServerMetadata)
		go func() {
			time.Sleep(150 * time.Millisecond)
			publishPlayerStatus()
		}()
	}
}
