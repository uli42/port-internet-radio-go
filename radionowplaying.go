package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"sort"
	"time"
)

const (
	rnpBaseURL = "http://www.radionowplaying.com"
)

type RNPBroadcaster struct {
	Name    string `json:"name"`
	Icon    string `json:"icon"`
	Web     string `json:"web"`
	ModTime int64  `json:"modtime"`
}

type RNPSummary struct {
	RNPSummary []RNPBroadcaster `json:"rnp_summary"`
}

func rnpRequest(endpoint string) ([]byte, error) {
	client := newHTTPClient(10*time.Second, 0)
	reqURL := rnpBaseURL + endpoint
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

func fetchRNPBroadcasters() ([]RNPBroadcaster, error) {
	data, err := rnpRequest("/rnp_summary.json")
	if err != nil {
		return nil, err
	}
	var summary RNPSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return nil, err
	}
	sort.Slice(summary.RNPSummary, func(i, j int) bool {
		return summary.RNPSummary[i].Name < summary.RNPSummary[j].Name
	})
	return summary.RNPSummary, nil
}

func handleRNPMenu(respClid, playerId string) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		return
	}
	session := getClientSession(respClid)
	session.Reset()

	broadcasters, err := fetchRNPBroadcasters()
	if err != nil {
		LogError("RNP", "Failed to fetch broadcasters: %v", err)
		return
	}

	items := make([]map[string]interface{}, 0, len(broadcasters))
	for _, b := range broadcasters {
		if b.Name == "" {
			continue
		}
		itemID := fmt.Sprintf("rnp_%s", b.Name)
		session.ItemURLs[itemID] = b.Web
		icon := "/plugins/radionowplaying/html/images/radio.png"
		if b.Icon != "" {
			icon = b.Icon
		}
		items = append(items, map[string]interface{}{
			"id":    itemID,
			"text":  b.Name,
			"weight": 0,
			"icon":  icon,
			"actions": map[string]interface{}{
				"go": map[string]interface{}{
					"cmd": []interface{}{"rnp", "broadcaster"},
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

func handleRNPBroadcaster(respClid, playerId string, broadcasterName string) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		return
	}
	session := getClientSession(respClid)
	session.Reset()

	broadcasters, err := fetchRNPBroadcasters()
	if err != nil {
		LogError("RNP", "Failed to fetch broadcasters: %v", err)
		return
	}

	var broadcaster *RNPBroadcaster
	for i := range broadcasters {
		if broadcasters[i].Name == broadcasterName {
			broadcaster = &broadcasters[i]
			break
		}
	}
	if broadcaster == nil {
		return
	}

	itemID := fmt.Sprintf("rnpweb_%s", broadcasterName)
	session.ItemURLs[itemID] = broadcaster.Web

	items := []map[string]interface{}{
		{
			"id":    itemID,
			"text":  "Visit Website",
			"weight": 0,
			"icon":  "/plugins/radionowplaying/html/images/radio.png",
			"actions": map[string]interface{}{
				"go": map[string]interface{}{
					"cmd": []interface{}{"rnp", "visit"},
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

func handleRNPVisit(respClid, playerId, broadcasterName string) {
	broadcasters, err := fetchRNPBroadcasters()
	if err != nil {
		LogError("RNP", "Failed to fetch broadcasters: %v", err)
		return
	}

	var broadcaster *RNPBroadcaster
	for i := range broadcasters {
		if broadcasters[i].Name == broadcasterName {
			broadcaster = &broadcasters[i]
			break
		}
	}
	if broadcaster == nil || broadcaster.Web == "" {
		return
	}

	pb := getPlayerPlayback(playerId)
	pb.Mu.Lock()
	pb.Playing = true
	pb.PlayingTitle = broadcaster.Name
	pb.PlayingDesc = "Radio Now Playing"
	pb.PlayingIcon = ""
	pb.PlayingURL = broadcaster.Web
	pb.StationName = broadcaster.Name
	pb.StationDesc = ""
	pb.Mu.Unlock()

	slimConnMu.Lock()
	conn := slimConn
	slimConnMu.Unlock()

	if conn != nil {
		LogInfo("RNP", "Tuning to broadcaster %s URL=%s", broadcasterName, broadcaster.Web)
		go runURLStream(conn, broadcaster.Web, pb, playerId)
		buildStatusResponse(respClid, playerId, nil, globalServerMetadata)
		go func() {
			time.Sleep(150 * time.Millisecond)
			publishPlayerStatus()
		}()
	}
}
