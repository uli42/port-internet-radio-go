package main

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	DefaultPort    = 3483
	MulticastGroup = "239.255.255.250"
)

// OPML schemas for parsing TuneIn responses
type OPML struct {
	XMLName xml.Name  `xml:"opml"`
	Version string    `xml:"version,attr"`
	Title   string    `xml:"head>title"`
	Items   []Outline `xml:"body>outline"`
}

type Outline struct {
	Type        string    `xml:"type,attr"`
	Text        string    `xml:"text,attr"`
	URL         string    `xml:"URL,attr"`
	Key         string    `xml:"key,attr"`
	Bitrate     string    `xml:"bitrate,attr"`
	Reliability string    `xml:"reliability,attr"`
	Subtext     string    `xml:"subtext,attr"`
	GenreID     string    `xml:"genre_id,attr"`
	Formats     string    `xml:"formats,attr"`
	Item        string    `xml:"item,attr"`
	Image       string    `xml:"image,attr"`
	PresetID    string    `xml:"preset_id,attr"`
	Playing     string    `xml:"playing,attr"`
	PlayingImg  string    `xml:"playing_image,attr"`
	Outlines    []Outline `xml:"outline"`
}

type MenuFeed struct {
	Title string
	Items []Outline
}

type PlaybackItem struct {
	Title   string
	Subtext string
	Icon    string
	URL     string
}

type ClientSession struct {
	Mu           sync.Mutex
	CachedFeeds  map[string]MenuFeed // maps feedID (e.g. "root", "presets", "item_0", etc.) to MenuFeed
	ItemURLs     map[string]string   // maps itemID (e.g. "item_2") to TuneIn play URL
	ItemMetadata map[string]PlaybackItem
	NextID       int

	// Current playback state
	Playing      bool
	PlayingTitle string
	PlayingDesc  string
	PlayingIcon  string
	PlayingURL   string
}

var (
	clientSessions   = make(map[string]*ClientSession)
	clientSessionsMu sync.Mutex
)

func getClientSession(clientId string) *ClientSession {
	clientSessionsMu.Lock()
	defer clientSessionsMu.Unlock()
	cs, ok := clientSessions[clientId]
	if !ok {
		cs = &ClientSession{
			CachedFeeds:  make(map[string]MenuFeed),
			ItemURLs:     make(map[string]string),
			ItemMetadata: make(map[string]PlaybackItem),
			NextID:       1,
		}
		clientSessions[clientId] = cs
	}
	return cs
}

func (cs *ClientSession) Reset() {
	cs.Mu.Lock()
	defer cs.Mu.Unlock()
	cs.CachedFeeds = make(map[string]MenuFeed)
	cs.ItemURLs = make(map[string]string)
	cs.ItemMetadata = make(map[string]PlaybackItem)
	cs.NextID = 1
}

// Fetch and parse an OPML feed from a URL
func fetchOPML(urlStr string) (*OPML, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   10 * time.Second,
	}
	req, err := http.NewRequest("GET", urlStr, nil)
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

	var opml OPML
	if err := xml.NewDecoder(resp.Body).Decode(&opml); err != nil {
		return nil, err
	}
	return &opml, nil
}

// resolveStreamURL resolves TuneIn Tune.ashx, PLS, and M3U playlists to find a direct stream URL
func resolveStreamURL(streamURL string) string {
	for depth := 0; depth < 5; depth++ {
		lowerURL := strings.ToLower(streamURL)
		isTuneIn := strings.Contains(lowerURL, "tune.ashx") || strings.Contains(lowerURL, "tunein.com") || strings.Contains(lowerURL, "radiotime.com")
		isPlaylist := strings.HasSuffix(lowerURL, ".m3u") || strings.HasSuffix(lowerURL, ".m3u8") || strings.HasSuffix(lowerURL, ".pls")

		if isTuneIn || isPlaylist {
			resolved := fetchPlaylistOrTuneIn(streamURL)
			if resolved != "" && resolved != streamURL {
				fmt.Printf("[Resolver] Resolved %s -> %s\n", streamURL, resolved)
				streamURL = resolved
				continue
			}
		}
		break
	}
	return streamURL
}

func fetchPlaylistOrTuneIn(urlStr string) string {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   5 * time.Second,
	}
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "LogitechSqueezebox/9.0")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	bodyStr := string(bodyBytes)
	lines := strings.Split(bodyStr, "\n")
	var urls []string

	isPLS := strings.Contains(strings.ToLower(bodyStr), "[playlist]")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if isPLS {
			if strings.Contains(line, "=") {
				parts := strings.SplitN(line, "=", 2)
				val := strings.TrimSpace(parts[1])
				if strings.HasPrefix(strings.ToLower(val), "http://") || strings.HasPrefix(strings.ToLower(val), "https://") {
					urls = append(urls, val)
				}
			}
		} else {
			if strings.HasPrefix(strings.ToLower(line), "http://") || strings.HasPrefix(strings.ToLower(line), "https://") {
				urls = append(urls, line)
			}
		}
	}

	if len(urls) == 0 {
		return ""
	}

	// Filter out pre-rolls/ads if we have other URLs
	var cleanUrls []string
	for _, u := range urls {
		lowerU := strings.ToLower(u)
		isAd := strings.Contains(lowerU, "bump") ||
			strings.Contains(lowerU, "preroll") ||
			strings.Contains(lowerU, "sonic") ||
			strings.Contains(lowerU, "_pre") ||
			strings.Contains(lowerU, "/ad/") ||
			strings.Contains(lowerU, "ad_") ||
			strings.Contains(lowerU, "advert")
		if isAd {
			continue
		}
		cleanUrls = append(cleanUrls, u)
	}

	if len(cleanUrls) > 0 {
		return cleanUrls[0]
	}
	return urls[0]
}

type CLIRequest struct {
	Command string
	Args    []string
	Params  map[string]string
}

func parseCLIRequest(cmdArr []interface{}) CLIRequest {
	req := CLIRequest{
		Params: make(map[string]string),
	}
	if len(cmdArr) == 0 {
		return req
	}
	if cmd, ok := cmdArr[0].(string); ok {
		req.Command = cmd
	}

	for i := 1; i < len(cmdArr); i++ {
		val := cmdArr[i]
		if strVal, ok := val.(string); ok {
			if strings.Contains(strVal, ":") {
				parts := strings.SplitN(strVal, ":", 2)
				req.Params[parts[0]] = parts[1]
			} else {
				req.Args = append(req.Args, strVal)
			}
		} else {
			req.Args = append(req.Args, fmt.Sprintf("%v", val))
		}
	}
	return req
}

type ServerMetadata struct {
	IPAddress string
	Name      string
	UUID      string
	JSONPort  string
	CLIPort   string
	Version   string
}

type CometdRequest struct {
	Channel                  string                 `json:"channel"`
	ClientId                 string                 `json:"clientId,omitempty"`
	Id                       interface{}            `json:"id,omitempty"`
	Successful               bool                   `json:"successful"`
	Advice                   map[string]interface{} `json:"advice,omitempty"`
	Data                     map[string]interface{} `json:"data,omitempty"`
	Version                  string                 `json:"version,omitempty"`
	MinimumVersion           string                 `json:"minimumVersion,omitempty"`
	SupportedConnectionTypes []string               `json:"supportedConnectionTypes,omitempty"`
	Subscription             interface{}            `json:"subscription,omitempty"`
	ConnectionType           string                 `json:"connectionType,omitempty"`
}

type StreamingClient struct {
	events          chan []interface{}
	clientId        string
	pendingResponse map[string]interface{}
	pendingMu       sync.RWMutex
}

var (
	streamingClients   = make(map[string]*StreamingClient)
	streamingClientsMu sync.Mutex

	connectedPlayers   = make(map[string]PlayerInfo)
	connectedPlayersMu sync.RWMutex

	slimConn   net.Conn
	slimConnMu sync.Mutex

	globalServerMetadata *ServerMetadata
)

type PlayerPlaybackState struct {
	Mu           sync.Mutex
	Playing      bool
	PlayingTitle string
	PlayingDesc  string
	PlayingIcon  string
	PlayingURL   string
}

var (
	playerPlaybacks   = make(map[string]*PlayerPlaybackState)
	playerPlaybacksMu sync.Mutex
)

func getPlayerPlayback(mac string) *PlayerPlaybackState {
	playerPlaybacksMu.Lock()
	defer playerPlaybacksMu.Unlock()
	pb, ok := playerPlaybacks[mac]
	if !ok {
		pb = &PlayerPlaybackState{}
		playerPlaybacks[mac] = pb
	}
	return pb
}

type PresetItem struct {
	Title   string
	Subtext string
	Icon    string
	URL     string
}

var (
	presets   = make(map[int]PresetItem)
	presetsMu sync.Mutex
)

func initPresets() {
	presetsMu.Lock()
	defer presetsMu.Unlock()
	presets[1] = PresetItem{
		Title:   "SWR3",
		Subtext: "Pop music station",
		Icon:    "plugins/TuneIn/html/images/radiolocal.png",
		URL:     "https://liveradio.swr.de/sw282p3/swr3/",
	}
}

type PlayerInfo struct {
	MAC   string
	Name  string
	Model string
	UUID  string
}

func addConnectedPlayer(mac, name, model, uuid string) {
	connectedPlayersMu.Lock()
	connectedPlayers[mac] = PlayerInfo{MAC: mac, Name: name, Model: model, UUID: uuid}
	connectedPlayersMu.Unlock()
}

func removeConnectedPlayer(mac string) {
	connectedPlayersMu.Lock()
	delete(connectedPlayers, mac)
	connectedPlayersMu.Unlock()
}

func getConnectedPlayers() []PlayerInfo {
	connectedPlayersMu.RLock()
	defer connectedPlayersMu.RUnlock()
	result := make([]PlayerInfo, 0, len(connectedPlayers))
	for _, p := range connectedPlayers {
		result = append(result, p)
	}
	return result
}

func registerStreamingClient(clientId string) *StreamingClient {
	sc := &StreamingClient{
		events:          make(chan []interface{}, 100),
		clientId:        clientId,
		pendingResponse: nil,
	}
	streamingClientsMu.Lock()
	streamingClients[clientId] = sc
	streamingClientsMu.Unlock()
	return sc
}

func unregisterStreamingClient(clientId string) {
	streamingClientsMu.Lock()
	delete(streamingClients, clientId)
	streamingClientsMu.Unlock()
}

func getStreamingClient(clientId string) *StreamingClient {
	streamingClientsMu.Lock()
	defer streamingClientsMu.Unlock()
	return streamingClients[clientId]
}

type JsonRpcRequest struct {
	Id     interface{}   `json:"id"`
	Method string        `json:"method"`
	Params []interface{} `json:"params"`
}

type JsonRpcResponse struct {
	Id     interface{}            `json:"id"`
	Result map[string]interface{} `json:"result"`
}

// parseHELO parses the HELO payload and returns heloInfo struct
func parseHELO(payload []byte) heloInfo {
	h := heloInfo{}
	if len(payload) < 2 {
		return h
	}
	h.DeviceID = payload[0]
	h.Revision = payload[1]
	if len(payload) >= 8 {
		mac := payload[2:8]
		h.MAC = fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
			mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])
	}
	if len(payload) >= 36 {
		h.Lang = strings.TrimRight(string(payload[34:36]), "\x00")
	}
	if len(payload) > 36 {
		h.Capabilities = string(payload[36:])
	}
	return h
}

// heloInfo struct for HELO packet processing
// Using the same struct name but this needs to be defined above

type heloInfo struct {
	DeviceID     uint8
	Revision     uint8
	MAC          string
	Lang         string
	Capabilities string
}

// readPacket reads one complete SlimProto packet from a TCP connection.
func readPacket(r io.Reader) (opcode string, payload []byte, err error) {
	header := make([]byte, 8)
	if _, err = io.ReadFull(r, header); err != nil {
		return
	}
	opcode = strings.TrimRight(string(header[0:4]), "\x00")
	length := binary.BigEndian.Uint32(header[4:8])
	if length > 0 {
		payload = make([]byte, length)
		if _, err = io.ReadFull(r, payload); err != nil {
			return
		}
	}
	return
}

// sendPacket writes one complete SlimProto packet to a TCP connection (server → client).
func sendPacket(w io.Writer, opcode string, payload []byte) error {
	packetLen := 4 + len(payload)
	buf := make([]byte, 2+packetLen)
	binary.BigEndian.PutUint16(buf[0:2], uint16(packetLen))
	copy(buf[2:6], []byte(opcode))
	copy(buf[6:], payload)
	_, err := w.Write(buf)
	return err
}

// sendStrmCommand sends a strm packet telling the player to start streaming
// from a specific port. The player connects to serverIp:serverPort and receives
// audio data in the format specified by mode (e.g. 'm' = MP3/mad, 'p' = PCM).
func sendStrmCommand(conn net.Conn, serverPort uint16, mode byte, pcmSampleSize, pcmSampleRate, pcmChannels, pcmEndianness byte) error {
	payload := make([]byte, 28)
	payload[0] = 's'                     // command: start
	payload[1] = '1'                     // autostart (ASCII '1')
	payload[2] = mode
	payload[3] = pcmSampleSize
	payload[4] = pcmSampleRate
	payload[5] = pcmChannels
	payload[6] = pcmEndianness
	payload[7] = 1                       // threshold (1ms — minimal buffering)
	payload[8] = 0                       // spdifEnable
	payload[9] = 0                       // transitionPeriod
	payload[10] = 0                      // transitionType
	payload[11] = 0x00                   // flags
	payload[12] = 0                      // outputThreshold
	payload[13] = 0                      // slaves
	// bytes 14-17: replayGain = 0 (4 bytes)
	// bytes 18-19: serverPort (2 bytes BE)
	binary.BigEndian.PutUint16(payload[18:20], serverPort)
	// bytes 20-23: serverIp = 0 (use current server IP)
	// bytes 24-27: reserved
	return sendPacket(conn, "strm", payload)
}

// sendAudg sends an audio gain packet setting volume.
// The Lua SlimProto parser (SlimProto.lua:236) does `gainL = unpackNumber(packet, 5, 4) << 9`,
// so the value sent here is the raw volume 0–100 (NOT pre-shifted). Sending 50 yields
// gain = 50 << 9 = 25600 in decode:audioGain, giving ~39 % of full digital output.
func sendAudg(conn net.Conn, volume int) error {
	payload := make([]byte, 8)
	binary.BigEndian.PutUint32(payload[0:4], uint32(volume))
	binary.BigEndian.PutUint32(payload[4:8], uint32(volume))
	return sendPacket(conn, "audg", payload)
}

// sendStrmKeepalive sends a strm timestamp/keepalive packet.
func sendStrmKeepalive(conn net.Conn) error {
	payload := make([]byte, 28)
	payload[0] = 't' // timestamp command (keepalive)
	return sendPacket(conn, "strm", payload)
}

// sendStrmStop sends a strm stop command, telling the player to stop playback
// and flush its audio buffers. This must be called before sendStrmCommand to
// avoid the 6-9s delay caused by the player draining its old audio buffer.
func sendStrmStop(conn net.Conn) error {
	payload := make([]byte, 28)
	payload[0] = 'q' // stop/quit command
	return sendPacket(conn, "strm", payload)
}

// sendAude sends an audio enable/disable command to the player.
func sendAude(conn net.Conn, enable bool) error {
	val := byte(0)
	if enable {
		val = 1
	}
	return sendPacket(conn, "aude", []byte{val})
}

// Package-level stream lifecycle: when a new stream starts, any previous stream's
// goroutines (keepalive, data piping) are cancelled immediately so the player gets
// a clean transition instead of competing keepalive/connection state.
var (
	streamStopMu   sync.Mutex
	streamStartMu  sync.Mutex // serializes entry into stream/PCMTest handlers
	streamStop     chan struct{}
	streamDataLis  net.Listener // data listener, closed on stop to unblock Accept
)

func stopCurrentStream() {
	streamStopMu.Lock()
	if streamStop != nil {
		close(streamStop)
		streamStop = nil
	}
	// Close the listener to unblock Accept() in the old goroutine.
	// Do NOT close the player's data connection — that would send RST
	// while the player may still be reading, causing DSCO reason=2.
	// The old goroutine will close it in its defer when it exits.
	if streamDataLis != nil {
		streamDataLis.Close()
		streamDataLis = nil
	}
	streamStopMu.Unlock()
}

// handleButtonPress dispatches preset button presses from SqueezePlay's
// CometD /slim/request channel. Called in a new goroutine.
func handleButtonPress(playerId string, btnName string) {
	slimConnMu.Lock()
	conn := slimConn
	slimConnMu.Unlock()
	if conn == nil {
		fmt.Printf("[Button] no SlimProto connection\n")
		return
	}

	var presetNum int
	var action string
	if n, err := fmt.Sscanf(btnName, "preset_%d.%s", &presetNum, &action); err == nil && n == 2 {
		pb := getPlayerPlayback(playerId)
		if action == "hold" {
			pb.Mu.Lock()
			playing := pb.Playing
			title := pb.PlayingTitle
			desc := pb.PlayingDesc
			icon := pb.PlayingIcon
			urlStr := pb.PlayingURL
			pb.Mu.Unlock()

			if playing && urlStr != "" {
				presetsMu.Lock()
				presets[presetNum] = PresetItem{
					Title:   title,
					Subtext: desc,
					Icon:    icon,
					URL:     urlStr,
				}
				presetsMu.Unlock()
				fmt.Printf("[Button] Stored player %s stream to Preset %d: %s (%s)\n", playerId, presetNum, title, urlStr)
			} else {
				fmt.Printf("[Button] Cannot store Preset %d: player is not playing anything\n", presetNum)
			}
			return
		} else if action == "single" {
			presetsMu.Lock()
			item, ok := presets[presetNum]
			presetsMu.Unlock()

			if ok && item.URL != "" {
				fmt.Printf("[Button] Recalling Preset %d: playing %s (%s)\n", presetNum, item.Title, item.URL)
				pb.Mu.Lock()
				pb.Playing = true
				pb.PlayingTitle = item.Title
				pb.PlayingDesc = item.Subtext
				pb.PlayingIcon = item.Icon
				pb.PlayingURL = item.URL
				pb.Mu.Unlock()

				go runURLStream(conn, item.URL, pb)

				go func() {
					time.Sleep(150 * time.Millisecond)
					publishPlayerStatus()
				}()
			} else {
				fmt.Printf("[Button] Recall Preset %d failed: not set\n", presetNum)
			}
			return
		}
	}

	switch btnName {
	case "preset_2.single":
		fmt.Printf("[Button] preset 2 → PCM test\n")
		go runPCMTest(conn)
	default:
		fmt.Printf("[Button] unhandled button: %s\n", btnName)
	}
}

func handleJiveFavorites(respClid, playerId string, cmdArr []interface{}) {
	req := parseCLIRequest(cmdArr)
	if len(req.Args) == 0 {
		return
	}
	subCmd := req.Args[0]
	if subCmd == "set_preset" {
		keyStr := req.Params["key"]
		var presetNum int
		if _, err := fmt.Sscanf(keyStr, "%d", &presetNum); err == nil && presetNum >= 0 && presetNum <= 9 {
			pb := getPlayerPlayback(playerId)
			pb.Mu.Lock()
			playing := pb.Playing
			title := pb.PlayingTitle
			desc := pb.PlayingDesc
			icon := pb.PlayingIcon
			urlStr := pb.PlayingURL
			pb.Mu.Unlock()

			if playing && urlStr != "" {
				presetsMu.Lock()
				presets[presetNum] = PresetItem{
					Title:   title,
					Subtext: desc,
					Icon:    icon,
					URL:     urlStr,
				}
				presetsMu.Unlock()
				fmt.Printf("[Presets] Stored SqueezePlay player %s stream to Preset %d: %s (%s)\n", playerId, presetNum, title, urlStr)
			} else {
				fmt.Printf("[Presets] Cannot store Preset %d: player is not playing anything\n", presetNum)
			}
		}
	}
}

type IcyReader struct {
	reader      io.Reader
	metaInt     int
	bytesToNext int
	onMetadata  func(string)
}

func (r *IcyReader) Read(p []byte) (n int, err error) {
	if r.metaInt <= 0 {
		return r.reader.Read(p)
	}

	if r.bytesToNext == 0 {
		lenBuf := []byte{0}
		_, err := io.ReadFull(r.reader, lenBuf)
		if err != nil {
			return 0, err
		}
		metaLen := int(lenBuf[0]) * 16
		if metaLen > 0 {
			metaBuf := make([]byte, metaLen)
			_, err = io.ReadFull(r.reader, metaBuf)
			if err != nil {
				return 0, err
			}
			metaStr := string(metaBuf)
			if r.onMetadata != nil {
				r.onMetadata(metaStr)
			}
		}
		r.bytesToNext = r.metaInt
	}

	limit := len(p)
	if limit > r.bytesToNext {
		limit = r.bytesToNext
	}

	n, err = r.reader.Read(p[:limit])
	if n > 0 {
		r.bytesToNext -= n
	}
	return n, err
}

func parseIcyMetadata(metaStr string) (string, string) {
	idx := strings.Index(metaStr, "StreamTitle='")
	if idx == -1 {
		return "", ""
	}
	sub := metaStr[idx+len("StreamTitle='"):]
	endIdx := strings.Index(sub, "';")
	if endIdx == -1 {
		return "", ""
	}
	fullTitle := sub[:endIdx]

	artist := ""
	title := fullTitle
	if dashIdx := strings.Index(fullTitle, " - "); dashIdx != -1 {
		artist = fullTitle[:dashIdx]
		title = fullTitle[dashIdx+3:]
	}
	return title, artist
}

// runURLStream starts streaming the given URL to the player.
// Runs entirely in a new goroutine (called from handleButtonPress and direct HTTP).
func runURLStream(conn net.Conn, streamURL string, pb *PlayerPlaybackState) {
	streamURL = resolveStreamURL(streamURL)
	streamStartTime := time.Now()

	// Start upstream fetch early so data may be ready when the player connects
	fetchCh := make(chan streamFetchResult, 1)
	go func() {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext: (&net.Dialer{
				Timeout: 10 * time.Second,
			}).DialContext,
		}
		insecureClient := &http.Client{Transport: tr}
		req, err := http.NewRequest("GET", streamURL, nil)
		if err != nil {
			fmt.Printf("[Stream] request create failed: %v\n", err)
			fetchCh <- streamFetchResult{err: err}
			return
		}
		req.Header.Set("Icy-MetaData", "1")
		req.Header.Set("User-Agent", "SlimProto/1.0")

		resp, err := insecureClient.Do(req)
		if err != nil {
			fmt.Printf("[Stream] upstream fetch failed after %v: %v\n", time.Since(streamStartTime), err)
			fetchCh <- streamFetchResult{err: err}
			return
		}

		icyMetaInt := 0
		if icyMetaIntStr := resp.Header.Get("icy-metaint"); icyMetaIntStr != "" {
			if mi, err := strconv.Atoi(icyMetaIntStr); err == nil {
				icyMetaInt = mi
			}
		}

		ct := "audio/mpeg"
		if h := resp.Header.Get("Content-Type"); h != "" {
			ct = h
		}
		fmt.Printf("[Stream] upstream connected in %v, status=%s, content-type=%s, icy-metaint=%d\n", time.Since(streamStartTime), resp.Status, ct, icyMetaInt)
		fetchCh <- streamFetchResult{
			reader:      resp.Body,
			contentType: ct,
			httpStatus:  resp.Status,
			icyMetaInt:  icyMetaInt,
		}
	}()

	// Serialize with other stream/PCMTest requests to avoid races
	streamStartMu.Lock()
	defer streamStartMu.Unlock()

	// Tell the player to stop and flush its audio buffers
	if err := sendStrmStop(conn); err != nil {
		fmt.Printf("[Stream] sendStrmStop error: %v\n", err)
	}

	// Cancel any previous stream first
	stopCurrentStream()

	// Enable audio output — don't force volume, let the player use its knob position
	if err := sendAude(conn, true); err != nil {
		fmt.Printf("[Stream] sendAude error: %v\n", err)
	}
	listener, err := listenTCP4(":0")
	if err != nil {
		fmt.Printf("[Stream] listen error: %v\n", err)
		return
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	fmt.Printf("[Stream] t=%s listening on port %d for URL: %s\n", time.Since(streamStartTime), port, streamURL)

	// Register the listener BEFORE sendStrmCommand
	streamStopMu.Lock()
	streamStop = make(chan struct{})
	thisStop := streamStop
	streamDataLis = listener
	streamStopMu.Unlock()

	if err := sendStrmCommand(conn, port, 'm', 0, 0, 0, 0); err != nil {
		listener.Close()
		fmt.Printf("[Stream] sendStrmCommand error: %v\n", err)
		return
	}

	// Keepalive goroutine
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := sendStrmKeepalive(conn); err != nil {
					return
				}
			case <-thisStop:
				return
			}
		}
	}()

	go func() {
		defer listener.Close()
		defer func() {
			streamStopMu.Lock()
			if streamStop == thisStop {
				close(streamStop)
				streamStop = nil
			}
			streamStopMu.Unlock()
			if pb != nil {
				pb.Mu.Lock()
				pb.Playing = false
				pb.Mu.Unlock()
				publishPlayerStatus()
			}
		}()

		playerConn, err := listener.Accept()
		if err != nil {
			fmt.Printf("[Stream] accept error: %v\n", err)
			return
		}
		defer playerConn.Close()

		// Write HTTP headers immediately so the player can start buffering
		headerStr := "HTTP/1.0 200 OK\r\nContent-Type: audio/mpeg\r\nConnection: close\r\n\r\n"
		if _, err := playerConn.Write([]byte(headerStr)); err != nil {
			fmt.Printf("[Stream] write header error: %v\n", err)
			return
		}
		fmt.Printf("[Stream] t=%s wrote HTTP headers\n", time.Since(streamStartTime))

		// Wait for the upstream fetch
		fmt.Printf("[Stream] t=%s waiting for upstream fetch...\n", time.Since(streamStartTime))
		result := <-fetchCh
		if result.err != nil {
			fmt.Printf("[Stream] http get error: %v\n", result.err)
			return
		}
		defer result.reader.Close()
		icyReader := &IcyReader{
			reader:      result.reader,
			metaInt:     result.icyMetaInt,
			bytesToNext: result.icyMetaInt,
			onMetadata: func(metaStr string) {
				title, artist := parseIcyMetadata(metaStr)
				if title != "" {
					fmt.Printf("[ICY] Song update: %s - %s\n", artist, title)
					if pb != nil {
						pb.Mu.Lock()
						pb.PlayingTitle = title
						pb.PlayingDesc = artist
						pb.Mu.Unlock()
						publishPlayerStatus()
					}
				}
			},
		}
		fmt.Printf("[Stream] t=%s upstream ready, proxying %s (icy-metaint=%d)\n", time.Since(streamStartTime), streamURL, result.icyMetaInt)

		totalWritten := int64(0)
		lastLog := time.Now()
		buf := make([]byte, 65536)
		firstLog := true
		for {
			select {
			case <-thisStop:
				fmt.Printf("[Stream] cancelled by new stream\n")
				return
			default:
			}

			n, err := icyReader.Read(buf)
			if n > 0 {
				if firstLog {
					fmt.Printf("[Stream] first %d bytes of content: % x\n", n, buf[:n])
					firstLog = false
				}
				if _, werr := playerConn.Write(buf[:n]); werr != nil {
					fmt.Printf("[Stream] write error: %v\n", werr)
					return
				}
				totalWritten += int64(n)
				if time.Since(lastLog) > 5*time.Second {
					fmt.Printf("[Stream] wrote %d bytes so far\n", totalWritten)
					lastLog = time.Now()
				}
			}
			if err != nil {
				if err == io.EOF {
					fmt.Printf("[Stream] stream ended, total written: %d bytes\n", totalWritten)
					return
				}
				fmt.Printf("[Stream] read error: %v\n", err)
				return
			}
		}
	}()
}

// runPCMTest starts generating a PCM sine wave on the player.
// Runs entirely in a new goroutine (called from handleButtonPress).
func runPCMTest(conn net.Conn) {
	streamStartMu.Lock()
	defer streamStartMu.Unlock()

	// Tell the player to stop and flush its audio buffers first
	if err := sendStrmStop(conn); err != nil {
		fmt.Printf("[PCMTest] sendStrmStop error: %v\n", err)
	}

	// Cancel any previous stream first
	stopCurrentStream()

	listener, err := listenTCP4(":0")
	if err != nil {
		fmt.Printf("[PCMTest] listen error: %v\n", err)
		return
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	fmt.Printf("[PCMTest] listening on port %d\n", port)

	// Enable audio output — let the player use its knob position
	if err := sendAude(conn, true); err != nil {
		fmt.Printf("[PCMTest] sendAude error: %v\n", err)
	}

	// Register the listener BEFORE sendStrmCommand
	streamStopMu.Lock()
	streamStop = make(chan struct{})
	thisStop := streamStop
	streamDataLis = listener
	streamStopMu.Unlock()

	// PCM: 16-bit signed LE stereo 44100 Hz
	if err := sendStrmCommand(conn, port, 'p', '1', '3', '2', '1'); err != nil {
		listener.Close()
		fmt.Printf("[PCMTest] sendStrmCommand error: %v\n", err)
		return
	}

	// Send an immediate strm timestamp so the player doesn't time out
	if err := sendStrmKeepalive(conn); err != nil {
		fmt.Printf("[PCMTest] initial keepalive error: %v\n", err)
	}

	// Keepalive goroutine
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := sendStrmKeepalive(conn); err != nil {
					return
				}
			case <-thisStop:
				return
			}
		}
	}()

	go func() {
		defer listener.Close()
		defer func() {
			streamStopMu.Lock()
			if streamStop == thisStop {
				close(streamStop)
				streamStop = nil
			}
			streamStopMu.Unlock()
		}()

		playerConn, err := listener.Accept()
		if err != nil {
			fmt.Printf("[PCMTest] accept error: %v\n", err)
			return
		}
		defer playerConn.Close()

		fmt.Printf("[PCMTest] player connected\n")

		// HTTP headers required by stream_readL
		header := "HTTP/1.0 200 OK\r\nContent-Type: audio/L16\r\nConnection: close\r\n\r\n"
		if _, err := playerConn.Write([]byte(header)); err != nil {
			fmt.Printf("[PCMTest] write header error: %v\n", err)
			return
		}
		fmt.Printf("[PCMTest] wrote HTTP headers (%d bytes)\n", len(header))

		// Generate PCM sine wave: 440 Hz, 16-bit signed LE stereo at 44100 Hz
		const (
			sampleRate = 44100
			freq       = 440.0
			amplitude  = 3000.0
			duration   = 10 * time.Second
		)

		totalSamples := int(sampleRate * int(duration.Seconds()))
		buf := make([]byte, 4096)
		sampleIdx := 0
		totalWritten := int64(0)
		lastLog := time.Now()

		for sampleIdx < totalSamples {
			select {
			case <-thisStop:
				fmt.Printf("[PCMTest] cancelled by new stream\n")
				return
			default:
			}

			bytesPerSample := 2
			channels := 2
			samplesPerBuf := len(buf) / (bytesPerSample * channels)
			off := 0
			for s := 0; s < samplesPerBuf && sampleIdx < totalSamples; s++ {
				t := float64(sampleIdx) / sampleRate
				val := int16(amplitude * math.Sin(2*math.Pi*freq*t))
				binary.LittleEndian.PutUint16(buf[off:off+2], uint16(val))
				binary.LittleEndian.PutUint16(buf[off+2:off+4], uint16(val))
				off += 4
				sampleIdx++
			}

			if _, werr := playerConn.Write(buf[:off]); werr != nil {
				fmt.Printf("[PCMTest] write error: %v\n", werr)
				return
			}
			totalWritten += int64(off)
			if time.Since(lastLog) > 2*time.Second {
				fmt.Printf("[PCMTest] wrote %d bytes (%d samples)\n", totalWritten, sampleIdx)
				lastLog = time.Now()
			}
		}

		fmt.Printf("[PCMTest] done, total written: %d bytes (%d samples)\n", totalWritten, sampleIdx)
	}()
}

type streamFetchResult struct {
	reader      io.ReadCloser
	contentType string
	httpStatus  string
	icyMetaInt  int
	err         error
}

func handleStreamStart(w http.ResponseWriter, r *http.Request) {
	streamURL := r.URL.Query().Get("url")
	if streamURL != "" {
		streamURL = resolveStreamURL(streamURL)
	}
	streamFile := r.URL.Query().Get("file")

	if streamURL == "" && streamFile == "" {
		http.Error(w, "missing url or file parameter", http.StatusBadRequest)
		return
	}

	slimConnMu.Lock()
	conn := slimConn
	slimConnMu.Unlock()
	if conn == nil {
		http.Error(w, "no SlimProto connection", http.StatusServiceUnavailable)
		return
	}

	// Start upstream fetch early so data may be ready when the player connects
	var fetchCh chan streamFetchResult
	streamStartTime := time.Now()
	if streamURL != "" {
		fetchCh = make(chan streamFetchResult, 1)
		go func() {
			tr := &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				DialContext: (&net.Dialer{
					Timeout: 10 * time.Second,
				}).DialContext,
			}
			insecureClient := &http.Client{Transport: tr}
			resp, err := insecureClient.Get(streamURL)
			if err != nil {
				fmt.Printf("[Stream] upstream fetch failed after %v: %v\n", time.Since(streamStartTime), err)
				fetchCh <- streamFetchResult{err: err}
				return
			}
			ct := "audio/mpeg"
			if h := resp.Header.Get("Content-Type"); h != "" {
				ct = h
			}
			fmt.Printf("[Stream] upstream connected in %v, status=%s, content-type=%s\n", time.Since(streamStartTime), resp.Status, ct)
			fetchCh <- streamFetchResult{
				reader:      resp.Body,
				contentType: ct,
				httpStatus:  resp.Status,
			}
		}()
	}

	// Serialize with other stream/PCMTest requests to avoid races
	streamStartMu.Lock()
	defer streamStartMu.Unlock()

	// Tell the player to stop and flush its audio buffers BEFORE we make any
	// changes. This avoids a 6-9s delay where the player continues playing
	// from its internal buffer before switching to the new stream.
	if err := sendStrmStop(conn); err != nil {
		fmt.Printf("[Stream] sendStrmStop error: %v\n", err)
	}

	// Cancel any previous stream first, so the player doesn't see the old
	// data connection drop while processing the new strm command.
	stopCurrentStream()

	// Enable audio output — don't force volume, let the player use its knob position
	if err := sendAude(conn, true); err != nil {
		fmt.Printf("[Stream] sendAude error: %v\n", err)
	}
	listener, err := listenTCP4(":0")
	if err != nil {
		fmt.Printf("[Stream] listen error: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)

	if streamURL != "" {
		fmt.Printf("[Stream] t=%s listening on port %d for URL: %s\n", time.Since(streamStartTime), port, streamURL)
	} else {
		fmt.Printf("[Stream] listening on port %d for file: %s\n", port, streamFile)
	}

	// Register the listener BEFORE sendStrmCommand so the stream lifecycle is
	// fully set up when the player starts connecting.
	streamStopMu.Lock()
	streamStop = make(chan struct{})
	thisStop := streamStop
	streamDataLis = listener
	streamStopMu.Unlock()

	if err := sendStrmCommand(conn, port, 'm', 0, 0, 0, 0); err != nil {
		listener.Close()
		fmt.Printf("[Stream] sendStrmCommand error: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Start keepalive goroutine (strm 't' every ~4s)
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := sendStrmKeepalive(conn); err != nil {
					return
				}
			case <-thisStop:
				return
			}
		}
	}()

	go func() {
		defer listener.Close()
		// Only close thisStop if it's still the current streamStop (not replaced by a new stream)
		defer func() {
			streamStopMu.Lock()
			if streamStop == thisStop {
				close(streamStop)
				streamStop = nil
			}
			streamStopMu.Unlock()
		}()

		playerConn, err := listener.Accept()
		if err != nil {
			fmt.Printf("[Stream] accept error: %v\n", err)
			return
		}
		defer playerConn.Close()

		// Write HTTP headers immediately so the player can start buffering
		headerStr := "HTTP/1.0 200 OK\r\nContent-Type: audio/mpeg\r\nConnection: close\r\n\r\n"
		if streamFile != "" {
			headerStr = "HTTP/1.0 200 OK\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n"
		}
		if _, err := playerConn.Write([]byte(headerStr)); err != nil {
			fmt.Printf("[Stream] write header error: %v\n", err)
			return
		}
		fmt.Printf("[Stream] t=%s wrote HTTP headers\n", time.Since(streamStartTime))

		var reader io.ReadCloser
		if streamFile != "" {
			f, err := os.Open(streamFile)
			if err != nil {
				fmt.Printf("[Stream] open file error: %v\n", err)
				return
			}
			defer f.Close()
			reader = f
			fmt.Printf("[Stream] player connected, serving file %s\n", streamFile)
		} else {
			// Wait for the upstream fetch started in the handler
			fmt.Printf("[Stream] t=%s waiting for upstream fetch...\n", time.Since(streamStartTime))
			result := <-fetchCh
			if result.err != nil {
				fmt.Printf("[Stream] http get error: %v\n", result.err)
				return
			}
			defer result.reader.Close()
			reader = result.reader
			fmt.Printf("[Stream] t=%s upstream ready, proxying %s\n", time.Since(streamStartTime), streamURL)
		}

		totalWritten := int64(0)
		lastLog := time.Now()
		buf := make([]byte, 65536)
		firstLog := true
		for {
			// Check if a new stream has cancelled us
			select {
			case <-thisStop:
				fmt.Printf("[Stream] cancelled by new stream\n")
				return
			default:
			}

			n, err := reader.Read(buf)
			if n > 0 {
				if firstLog {
					fmt.Printf("[Stream] first %d bytes of content: % x\n", n, buf[:n])
					firstLog = false
				}
				if _, werr := playerConn.Write(buf[:n]); werr != nil {
					fmt.Printf("[Stream] write error: %v\n", werr)
					return
				}
				totalWritten += int64(n)
				if time.Since(lastLog) > 5*time.Second {
					fmt.Printf("[Stream] wrote %d bytes so far\n", totalWritten)
					lastLog = time.Now()
				}
			}
			if err != nil {
				if err == io.EOF {
					fmt.Printf("[Stream] stream ended, total written: %d bytes\n", totalWritten)
					return
				}
				fmt.Printf("[Stream] read error: %v\n", err)
				return
			}
		}
	}()

	if streamURL != "" {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "streaming %s on port %d\n", streamURL, port)
	} else {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "streaming file %s on port %d\n", streamFile, port)
	}
}

// handlePCMTest streams a generated PCM sine wave to test the audio output chain
// without involving the MP3 decoder. A clean 440 Hz tone means the output chain
// (ALSA, gain, format) is working; noise/white noise means the bug is downstream
// of the decoder.
func handlePCMTest(w http.ResponseWriter, r *http.Request) {
	slimConnMu.Lock()
	conn := slimConn
	slimConnMu.Unlock()
	if conn == nil {
		http.Error(w, "no SlimProto connection", http.StatusServiceUnavailable)
		return
	}

	// Serialize with other stream/PCMTest requests to avoid races
	streamStartMu.Lock()
	defer streamStartMu.Unlock()

	// Tell the player to stop and flush its audio buffers first
	if err := sendStrmStop(conn); err != nil {
		fmt.Printf("[PCMTest] sendStrmStop error: %v\n", err)
	}

	// Cancel any previous stream first
	stopCurrentStream()

	listener, err := listenTCP4(":0")
	if err != nil {
		fmt.Printf("[PCMTest] listen error: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	fmt.Printf("[PCMTest] listening on port %d\n", port)

	// Enable audio output — let the player use its knob position
	if err := sendAude(conn, true); err != nil {
		fmt.Printf("[PCMTest] sendAude error: %v\n", err)
	}

	// Register the listener BEFORE sendStrmCommand
	streamStopMu.Lock()
	streamStop = make(chan struct{})
	thisStop := streamStop
	streamDataLis = listener
	streamStopMu.Unlock()

	// PCM: 16-bit signed LE stereo 44100 Hz
	// Note: the Radio's firmware expects ASCII characters for these fields
	// pcmSampleSize = '1' (16-bit)
	// pcmSampleRate = '3' (44100 Hz)
	// pcmChannels  = '2' (stereo)
	// pcmEndianness = '1' (little endian)
	if err := sendStrmCommand(conn, port, 'p', '1', '3', '2', '1'); err != nil {
		listener.Close()
		fmt.Printf("[PCMTest] sendStrmCommand error: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Send an immediate strm timestamp so the player doesn't time out before the first keepalive
	if err := sendStrmKeepalive(conn); err != nil {
		fmt.Printf("[PCMTest] initial keepalive error: %v\n", err)
	}

	// Keepalive goroutine
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := sendStrmKeepalive(conn); err != nil {
					return
				}
			case <-thisStop:
				return
			}
		}
	}()

	go func() {
		defer listener.Close()
		// Only close thisStop if it's still the current streamStop (not replaced by a new stream)
		defer func() {
			streamStopMu.Lock()
			if streamStop == thisStop {
				close(streamStop)
				streamStop = nil
			}
			streamStopMu.Unlock()
		}()

		playerConn, err := listener.Accept()
		if err != nil {
			fmt.Printf("[PCMTest] accept error: %v\n", err)
			return
		}
		defer playerConn.Close()

		fmt.Printf("[PCMTest] player connected\n")

		// HTTP headers required by stream_readL
		header := "HTTP/1.0 200 OK\r\nContent-Type: audio/L16\r\nConnection: close\r\n\r\n"
		if _, err := playerConn.Write([]byte(header)); err != nil {
			fmt.Printf("[PCMTest] write header error: %v\n", err)
			return
		}
		fmt.Printf("[PCMTest] wrote HTTP headers (%d bytes)\n", len(header))

		// Generate PCM sine wave: 440 Hz, 16-bit signed LE stereo at 44100 Hz
		const (
			sampleRate = 44100
			freq       = 440.0
			amplitude  = 3000.0
			duration   = 10 * time.Second
		)

		totalSamples := int(sampleRate * int(duration.Seconds()))
		buf := make([]byte, 4096)
		sampleIdx := 0
		totalWritten := int64(0)
		lastLog := time.Now()

		for sampleIdx < totalSamples {
			select {
			case <-thisStop:
				fmt.Printf("[PCMTest] cancelled by new stream\n")
				return
			default:
			}

			bytesPerSample := 2
			channels := 2
			samplesPerBuf := len(buf) / (bytesPerSample * channels)
			off := 0
			for s := 0; s < samplesPerBuf && sampleIdx < totalSamples; s++ {
				t := float64(sampleIdx) / sampleRate
				val := int16(amplitude * math.Sin(2*math.Pi*freq*t))
				binary.LittleEndian.PutUint16(buf[off:off+2], uint16(val))
				binary.LittleEndian.PutUint16(buf[off+2:off+4], uint16(val)) // same for right channel
				off += 4
				sampleIdx++
			}

			if _, werr := playerConn.Write(buf[:off]); werr != nil {
				fmt.Printf("[PCMTest] write error: %v\n", werr)
				return
			}
			totalWritten += int64(off)
			if time.Since(lastLog) > 2*time.Second {
				fmt.Printf("[PCMTest] wrote %d bytes (%d samples)\n", totalWritten, sampleIdx)
				lastLog = time.Now()
			}
		}

		fmt.Printf("[PCMTest] done, total written: %d bytes (%d samples)\n", totalWritten, sampleIdx)
	}()

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "streaming PCM sine wave on port %d (10s)\n", port)
}

// sendVers sends the server version string.
func sendVers(conn net.Conn, version string) error {
	return sendPacket(conn, "vers", []byte(version))
}

// sendSetdName sends the player name.
func sendSetdName(conn net.Conn, name string) error {
	payload := make([]byte, 1+len(name))
	payload[0] = 0
	copy(payload[1:], []byte(name))
	return sendPacket(conn, "setd", payload)
}

// sendStrm sends a strm packet with metadata for a track.
func sendStrm(conn net.Conn, command byte, trackNum uint32, title string, artist string, album string, duration uint32) error {
	payload := []byte{command}

	trackNumBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(trackNumBytes, trackNum)
	payload = append(payload, trackNumBytes...)

	payload = append(payload, byte(len(title)))
	payload = append(payload, []byte(title)...)

	payload = append(payload, byte(len(artist)))
	payload = append(payload, []byte(artist)...)

	payload = append(payload, byte(len(album)))
	payload = append(payload, []byte(album)...)

	durationBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(durationBytes, duration)
	payload = append(payload, durationBytes...)

	payload = append(payload, 0xAC, 0x44)
	payload = append(payload, 0x00, 0x00)
	payload = append(payload, 0x00, 0x18)

	return sendPacket(conn, "strm", payload)
}

// handleSlimProtoConn manages a single player TCP connection.
func handleSlimProtoConn(conn net.Conn, m *ServerMetadata) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	fmt.Printf("[TCP] New SlimProto connection from %s\n", remote)

	slimConnMu.Lock()
	slimConn = conn
	slimConnMu.Unlock()

	heloReceived := false
	clientMAC := ""
	defer func() {
		if clientMAC != "" {
			removeConnectedPlayer(clientMAC)
			pushServerstatusToAll(m)
		}
	}()

	// Keepalive goroutine: SqueezePlay silently discards "test" packets
	// but resets its 60-second inactivity timer on any received data.
	done := make(chan struct{})
	defer close(done)
		go func() {
			ticker := time.NewTicker(25 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := sendPacket(conn, "test", []byte{}); err != nil {
						return
					}
					fmt.Printf("[TCP] %s keepalive sent\n", remote)
				case <-done:
					return
				}
			}
		}()

	for {
		opcode, payload, err := readPacket(conn)
		if err != nil {
			if err != io.EOF {
				fmt.Printf("[TCP] %s read error: %v\n", remote, err)
			} else {
				fmt.Printf("[TCP] %s disconnected\n", remote)
			}
			return
		}

		switch opcode {
		case "HELO":
			h := parseHELO(payload)
			fmt.Printf("[TCP] HELO from %s  device=%d rev=%d mac=%s lang=%s caps=%q\n",
				remote, h.DeviceID, h.Revision, h.MAC, h.Lang, h.Capabilities)

			heloReceived = true
			clientMAC = h.MAC

			addConnectedPlayer(h.MAC, h.MAC, "squeezeplay", "player-"+h.MAC)
			pushServerstatusToAll(m)

			if err := sendVers(conn, m.Version); err != nil {
				fmt.Printf("[TCP] %s error sending vers: %v\n", remote, err)
				return
			}

			if err := sendSetdName(conn, m.Name); err != nil {
				fmt.Printf("[TCP] %s error sending setd name: %v\n", remote, err)
				return
			}

			fmt.Printf("[TCP] %s HELO complete, connection established\n", remote)

			// Send additional SETD messages so the client receives enough
			// within its read timeout period to consider setup complete.
			for _, kv := range []struct{ cmd byte; val string }{
				{1, "EN"},    // language
				{2, "1"},     // canRandomPlay
				{3, "1"},     // canMusicFolder
				{4, "1"},     // canInternetRadio
			} {
				payload := []byte{kv.cmd}
				payload = append(payload, []byte(kv.val)...)
				if err := sendPacket(conn, "setd", payload); err != nil {
					fmt.Printf("[TCP] %s error sending SETD %d: %v\n", remote, kv.cmd, err)
					return
				}
				fmt.Printf("[TCP] %s SETD %d sent\n", remote, kv.cmd)
			}

			// Send initial strm metadata packet — SqueezePlay needs this
			// to initialize its audio pipeline.
			if err := sendStrm(conn, 0, 0, "", "", "", 0); err != nil {
				fmt.Printf("[TCP] %s error sending initial strm: %v\n", remote, err)
				return
			}
			fmt.Printf("[TCP] %s initial strm sent\n", remote)

		case "STAT":
			if !heloReceived {
				fmt.Printf("[TCP] %s STAT before HELO, ignoring\n", remote)
				continue
			}
			event := ""
			if len(payload) >= 4 {
				event = strings.TrimRight(string(payload[0:4]), "\x00")
			}
			extra := ""
			if len(payload) > 4 {
				extra = fmt.Sprintf(" %x", payload[4:])
			}
			fmt.Printf("[TCP] %s STAT event=%q (%d bytes)%s\n", remote, event, len(payload), extra)

		case "RESP":
			// Player echoes HTTP response headers from the data stream connection
			// back on the control connection. No action needed.
			if len(payload) > 0 {
				fmt.Printf("[TCP] %s RESP (%d bytes)\n", remote, len(payload))
			}

		case "BYE!":
			fmt.Printf("[TCP] %s BYE! received\n", remote)
			if clientMAC != "" {
				removeConnectedPlayer(clientMAC)
				pushServerstatusToAll(m)
				clientMAC = "" // defer cleanup will see empty and skip
			}
			return

		case "DSCO":
			reason := uint8(0)
			if len(payload) >= 1 {
				reason = payload[0]
			}
			fmt.Printf("[TCP] %s DSCO reason=%d (stream disconnected, NOT control connection)\n", remote, reason)
			// DSCO is a stream status event, NOT a disconnection of the control connection.
			// The player sends it when the audio stream's TCP data connection is dropped
			// (e.g. because the server closed it during a stream switch). We must NOT return
			// here — that would close the SlimProto control connection, forcing the player
			// to reconnect and adding 6–9s of delay on every track change.

		case "setd":
			if len(payload) >= 1 {
				cmd := payload[0]
				switch cmd {
				case 5: // volume change from knob
					if len(payload) >= 3 {
						vol := int(binary.BigEndian.Uint16(payload[1:3]))
						fmt.Printf("[TCP] %s volume: %d\n", remote, vol)
					} else {
						fmt.Printf("[TCP] %s setd cmd=%d raw=%x\n", remote, cmd, payload)
					}
				default:
					fmt.Printf("[TCP] %s setd cmd=%d raw=%x\n", remote, cmd, payload)
				}
			} else {
				fmt.Printf("[TCP] %s setd (empty)\n", remote)
			}

		default:
			fmt.Printf("[TCP] %s unknown opcode %q (%d bytes) %x\n", remote, opcode, len(payload), payload)
		}
	}
}

// pushServerstatusToAll pushes a serverstatus event to all streaming CometD clients.
func pushServerstatusToAll(m *ServerMetadata) {
	players := getConnectedPlayers()
	playerCount := len(players)

	playersLoop := make([]map[string]interface{}, 0, playerCount)
	for _, p := range players {
		playersLoop = append(playersLoop, map[string]interface{}{
			"playerid":  p.MAC,
			"name":      p.Name,
			"model":     p.Model,
			"connected": 1,
			"power":     1,
			"uuid":      p.UUID,
			"ip":        m.IPAddress,
		})
	}

	data := map[string]interface{}{
		"version":         m.Version,
		"uuid":            m.UUID,
		"name":            m.Name,
		"ip":              m.IPAddress,
		"port":            m.JSONPort,
		"cliport":         m.CLIPort,
		"compatible":      true,
		"player count":    playerCount,
		"players_loop":    playersLoop,
		"canMusicFolder":  true,
		"canRandomPlay":   true,
		"canInternetRadio": true,
		"canRemoteStream": true,
		"lang":            "EN",
	}

	streamingClientsMu.Lock()
	defer streamingClientsMu.Unlock()
	for clientId, sc := range streamingClients {
		select {
		case sc.events <- []interface{}{
			map[string]interface{}{
				"channel": "/" + clientId + "/slim/serverstatus",
				"data":    data,
			},
		}:
		default:
			fmt.Printf("[CometD] dropping serverstatus push for clientId=%s (channel full)\n", clientId)
		}
	}
}

// startTCPServer starts the TCP SlimProto control server.
func startTCPServer(metadata *ServerMetadata) {
	addr := fmt.Sprintf(":%d", DefaultPort)
	listener, err := listenTCP4(addr)
	if err != nil {
		fmt.Printf("[TCP] Error starting TCP listener on %s: %v\n", addr, err)
		os.Exit(1)
	}
	fmt.Printf("Listening for SlimProto TCP connections on %s...\n", addr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Printf("[TCP] Accept error: %v\n", err)
			continue
		}
		go handleSlimProtoConn(conn, metadata)
	}
}

// handleUDPPacket handles incoming UDP discovery requests from clients
func handleUDPPacket(conn *net.UDPConn, data []byte, raddr *net.UDPAddr, m *ServerMetadata, mode string) {
	switch data[0] {
	case 'e': // TLV discovery request
		tags := parseTLVs(data[1:])
		fmt.Printf("[%s] TLV request from %v tags=%v\n", mode, raddr, tags)
		rsp := buildTLVResponse(tags, m)
		conn.WriteToUDP(rsp, raddr)
		fmt.Printf("[%s] TLV response sent to %v\n", mode, raddr)

	case 'd': // legacy discovery request
		fmt.Printf("[%s] Legacy request from %v\n", mode, raddr)
		conn.WriteToUDP(buildLegacyResponse(m), raddr)
		fmt.Printf("[%s] Legacy response sent to %v\n", mode, raddr)

	default:
		fmt.Printf("[%s] Unknown UDP prefix 0x%02x from %v\n", mode, data[0], raddr)
	}
}

// startUDPListener starts UDP listener for SlimProto discovery
func startUDPListener(readConn, writeConn *net.UDPConn, m *ServerMetadata, mode string) {
	buf := make([]byte, 1024)
	for {
		n, raddr, err := readConn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		if n > 0 {
			handleUDPPacket(writeConn, buf[:n], raddr, m, mode)
		}
	}
}

// parseTLVs extracts tags from TLV payload
func parseTLVs(data []byte) []string {
	var tags []string
	idx := 0
	for idx <= len(data)-5 {
		tag := string(data[idx : idx+4])
		length := int(data[idx+4])
		tags = append(tags, tag)
		idx += 5 + length
	}
	return tags
}

// buildTLVResponse constructs a TLV response with requested server metadata
func buildTLVResponse(tags []string, m *ServerMetadata) []byte {
	if len(tags) == 0 {
		tags = []string{"NAME", "IPAD", "JSON", "VERS", "UUID"}
	}
	rsp := []byte{'E'}
	for _, tag := range tags {
		var val string
		switch tag {
		case "NAME":
			val = m.Name
		case "IPAD":
			val = m.IPAddress
		case "JSON":
			val = m.JSONPort
		case "CLIP":
			val = m.CLIPort
		case "VERS":
			val = m.Version
		case "UUID":
			val = m.UUID
		}
		if val != "" {
			valLen := len(val)
			if valLen > 255 {
				valLen = 255
			}
			rsp = append(rsp, []byte(tag)...)
			rsp = append(rsp, byte(valLen))
			rsp = append(rsp, []byte(val[:valLen])...)
		}
	}
	return rsp
}

// buildLegacyResponse constructs a legacy UDP discovery response
func buildLegacyResponse(m *ServerMetadata) []byte {
	rsp := make([]byte, 17)
	rsp[0] = 'D'
	n := m.IPAddress
	if len(n) > 16 {
		n = n[:16]
	}
	copy(rsp[1:], []byte(n))
	for i := 1 + len(n); i < 17; i++ {
		rsp[i] = 0
	}
	return rsp
}

// joinMulticast tries to join the multicast group for multicasting support
func joinMulticast(conn *net.UDPConn, group net.IP) error {
	if c, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: group, Port: DefaultPort}); err == nil {
		defer c.Close()
	}
	return nil
}

// A 40x40 gray PNG placeholder image embedded directly.
// Generated as a minimal valid PNG with a neutral gray fill.
var placeholderPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xde, 0x00, 0x00, 0x00,
	0x10, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x62, 0x3a, 0x73, 0xe6, 0x0c,
	0x20, 0x00, 0x00, 0xff, 0xff, 0x04, 0xd4, 0x02, 0x67, 0x77, 0xfa, 0xa8,
	0xe6, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60,
	0x82,
}

// handleArtwork serves placeholder images for artwork requests.
// The SqueezePlay client requests images at /music/<id>/cover[_spec][.ext]
func handleArtwork(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	fmt.Printf("[HTTP] artwork request: %s\n", path)

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(placeholderPNG)))
	w.WriteHeader(http.StatusOK)
	w.Write(placeholderPNG)
}

func handlePlugins(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.Contains(path, "..") {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if strings.HasPrefix(path, "/plugins/") {
		localPath := "." + path
		if _, err := os.Stat(localPath); err == nil {
			http.ServeFile(w, r, localPath)
			return
		}
		// Strip size suffixes like _40x40_m or _50x50 from image requests
		re := regexp.MustCompile(`_([0-9]+x[0-9]+)(_[a-z])?\.png$`)
		cleanPath := re.ReplaceAllString(localPath, ".png")
		if cleanPath != localPath {
			if _, err := os.Stat(cleanPath); err == nil {
				http.ServeFile(w, r, cleanPath)
				return
			}
		}
	}
	handleArtwork(w, r)
}

func cleanIconURL(icon string) string {
	if strings.HasPrefix(icon, "http://") || strings.HasPrefix(icon, "https://") {
		return "/imageproxy?u=" + url.QueryEscape(icon)
	}
	return icon
}

func handleImageProxy(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("u")
	if u == "" {
		handleArtwork(w, r)
		return
	}

	decodedURL, err := url.QueryUnescape(u)
	if err == nil {
		u = decodedURL
	}

	// Strip size suffixes like _40x40_m or _50x50 from remote image requests
	re := regexp.MustCompile(`_([0-9]+x[0-9]+)(_[a-z])?(\.[a-zA-Z0-9]+)`)
	u = re.ReplaceAllString(u, "$3")

	resp, err := http.Get(u)
	if err != nil {
		handleArtwork(w, r)
		return
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleDirectPCM generates PCM and feeds it directly into jive_alsa's
// shared memory FIFO, bypassing SqueezePlay entirely.
func handleDirectPCM(w http.ResponseWriter, r *http.Request) {
	// Kill SqueezePlay (jive binary) + jive_alsa so they don't interfere
	exec.Command("pkill", "-9", "jive").Run()
	exec.Command("pkill", "-9", "jive_alsa").Run()
	time.Sleep(500 * time.Millisecond)

	// Also destroy existing shared memory segment
	exec.Command("ipcrm", "-M", "56833").Run()

	path := "/home/uli/work/slim_slim_server/squeezeplay/src/squeezeplay/pcm_feed"
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		fmt.Printf("[DirectPCM] pcm_feed start error: %v\n", err)
		http.Error(w, fmt.Sprintf("pcm_feed error: %v", err), http.StatusInternalServerError)
		return
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			fmt.Printf("[DirectPCM] pcm_feed exited: %v\n", err)
		} else {
			fmt.Printf("[DirectPCM] pcm_feed done\n")
		}
	}()
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "direct PCM test started (10s 440Hz sine)\n")
}

// startHTTPServer starts the HTTP CometD server (for JSON-RPC and CometD
func startHTTPServer(m *ServerMetadata) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/play", handleStreamStart)
	mux.HandleFunc("/stream/pcmtest", handlePCMTest)
	mux.HandleFunc("/stream/direct", handleDirectPCM)
	mux.HandleFunc("/cometd", func(w http.ResponseWriter, r *http.Request) {
		handleCometd(w, r, m)
	})
	mux.HandleFunc("/jsonrpc", func(w http.ResponseWriter, r *http.Request) {
		handleJsonRpc(w, r, m)
	})
	mux.HandleFunc("/music/", handleArtwork)
	mux.HandleFunc("/contributor/", handleArtwork)
	mux.HandleFunc("/imageproxy", handleImageProxy)
	mux.HandleFunc("/imageproxy/", handleImageProxy)
	mux.HandleFunc("/html/", handleArtwork)
	mux.HandleFunc("/plugins/", handlePlugins)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// If it looks like an image/artwork request, serve placeholder
		if r.URL.Path != "/" && r.URL.Path != "/cometd" && r.URL.Path != "/jsonrpc" {
			handleArtwork(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	fmt.Printf("Starting HTTP server on port %s...\n", m.JSONPort)
	hl, err := listenTCP4(":"+m.JSONPort)
	if err != nil {
		fmt.Printf("HTTP server listen error: %v\n", err)
		os.Exit(1)
	}
	if err := http.Serve(hl, mux); err != nil {
		fmt.Printf("HTTP server error: %v\n", err)
		os.Exit(1)
	}
}

// handleCometd handles CometD protocol requests
func handleCometd(w http.ResponseWriter, r *http.Request, m *ServerMetadata) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var reqs []CometdRequest
	if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
		fmt.Printf("[CometD] failed to decode request: %v\n", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	fmt.Printf("[CometD] from %s\n", r.RemoteAddr)
	for i, req := range reqs {
		fmt.Printf("[CometD] <- req[%d] channel=%q clientId=%q\n", i, req.Channel, req.ClientId)
	}

	// Detect request type and assign/extract clientId
	isStreaming := false
	isHandshake := false
	clientId := ""
	for _, req := range reqs {
		switch req.Channel {
		case "/meta/handshake":
			isHandshake = true
		case "/meta/connect", "/meta/reconnect":
			isStreaming = true
			clientId = req.ClientId
		}
	}

	// If this is a connect streaming request with no clientId yet, generate one
	if isStreaming && clientId == "" {
		clientId = fmt.Sprintf("%08x", rand.Int31())
	}

	// Generate responses and detect subscribe/request channels for event scheduling
	resps := make([]CometdRequest, 0, len(reqs))
	for _, req := range reqs {
		resp := cometdResponse(req, m, clientId)
		resps = append(resps, resp)
	}

	for i, resp := range resps {
		fmt.Printf("[CometD] -> resp[%d] channel=%q clientId=%q\n", i, resp.Channel, resp.ClientId)
	}

	if isStreaming {
		// Register streaming client BEFORE starting the handler, so rhttp
		// handlers that push events via clientId can find it immediately.
		sc := registerStreamingClient(clientId)
		defer unregisterStreamingClient(clientId)
		handleCometdStreaming(w, clientId, resps, sc, m)
	} else {
		handleCometdNormal(w, resps, isHandshake)
	}
}

func handleCometdNormal(w http.ResponseWriter, resps []CometdRequest, isHandshake bool) {
	body, err := json.Marshal(resps)
	if err != nil {
		fmt.Printf("[CometD] marshal error: %v\n", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	fmt.Printf("[CometD] response body: %s\n", string(body))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func handleCometdStreaming(w http.ResponseWriter, clientId string, initialResps []CometdRequest, sc *StreamingClient, m *ServerMetadata) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fmt.Printf("[CometD] streaming not supported by ResponseWriter\n")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Mark clientId on responses
	for i := range initialResps {
		if initialResps[i].ClientId == "" {
			initialResps[i].ClientId = clientId
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	// Write initial response
	enc := json.NewEncoder(w)
	if err := enc.Encode(initialResps); err != nil {
		fmt.Printf("[CometD] streaming initial write error: %v\n", err)
		return
	}
	flusher.Flush()
	fmt.Printf("[CometD] streaming started for clientId=%s\n", clientId)

	// Push first serverstatus event immediately so the client gets initial player data
	go func() {
		time.Sleep(50 * time.Millisecond)
		players := getConnectedPlayers()
		pl := make([]map[string]interface{}, 0, len(players))
		for _, p := range players {
			pl = append(pl, map[string]interface{}{
				"playerid":  p.MAC,
				"name":      p.Name,
				"model":     p.Model,
				"connected": 1,
				"power":     1,
				"uuid":      p.UUID,
				"ip":        m.IPAddress,
			})
		}
		sc.events <- []interface{}{
			map[string]interface{}{
				"channel": "/" + clientId + "/slim/serverstatus",
				"data": map[string]interface{}{
					"version":          m.Version,
					"uuid":             m.UUID,
					"name":             m.Name,
					"ip":               m.IPAddress,
					"port":             m.JSONPort,
					"cliport":          m.CLIPort,
					"compatible":       true,
					"player count":     len(players),
					"players_loop":     pl,
					"canMusicFolder":   true,
					"canRandomPlay":    true,
					"canInternetRadio": true,
					"canRemoteStream":  true,
					"lang":             "EN",
				},
			},
		}
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	lifetimeLimit := time.After(180 * time.Second)

	for {
		select {
		case events, ok := <-sc.events:
			if !ok {
				fmt.Printf("[CometD] streaming channel closed for clientId=%s\n", clientId)
				return
			}
			if err := enc.Encode(events); err != nil {
				fmt.Printf("[CometD] streaming write error: %v\n", err)
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// Send an empty JSON array to keep the chunked HTTP connection alive
			if _, err := w.Write([]byte("[]\n")); err != nil {
				fmt.Printf("[CometD] streaming keepalive write error for clientId=%s: %v\n", clientId, err)
				return
			}
			flusher.Flush()
		case <-lifetimeLimit:
			fmt.Printf("[CometD] streaming connection lifetime limit reached for clientId=%s\n", clientId)
			return
		}
	}
}

func cometdResponse(req CometdRequest, m *ServerMetadata, assignedClientId string) CometdRequest {
	clid := req.ClientId
	if clid == "" {
		clid = assignedClientId
	}

	switch req.Channel {
	case "/meta/handshake":
		clid = fmt.Sprintf("%08x", rand.Int31())
		return CometdRequest{
			Channel:                  "/meta/handshake",
			Successful:               true,
			Id:                       req.Id,
			ClientId:                 clid,
			Version:                  "1.0",
			MinimumVersion:           "1.0",
			SupportedConnectionTypes: []string{"long-polling", "streaming"},
			Advice: map[string]interface{}{
				"reconnect": "retry",
				"interval":  0,
				"timeout":   60000,
			},
		}

	case "/meta/connect":
		interval := 5000
		return CometdRequest{
			Channel:    "/meta/connect",
			Successful: true,
			Id:         req.Id,
			ClientId:   clid,
			Advice: map[string]interface{}{
				"reconnect": "retry",
				"interval":  interval,
			},
		}

	case "/meta/reconnect":
		return CometdRequest{
			Channel:    "/meta/reconnect",
			Successful: true,
			Id:         req.Id,
			ClientId:   clid,
			Advice: map[string]interface{}{
				"reconnect": "retry",
				"interval":  5000,
			},
		}

	case "/meta/subscribe":
		sub := req.Subscription
		if sub == nil {
			sub = ""
		}
		return CometdRequest{
			Channel:      "/meta/subscribe",
			Successful:   true,
			Id:           req.Id,
			ClientId:     clid,
			Subscription: sub,
		}

	case "/meta/disconnect":
		return CometdRequest{
			Channel:    "/meta/disconnect",
			Successful: true,
			Id:         req.Id,
			ClientId:   clid,
		}

	case "/slim/subscribe":
		// Extract response clientId from data
		respClid := ""
		if req.Data != nil {
			if respPath, ok := req.Data["response"].(string); ok {
				if matches := regexp.MustCompile(`/([0-9a-f]{8})/`).FindStringSubmatch(respPath); len(matches) > 1 {
					respClid = matches[1]
				}
			}
		}
		if respClid == "" {
			respClid = clid
		}

		// Check if this is a serverstatus subscription and push event
		if req.Data != nil {
			if requestArr, ok := req.Data["request"].([]interface{}); ok && len(requestArr) >= 2 {
				if args, ok := requestArr[1].([]interface{}); ok && len(args) > 0 {
					if cmd, ok := args[0].(string); ok && cmd == "serverstatus" {
						done := make(chan struct{}, 1)
						pushServerStatus(respClid, req.Id, m, done)
						<-done
					}
				}
			}
		}

		return CometdRequest{
			Channel:    "/slim/subscribe",
			Successful: true,
			Id:         req.Id,
			ClientId:   clid,
		}

	case "/slim/request":
		respClid := ""
		respChannel := "/slim/request"
		if req.Data != nil {
			fmt.Printf("[CometD] /slim/request data: %v\n", req.Data)
			if respPath, ok := req.Data["response"].(string); ok {
				// Only use the response path as the channel when the request
				// has an id (i.e., a callback is registered). Requests without
				// an id (like artworkspec with func=nil) must respond on
				// /slim/request, which the client's "no action" handler
				// silently ignores. Responding on the response path without
				// an id triggers a "No id. event:" error in Comet.lua:943.
				if req.Id != nil {
					respChannel = respPath
				}
				if matches := regexp.MustCompile(`/([0-9a-f]{8})/`).FindStringSubmatch(respPath); len(matches) > 1 {
					respClid = matches[1]
				}
			}
		}
		if respClid == "" {
			respClid = clid
		}

		// Extract command from request.
		// Request format: [playerId, [command, args...]]
		reqPlayerId := ""
		reqCmdArr := []string{}
		if req.Data != nil {
			if requestArr, ok := req.Data["request"].([]interface{}); ok && len(requestArr) >= 2 {
				if p, ok := requestArr[0].(string); ok {
					reqPlayerId = p
				}
				if cmdArr, ok := requestArr[1].([]interface{}); ok && len(cmdArr) > 0 {
					if cmd, ok := cmdArr[0].(string); ok {
						isRadioCmd := false
						if cmd == "radios" {
							handleRadiosMenu(respClid, reqPlayerId, m)
							isRadioCmd = true
						} else {
							for _, sub := range defaultSubmenus {
								if sub["id"] == cmd {
									cmdReq := parseCLIRequest(cmdArr)
									if len(cmdReq.Args) > 0 && cmdReq.Args[0] == "playlist" {
										handleRadioPlay(respClid, reqPlayerId, cmdReq)
									} else {
										handleRadioSubmenu(respClid, reqPlayerId, cmdReq, m)
									}
									isRadioCmd = true
									break
								}
							}
						}

						if !isRadioCmd {
							switch cmd {
							case "menu":
								buildMenuResponse(respClid, reqPlayerId, m)
							case "status":
								buildStatusResponse(respClid, reqPlayerId, cmdArr, m)
							case "playlist":
								buildPlaylistResponse(respClid, reqPlayerId, cmdArr, m)
							case "artworkspec":
								// artworkspec commands are fire-and-forget; return empty
							case "button":
								if len(cmdArr) >= 2 {
									if btnName, ok := cmdArr[1].(string); ok {
										fmt.Printf("[Button] CometD button for player=%s: %s\n", reqPlayerId, btnName)
										go handleButtonPress(reqPlayerId, btnName)
									}
								}
							case "jivefavorites":
								go handleJiveFavorites(respClid, reqPlayerId, cmdArr)
							}
						}
					}
				}
			}
		}

		_ = reqCmdArr

		return CometdRequest{
			Channel:    respChannel,
			Successful: true,
			Id:         req.Id,
			ClientId:   respClid,
			Data:       buildSlimRequestResponse(respClid, reqPlayerId),
		}

	default:
		return CometdRequest{
			Channel:    req.Channel,
			Successful: true,
			Id:         req.Id,
			ClientId:   clid,
		}
	}
}

// buildMenuResponse generates a menu response and stores it for the streaming client.
var defaultSubmenus = []map[string]interface{}{
	{
		"id":   "presets",
		"text": "My Presets",
		"icon": "/plugins/TuneIn/html/images/radiopresets.png",
		"url":  "http://opml.radiotime.com/Browse.ashx?c=presets&partnerId=16",
	},
	{
		"id":   "local",
		"text": "Local Radio",
		"icon": "/plugins/TuneIn/html/images/radiolocal.png",
		"url":  "http://opml.radiotime.com/Browse.ashx?c=local&partnerId=16",
	},
	{
		"id":   "music",
		"text": "Music",
		"icon": "/plugins/TuneIn/html/images/radiomusic.png",
		"url":  "http://opml.radiotime.com/Browse.ashx?c=music&partnerId=16",
	},
	{
		"id":   "news",
		"text": "News & Talk",
		"icon": "/plugins/TuneIn/html/images/radionews.png",
		"url":  "http://opml.radiotime.com/Browse.ashx?id=c57922&partnerId=16",
	},
	{
		"id":   "sports",
		"text": "Sports",
		"icon": "/plugins/TuneIn/html/images/radiosports.png",
		"url":  "http://opml.radiotime.com/Browse.ashx?c=sports&partnerId=16",
	},
	{
		"id":   "talk",
		"text": "Talk",
		"icon": "/plugins/TuneIn/html/images/radiotalk.png",
		"url":  "http://opml.radiotime.com/Browse.ashx?c=talk&partnerId=16",
	},
	{
		"id":   "location",
		"text": "By Location",
		"icon": "/plugins/TuneIn/html/images/radioworld.png",
		"url":  "http://opml.radiotime.com/Browse.ashx?id=r0&partnerId=16",
	},
	{
		"id":   "language",
		"text": "By Language",
		"icon": "/plugins/TuneIn/html/images/radioworld.png",
		"url":  "http://opml.radiotime.com/Browse.ashx?c=lang&partnerId=16",
	},
	{
		"id":   "podcast",
		"text": "Podcasts",
		"icon": "/plugins/TuneIn/html/images/podcasts.png",
		"url":  "http://opml.radiotime.com/Browse.ashx?c=podcast&partnerId=16",
	},
	{
		"id":       "search",
		"text":     "Search TuneIn",
		"icon":     "/plugins/TuneIn/html/images/radiosearch.png",
		"isSearch": true,
	},
}

func handleRadiosMenu(respClid, playerId string, m *ServerMetadata) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		return
	}
	session := getClientSession(respClid)
	session.Reset()

	var items []map[string]interface{}

	opml, err := fetchOPML("http://opml.radiotime.com/Index.aspx?partnerId=16")
	if err == nil && len(opml.Items) > 0 {
		fmt.Printf("[Radio] Dynamic main menu fetched successfully\n")
		session.Mu.Lock()
		// My Presets first
		presetsID := fmt.Sprintf("item_%d", session.NextID)
		session.NextID++
		session.ItemURLs[presetsID] = "http://opml.radiotime.com/Browse.ashx?c=presets&partnerId=16"
		items = append(items, map[string]interface{}{
			"id":     presetsID,
			"text":   "My Presets",
			"weight": 5,
			"icon":   "/plugins/TuneIn/html/images/radiopresets.png",
			"actions": map[string]interface{}{
				"go": map[string]interface{}{
					"cmd": []interface{}{"presets", "items"},
					"params": map[string]interface{}{
						"feed_id": presetsID,
					},
				},
			},
		})

		for _, out := range opml.Items {
			tag := out.Key
			if tag == "" && out.Type == "search" {
				tag = "search"
			}
			if tag == "" {
				continue
			}

			itemID := fmt.Sprintf("item_%d", session.NextID)
			session.NextID++

			icon := "/plugins/TuneIn/html/images/radio.png"
			switch tag {
			case "local":
				icon = "/plugins/TuneIn/html/images/radiolocal.png"
			case "music":
				icon = "/plugins/TuneIn/html/images/radiomusic.png"
			case "news":
				icon = "/plugins/TuneIn/html/images/radionews.png"
			case "sports":
				icon = "/plugins/TuneIn/html/images/radiosports.png"
			case "talk":
				icon = "/plugins/TuneIn/html/images/radiotalk.png"
			case "location", "language", "world":
				icon = "/plugins/TuneIn/html/images/radioworld.png"
			case "podcast":
				icon = "/plugins/TuneIn/html/images/podcasts.png"
			case "search":
				icon = "/plugins/TuneIn/html/images/radiosearch.png"
			}

			if out.Type == "search" {
				items = append(items, map[string]interface{}{
					"id":   itemID,
					"text": out.Text,
					"type": "search",
					"icon": icon,
					"actions": map[string]interface{}{
						"go": map[string]interface{}{
							"cmd": []interface{}{tag, "items"},
							"params": map[string]interface{}{
								"search": "__TAGGEDINPUT__",
							},
						},
					},
					"input": map[string]interface{}{
						"len": 3,
						"help": map[string]interface{}{
							"text": "Search",
						},
					},
				})
			} else {
				session.ItemURLs[itemID] = out.URL
				items = append(items, map[string]interface{}{
					"id":     itemID,
					"text":   out.Text,
					"weight": len(items) * 10,
					"icon":   icon,
					"actions": map[string]interface{}{
						"go": map[string]interface{}{
							"cmd": []interface{}{tag, "items"},
							"params": map[string]interface{}{
								"feed_id": itemID,
							},
						},
					},
				})
			}
		}
		session.Mu.Unlock()
	} else {
		fmt.Printf("[Radio] Falling back to default static main menu due to error: %v\n", err)
		session.Mu.Lock()
		for _, sub := range defaultSubmenus {
			itemID := fmt.Sprintf("item_%d", session.NextID)
			session.NextID++

			isSearch, _ := sub["isSearch"].(bool)
			tag := sub["id"].(string)

			if isSearch {
				items = append(items, map[string]interface{}{
					"id":   itemID,
					"text": sub["text"],
					"type": "search",
					"icon": sub["icon"],
					"actions": map[string]interface{}{
						"go": map[string]interface{}{
							"cmd": []interface{}{tag, "items"},
							"params": map[string]interface{}{
								"search": "__TAGGEDINPUT__",
							},
						},
					},
					"input": map[string]interface{}{
						"len": 3,
						"help": map[string]interface{}{
							"text": "Search",
						},
					},
				})
			} else {
				urlVal, _ := sub["url"].(string)
				session.ItemURLs[itemID] = urlVal
				items = append(items, map[string]interface{}{
					"id":     itemID,
					"text":   sub["text"],
					"weight": len(items) * 10,
					"icon":   sub["icon"],
					"actions": map[string]interface{}{
						"go": map[string]interface{}{
							"cmd": []interface{}{tag, "items"},
							"params": map[string]interface{}{
								"feed_id": itemID,
							},
						},
					},
				})
			}
		}
		session.Mu.Unlock()
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

func handleRadioSubmenu(respClid, playerId string, cmdReq CLIRequest, m *ServerMetadata) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		return
	}
	session := getClientSession(respClid)

	var feedURL string
	var feedItems []Outline
	hasCachedFeed := false

	session.Mu.Lock()
	feedID := cmdReq.Params["feed_id"]
	if feedID != "" {
		if cachedFeed, ok := session.CachedFeeds[feedID]; ok {
			feedItems = cachedFeed.Items
			hasCachedFeed = true
		} else {
			feedURL = session.ItemURLs[feedID]
		}
	}

	if feedURL == "" && !hasCachedFeed {
		feedURL = cmdReq.Params["url"]
	}

	searchTerm := cmdReq.Params["search"]
	if searchTerm != "" {
		feedURL = "http://opml.radiotime.com/Search.ashx?query=" + url.QueryEscape(searchTerm) + "&partnerId=16"
	}

	if feedURL == "" && !hasCachedFeed {
		for _, sub := range defaultSubmenus {
			if sub["id"] == cmdReq.Command {
				urlVal, _ := sub["url"].(string)
				feedURL = urlVal
				break
			}
		}
	}
	session.Mu.Unlock()

	if !hasCachedFeed && feedURL != "" {
		fmt.Printf("[Radio] Fetching OPML feed URL: %s\n", feedURL)
		opml, err := fetchOPML(feedURL)
		if err != nil {
			fmt.Printf("[Radio] Error fetching subfeed: %v\n", err)
			return
		}
		feedItems = opml.Items
	}

	var items []map[string]interface{}
	session.Mu.Lock()

	for _, out := range feedItems {
		itemID := fmt.Sprintf("item_%d", session.NextID)
		session.NextID++

		if out.Type == "audio" {
			session.ItemURLs[itemID] = out.URL

			subtext := out.Subtext
			if subtext == "" {
				subtext = out.Playing
			}
			if subtext == "" && out.Bitrate != "" {
				subtext = fmt.Sprintf("%skbps", out.Bitrate)
			}

			session.ItemMetadata[itemID] = PlaybackItem{
				Title:   out.Text,
				Subtext: subtext,
				Icon:    cleanIconURL(out.Image),
				URL:     out.URL,
			}

			items = append(items, map[string]interface{}{
				"id":      itemID,
				"text":    out.Text,
				"subtext": subtext,
				"type":    "audio",
				"icon":    cleanIconURL(out.Image),
				"actions": map[string]interface{}{
					"go": map[string]interface{}{
						"cmd": []interface{}{cmdReq.Command, "playlist", "play"},
						"params": map[string]interface{}{
							"item_id": itemID,
						},
					},
				},
			})
		} else if out.Type == "link" || len(out.Outlines) > 0 {
			if len(out.Outlines) > 0 {
				session.CachedFeeds[itemID] = MenuFeed{
					Title: out.Text,
					Items: out.Outlines,
				}
			} else {
				session.ItemURLs[itemID] = out.URL
			}

			items = append(items, map[string]interface{}{
				"id":   itemID,
				"text": out.Text,
				"icon": cleanIconURL(out.Image),
				"actions": map[string]interface{}{
					"go": map[string]interface{}{
						"cmd": []interface{}{cmdReq.Command, "items"},
						"params": map[string]interface{}{
							"feed_id": itemID,
						},
					},
				},
			})
		} else if out.Type == "search" {
			items = append(items, map[string]interface{}{
				"id":   itemID,
				"text": out.Text,
				"type": "search",
				"icon": cleanIconURL(out.Image),
				"actions": map[string]interface{}{
					"go": map[string]interface{}{
						"cmd": []interface{}{cmdReq.Command, "items"},
						"params": map[string]interface{}{
							"search": "__TAGGEDINPUT__",
						},
					},
				},
				"input": map[string]interface{}{
					"len": 3,
					"help": map[string]interface{}{
						"text": "Search",
					},
				},
			})
		}
	}
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

func handleRadioPlay(respClid, playerId string, cmdReq CLIRequest) {
	session := getClientSession(respClid)
	session.Mu.Lock()
	itemID := cmdReq.Params["item_id"]
	streamURL := session.ItemURLs[itemID]
	meta, hasMeta := session.ItemMetadata[itemID]
	session.Mu.Unlock()

	if streamURL == "" {
		fmt.Printf("[Radio] Play error: item %s not found in cache\n", itemID)
		return
	}

	pb := getPlayerPlayback(playerId)
	pb.Mu.Lock()
	if hasMeta {
		pb.Playing = true
		pb.PlayingTitle = meta.Title
		pb.PlayingDesc = meta.Subtext
		pb.PlayingIcon = meta.Icon
		pb.PlayingURL = meta.URL
	}
	pb.Mu.Unlock()

	slimConnMu.Lock()
	conn := slimConn
	slimConnMu.Unlock()

	if conn == nil {
		fmt.Printf("[Radio] Play error: no SlimProto connection\n")
		return
	}

	fmt.Printf("[Radio] Tuning to station ID=%s, URL=%s\n", itemID, streamURL)
	go runURLStream(conn, streamURL, pb)

	sc := getStreamingClient(respClid)
	if sc != nil {
		data := map[string]interface{}{
			"count":     0,
			"offset":    0,
			"item_loop": []interface{}{},
		}
		sc.pendingMu.Lock()
		sc.pendingResponse = data
		sc.pendingMu.Unlock()
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		publishPlayerStatus()
	}()
}

func buildMenuResponse(respClid, playerId string, m *ServerMetadata) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		fmt.Printf("[Menu] no streaming client found for %s\n", respClid)
		return
	}

	data := map[string]interface{}{
		"item_loop": []interface{}{
			map[string]interface{}{
				"id":        "nowPlaying",
				"text":      "Now Playing",
				"node":      "home",
				"iconStyle": "hm_appletNowPlaying",
				"actions": map[string]interface{}{
					"go": map[string]interface{}{
						"cmd": []interface{}{"applet", "NowPlaying", "play"},
					},
				},
				"window": map[string]interface{}{
					"windowId": "nowPlaying",
				},
			},
			map[string]interface{}{
				"id":     "opmlradio",
				"text":   "Internet Radio",
				"node":   "home",
				"icon":   "/plugins/TuneIn/html/images/radio.png",
				"weight": 25,
				"actions": map[string]interface{}{
					"go": map[string]interface{}{
						"cmd": []interface{}{"radios", 0, 100},
						"params": map[string]interface{}{
							"menu": "radios",
						},
					},
				},
			},
		},
	}

	sc.pendingMu.Lock()
	sc.pendingResponse = data
	sc.pendingMu.Unlock()
	fmt.Printf("[Menu] prepared menu response for player=%s clientId=%s\n", playerId, respClid)
}

func publishPlayerStatus() {
	m := globalServerMetadata
	if m == nil {
		return
	}

	streamingClientsMu.Lock()
	defer streamingClientsMu.Unlock()

	for clid, sc := range streamingClients {
		players := getConnectedPlayers()
		for _, p := range players {
			session := getClientSession(clid)
			session.Mu.Lock()
			playing := session.Playing
			title := session.PlayingTitle
			desc := session.PlayingDesc
			icon := session.PlayingIcon
			urlStr := session.PlayingURL
			session.Mu.Unlock()

			mode := "stop"
			count := 0
			itemLoop := []interface{}{}
			remote := 0
			currentTitle := ""
			if playing {
				mode = "play"
				count = 1
				remote = 1
				currentTitle = title
				itemLoop = []interface{}{
					map[string]interface{}{
						"id":          0,
						"title":       title,
						"track":       title,
						"artist":      desc,
						"album":       desc,
						"text":        title,
						"artwork_url": icon,
						"url":         urlStr,
						"duration":    0,
					},
				}
			}

			statusData := map[string]interface{}{
				"player_name":        p.Name,
				"player_ip":          m.IPAddress,
				"mode":               mode,
				"power":              1,
				"time":               0,
				"rate":               1,
				"playlist_timestamp": time.Now().Unix(),
				"playlist_cur_index": 0,
				"playlist_tracks":    count,
				"count":              count,
				"item_loop":          itemLoop,
				"remote":             remote,
				"current_title":      currentTitle,
			}

			select {
			case sc.events <- []interface{}{
				map[string]interface{}{
					"channel": "/" + clid + "/slim/playerstatus",
					"data":    statusData,
				},
				map[string]interface{}{
					"channel": "/slim/playerstatus/" + p.MAC,
					"data":    statusData,
				},
			}:
				fmt.Printf("[CometD] Published playerstatus for client=%s player=%s (mode=%s)\n", clid, p.MAC, mode)
			default:
				// Avoid blocking
			}
		}
	}
}

// buildStatusResponse generates a player status response and stores it for the streaming client.
func buildStatusResponse(respClid, playerId string, cmdArr []interface{}, m *ServerMetadata) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		fmt.Printf("[Status] no streaming client found for %s\n", respClid)
		return
	}

	players := getConnectedPlayers()
	playerName := ""
	for _, p := range players {
		if p.MAC == playerId {
			playerName = p.Name
			break
		}
	}
	if playerName == "" {
		playerName = playerId
	}

	session := getClientSession(respClid)
	session.Mu.Lock()
	playing := session.Playing
	title := session.PlayingTitle
	desc := session.PlayingDesc
	icon := session.PlayingIcon
	urlStr := session.PlayingURL
	session.Mu.Unlock()

	mode := "stop"
	count := 0
	itemLoop := []interface{}{}
	remote := 0
	currentTitle := ""
	if playing {
		mode = "play"
		count = 1
		remote = 1
		currentTitle = title
		itemLoop = []interface{}{
			map[string]interface{}{
				"id":          0,
				"title":       title,
				"track":       title,
				"artist":      desc,
				"album":       desc,
				"text":        title,
				"artwork_url": icon,
				"url":         urlStr,
				"duration":    0,
			},
		}
	}

	data := map[string]interface{}{
		"player_name":        playerName,
		"player_ip":          m.IPAddress,
		"mode":               mode,
		"power":              1,
		"time":               0,
		"rate":               1,
		"playlist_timestamp": time.Now().Unix(),
		"playlist_cur_index": 0,
		"playlist_tracks":    count,
		"count":              count,
		"item_loop":          itemLoop,
		"remote":             remote,
		"current_title":      currentTitle,
	}

	sc.pendingMu.Lock()
	sc.pendingResponse = data
	sc.pendingMu.Unlock()
	fmt.Printf("[Status] prepared status response for player=%s clientId=%s (mode=%s)\n", playerId, respClid, mode)
}

func buildPlaylistResponse(respClid, playerId string, cmdArr []interface{}, m *ServerMetadata) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		fmt.Printf("[Playlist] no streaming client found for %s\n", respClid)
		return
	}

	session := getClientSession(respClid)
	session.Mu.Lock()
	playing := session.Playing
	title := session.PlayingTitle
	desc := session.PlayingDesc
	icon := session.PlayingIcon
	urlStr := session.PlayingURL
	session.Mu.Unlock()

	var playlistLoop []interface{}
	if playing {
		playlistLoop = []interface{}{
			map[string]interface{}{
				"id":          0,
				"title":       title,
				"track":       title,
				"artist":      desc,
				"album":       desc,
				"text":        title,
				"artwork_url": icon,
				"url":         urlStr,
				"duration":    0,
			},
		}
	}

	data := map[string]interface{}{
		"playlist_loop": playlistLoop,
		"count":         len(playlistLoop),
	}

	sc.pendingMu.Lock()
	sc.pendingResponse = data
	sc.pendingMu.Unlock()
	fmt.Printf("[Playlist] prepared playlist response for player=%s clientId=%s (count=%d)\n", playerId, respClid, len(playlistLoop))
}

// buildSlimRequestResponse returns the appropriate CometD response data for a
// /slim/request command. It returns cached data if buildMenuResponse or
// buildStatusResponse have prepared it, or a default empty response otherwise.
func buildSlimRequestResponse(respClid, playerId string) map[string]interface{} {
	sc := getStreamingClient(respClid)
	if sc == nil {
		return map[string]interface{}{"response": []interface{}{}}
	}

	sc.pendingMu.Lock()
	data := sc.pendingResponse
	sc.pendingResponse = nil
	sc.pendingMu.Unlock()

	if data != nil {
		return data
	}
	return map[string]interface{}{"response": []interface{}{}}
}

func pushServerStatus(respClid string, id interface{}, m *ServerMetadata, done chan struct{}) {
	// Retry a few times to find the streaming client (race: subscribe
	// can arrive before connect establishes the streaming session).
	var sc *StreamingClient
	for i := 0; i < 10; i++ {
		sc = getStreamingClient(respClid)
		if sc != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sc == nil {
		fmt.Printf("[CometD] no streaming client found for %s\n", respClid)
		if done != nil {
			done <- struct{}{}
		}
		return
	}

	go func() {
		time.Sleep(50 * time.Millisecond)

		players := getConnectedPlayers()
		pl := make([]map[string]interface{}, 0, len(players))
		for _, p := range players {
			pl = append(pl, map[string]interface{}{
				"playerid":  p.MAC,
				"name":      p.Name,
				"model":     p.Model,
				"connected": 1,
				"power":     1,
				"uuid":      p.UUID,
				"ip":        m.IPAddress,
			})
		}

		sc.events <- []interface{}{
			map[string]interface{}{
				"channel": "/" + respClid + "/slim/serverstatus",
				"id":      id,
				"data": map[string]interface{}{
					"version":          m.Version,
					"uuid":             m.UUID,
					"name":             m.Name,
					"ip":               m.IPAddress,
					"port":             m.JSONPort,
					"cliport":          m.CLIPort,
					"compatible":       true,
					"player count":     len(players),
					"players_loop":     pl,
					"canMusicFolder":   true,
					"canRandomPlay":    true,
					"canInternetRadio": true,
					"canRemoteStream":  true,
					"lang":             "EN",
				},
			},
		}
		if done != nil {
			done <- struct{}{}
		}
	}()
}

// handleJsonRpc handles JSON-RPC protocol requests
func handleJsonRpc(w http.ResponseWriter, r *http.Request, m *ServerMetadata) {
	var req JsonRpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	fmt.Printf("[JSON-RPC] method=%s params=%v\n", req.Method, req.Params)

	switch req.Method {
	case "slim.subscribe":
		// params[0] is the channel path like "/{clientId}/slim/serverstatus"
		if len(req.Params) > 0 {
			if channel, ok := req.Params[0].(string); ok {
				if matches := regexp.MustCompile(`/([0-9a-f]{8})/`).FindStringSubmatch(channel); len(matches) > 1 {
					clientId := matches[1]
					// Check if it's a serverstatus subscription
					if strings.Contains(channel, "serverstatus") {
						pushServerStatus(clientId, req.Id, m, nil)
					}
				}
			}
		}
	}

	resp := JsonRpcResponse{Id: req.Id, Result: make(map[string]interface{})}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// getLocalIPv4 returns the first non-loopback IPv4 address on the system
func getLocalIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		panic(err)
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	panic(fmt.Errorf("no non-loopback IPv4 address found"))
}

func main() {
	initPresets()
	nameFlag := flag.String("name", "GoSlimServer", "Friendly name of the server")
	uuidFlag := flag.String("uuid", "goslimserver-uuid-1234", "UUID of the server")
	jsonPortFlag := flag.String("json", "9000", "CometD HTTP port")
	cliPortFlag := flag.String("cli", "9090", "CLI port")
	versionFlag := flag.String("version", "9.0", "Server version string")
	flag.Parse()

	name := *nameFlag
	uuid := *uuidFlag
	jsonPort := *jsonPortFlag
	cliPort := *cliPortFlag
	version := *versionFlag

	// Detect local IP address for discovery
	ipAddress := getLocalIPv4()

	metadata := &ServerMetadata{
		IPAddress: ipAddress,
		Name:      name,
		UUID:      uuid,
		JSONPort:  jsonPort,
		CLIPort:   cliPort,
		Version:   version,
	}
	globalServerMetadata = metadata

	fmt.Println("==================================================")
	fmt.Println("            SlimProto Server (Go)")
	fmt.Println("==================================================")
	fmt.Printf("  Name    : %s\n", metadata.Name)
	fmt.Printf("  IP      : %s\n", metadata.IPAddress)
	fmt.Printf("  UUID    : %s\n", metadata.UUID)
	fmt.Printf("  JSON    : %s\n", metadata.JSONPort)
	fmt.Printf("  Version : %s\n", metadata.Version)
	fmt.Println("--------------------------------------------------")

	// Start HTTP CometD server (required for client discovery)
	go startHTTPServer(metadata)

	// Start TCP SlimProto control server (port 3483)
	go startTCPServer(metadata)

	// Setup UDP discovery for network discovery (localhost only)
	localAddr := fmt.Sprintf("127.0.0.1:%d", DefaultPort)
	broadcastConn, err := listenUDP4(DefaultPort)
	if err != nil {
		fmt.Printf("Warning: UDP discovery listener failed: %v\n", err)
	} else {
		defer broadcastConn.Close()
		fmt.Printf("Listening for UDP discovery on %s...\n", localAddr)
		go startUDPListener(broadcastConn, broadcastConn, metadata, "Broadcast")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nShutting down. Goodbye!")
}

// listenTCP4 creates a net.Listener using raw syscalls to bypass GO's default SOCK_CLOEXEC / SOCK_NONBLOCK flags inside socket().
// This is required to support older Linux kernels like the Squeezebox Radio's 2.6.26-rt kernel, which throws EINVAL (invalid argument) socket errors otherwise.
func listenTCP4(addr string) (net.Listener, error) {
	portStr := addr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		portStr = addr[idx+1:]
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		port = 0
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}

	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(fd)
		return nil, err
	}

	saddr := &syscall.SockaddrInet4{
		Port: port,
		Addr: [4]byte{127, 0, 0, 1},
	}
	if err := syscall.Bind(fd, saddr); err != nil {
		syscall.Close(fd)
		return nil, err
	}

	if err := syscall.Listen(fd, 128); err != nil {
		syscall.Close(fd)
		return nil, err
	}

	file := os.NewFile(uintptr(fd), "listener")
	defer file.Close()

	return net.FileListener(file)
}

// listenUDP4 creates a *net.UDPConn using raw syscalls to bypass Go's default SOCK_CLOEXEC / SOCK_NONBLOCK flags.
// This is required to support legacy Linux kernels (like 2.6.26) on the Squeezebox Radio.
func listenUDP4(port int) (*net.UDPConn, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, err
	}

	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(fd)
		return nil, err
	}

	saddr := &syscall.SockaddrInet4{
		Port: port,
		Addr: [4]byte{127, 0, 0, 1},
	}
	if err := syscall.Bind(fd, saddr); err != nil {
		syscall.Close(fd)
		return nil, err
	}

	file := os.NewFile(uintptr(fd), "udp-listener")
	defer file.Close()

	conn, err := net.FilePacketConn(file)
	if err != nil {
		return nil, err
	}

	udpConn, ok := conn.(*net.UDPConn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("not a UDP connection")
	}

	return udpConn, nil
}

// listenMulticastUDP4 creates a multicast *net.UDPConn and joins the specified multicast group using raw syscalls.
func listenMulticastUDP4(mcastGroup string, port int) (*net.UDPConn, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, err
	}

	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(fd)
		return nil, err
	}

	saddr := &syscall.SockaddrInet4{
		Port: port,
		Addr: [4]byte{0, 0, 0, 0},
	}
	if err := syscall.Bind(fd, saddr); err != nil {
		syscall.Close(fd)
		return nil, err
	}

	ip := net.ParseIP(mcastGroup).To4()
	if ip == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("invalid multicast group: %s", mcastGroup)
	}

	mreq := &syscall.IPMreq{
		Multiaddr: [4]byte{ip[0], ip[1], ip[2], ip[3]},
		Interface: [4]byte{0, 0, 0, 0},
	}
	if err := syscall.SetsockoptIPMreq(fd, syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, mreq); err != nil {
		syscall.Close(fd)
		return nil, err
	}

	file := os.NewFile(uintptr(fd), "udp-mcast-listener")
	defer file.Close()

	conn, err := net.FilePacketConn(file)
	if err != nil {
		return nil, err
	}

	udpConn, ok := conn.(*net.UDPConn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("not a UDP connection")
	}

	return udpConn, nil
}
