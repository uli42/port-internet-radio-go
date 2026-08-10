package main

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
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
func isAllHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return len(s) > 0
}

func extractClientID(path string) string {
	for i := 0; i < len(path)-10; i++ {
		if path[i] == '/' {
			segment := path[i+1 : i+9]
			if isAllHex(segment) && path[i+9] == '/' {
				return segment
			}
		}
	}
	return ""
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

func isLetters(s string) bool {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return len(s) > 0
}

func stripImageSizeSuffix(u string) string {
	dotIdx := strings.LastIndex(u, ".")
	if dotIdx == -1 {
		return u
	}
	ext := u[dotIdx:]
	prefix := u[:dotIdx]
	idx := strings.LastIndex(prefix, "_")
	if idx == -1 {
		return u
	}
	after := prefix[idx+1:]
	xPos := strings.Index(after, "x")
	if xPos <= 0 {
		return u
	}
	w := after[:xPos]
	if !isDigits(w) {
		return u
	}
	rest := after[xPos+1:]
	if rest == "" {
		return u
	}
	if isDigits(rest) {
		return prefix[:idx] + ext
	}
	if underscoreIdx := strings.Index(rest, "_"); underscoreIdx != -1 {
		if isDigits(rest[:underscoreIdx]) && len(rest) == underscoreIdx+2 && isLetters(rest[underscoreIdx+1:]) {
			return prefix[:idx] + ext
		}
	}
	return u
}

type OPML struct {
	Version string
	Title   string
	Items   []Outline
}

type Outline struct {
	Type        string
	Text        string
	URL         string
	Key         string
	Bitrate     string
	Reliability string
	Subtext     string
	GenreID     string
	Formats     string
	Item        string
	Image       string
	PresetID    string
	Playing     string
	PlayingImg  string
	Outlines    []Outline
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
	client := newHTTPClient(10*time.Second, 0)
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

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	opml := &OPML{
		Items: parseOPMLOutlines(string(body)),
	}
	return opml, nil
}

func parseOPMLOutlines(data string) []Outline {
	var items []Outline
	i := 0
	for i < len(data) {
		idx := strings.Index(data[i:], "<outline")
		if idx == -1 {
			break
		}
		i += idx
		endIdx := strings.Index(data[i:], "/>")
		if endIdx == -1 {
			break
		}
		tagContent := data[i+8 : i+endIdx]
		items = append(items, parseOutlineAttrs(tagContent))
		i += endIdx + 2
	}
	return items
}

func parseOutlineAttrs(tagContent string) Outline {
	outline := Outline{}
	for len(tagContent) > 0 {
		spaceIdx := strings.Index(tagContent, " ")
		if spaceIdx == -1 {
			break
		}
		attrName := tagContent[:spaceIdx]
		tagContent = tagContent[spaceIdx+1:]

		eqIdx := strings.Index(tagContent, "=\"")
		if eqIdx == -1 {
			break
		}
		var attrValue string
		rest := tagContent[eqIdx+2:]
		endQuote := strings.Index(rest, "\"")
		if endQuote != -1 {
			attrValue = rest[:endQuote]
		}

		switch attrName {
		case "type":
			outline.Type = attrValue
		case "text":
			outline.Text = attrValue
		case "URL":
			outline.URL = attrValue
		case "key":
			outline.Key = attrValue
		case "bitrate":
			outline.Bitrate = attrValue
		case "reliability":
			outline.Reliability = attrValue
		case "subtext":
			outline.Subtext = attrValue
		case "genre_id":
			outline.GenreID = attrValue
		case "formats":
			outline.Formats = attrValue
		case "item":
			outline.Item = attrValue
		case "image":
			outline.Image = attrValue
		case "preset_id":
			outline.PresetID = attrValue
		case "playing":
			outline.Playing = attrValue
		case "playing_image":
			outline.PlayingImg = attrValue
		}
	}
	return outline
}

// resolveStreamURL resolves TuneIn Tune.ashx, PLS, and M3U playlists to find a direct stream URL
func resolveStreamURL(streamURL string) string {
	for depth := 0; depth < 5; depth++ {
		lowerURL := strings.ToLower(streamURL)
		isTuneIn := strings.Contains(lowerURL, "tune.ashx") || strings.Contains(lowerURL, "tunein.com") || strings.Contains(lowerURL, "radiotime.com")
		
		parsed, err := url.Parse(streamURL)
		var path string
		if err == nil {
			path = strings.ToLower(parsed.Path)
		} else {
			path = lowerURL
		}
		isPlaylist := strings.HasSuffix(path, ".m3u") || strings.HasSuffix(path, ".m3u8") || strings.HasSuffix(path, ".pls") || strings.Contains(path, "/playlist/")

		if isTuneIn || isPlaylist {
			resolved := fetchPlaylistOrTuneIn(streamURL)
			if resolved != "" && resolved != streamURL {
				LogInfo("Resolver", "Resolved %s -> %s", streamURL, resolved)
				streamURL = resolved
				continue
			}
		}
		break
	}
	return streamURL
}

func newHTTPClient(timeout time.Duration, dialTimeout time.Duration) *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	if dialTimeout > 0 {
		tr.DialContext = (&net.Dialer{
			Timeout: dialTimeout,
		}).DialContext
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}
}

func fetchPlaylistOrTuneIn(urlStr string) string {
	client := newHTTPClient(5*time.Second, 0)
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
		   	LogInfo("general", "Removing unwanted url: %s", u)
			continue
		}
		LogInfo("general", "Adding wanted url: %s", u)
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

func parseCLIRequest(cmdArr []interface{}, paramsObj map[string]interface{}) CLIRequest {
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

	for k, v := range paramsObj {
		if strVal, ok := v.(string); ok {
			req.Params[k] = strVal
		} else {
			req.Params[k] = fmt.Sprintf("%v", v)
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
	Error                    string                 `json:"error,omitempty"`
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
	Paused       bool
	PlayingTitle string
	PlayingDesc  string
	PlayingIcon  string
	PlayingURL   string
	StationName  string
	StationDesc  string
	StreamURL    string
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

func pausePlayer(playerId string) {
	playerId = normalizePlayerId(playerId)
	pb := getPlayerPlayback(playerId)
	pb.Mu.Lock()
	pb.Playing = false
	pb.Paused = true
	pb.Mu.Unlock()
	publishPlayerStatus()
}

func resumePlayer(playerId string, conn net.Conn) {
	playerId = normalizePlayerId(playerId)
	pb := getPlayerPlayback(playerId)
	pb.Mu.Lock()
	pb.Playing = true
	pb.Paused = false
	streamURL := pb.StreamURL
	pb.Mu.Unlock()

	if streamURL != "" && conn != nil {
		LogInfo("Stream", "resume: restarting stream for player %s: %s", playerId, streamURL)
		go runURLStream(conn, streamURL, pb, playerId)
	} else if conn != nil && streamDataPort != 0 {
		if err := sendStrmStop(conn); err != nil {
			LogError("Stream", "resume sendStrmStop error: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		if err := sendAude(conn, true); err != nil {
			LogError("Stream", "resume sendAude error: %v", err)
		}
		if err := sendStrmCommand(conn, 's', streamDataPort, 'm', 0, 0, 0, 0); err != nil {
			LogError("Stream", "resume sendStrmCommand error: %v", err)
		} else {
			LogInfo("Stream", "resumed player %s with strm on port %d", playerId, streamDataPort)
		}
	}

	publishPlayerStatus()
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

func normalizePlayerId(playerId string) string {
	if len(playerId) == 17 && strings.Contains(playerId, ":") {
		return strings.ToLower(playerId)
	}

	connectedPlayersMu.RLock()
	defer connectedPlayersMu.RUnlock()
	for mac := range connectedPlayers {
		return mac
	}

	return strings.ToLower(playerId)
}

func getPresetsFilePath() string {
	if info, err := os.Stat("/etc/squeezeplay/userpath/settings"); err == nil && info.IsDir() {
		testPath := "/etc/squeezeplay/userpath/settings/.presets_test"
		if err := ioutil.WriteFile(testPath, []byte("test"), 0644); err == nil {
			_ = os.Remove(testPath)
			return "/etc/squeezeplay/userpath/settings/presets.json"
		}
	}
	if info, err := os.Stat("/root"); err == nil && info.IsDir() {
		testPath := "/root/.presets_test"
		if err := ioutil.WriteFile(testPath, []byte("test"), 0644); err == nil {
			_ = os.Remove(testPath)
			return "/root/presets.json"
		}
	}
	return "presets.json"
}

func savePresets() {
	presetsMu.Lock()
	defer presetsMu.Unlock()

	data, err := json.MarshalIndent(presets, "", "  ")
	if err != nil {
		LogError("Presets", "Error marshaling presets: %v", err)
		return
	}

	path := getPresetsFilePath()
	err = ioutil.WriteFile(path, data, 0644)
	if err != nil {
		LogError("Presets", "Error saving presets to file (%s): %v", path, err)
	} else {
		LogInfo("Presets", "Presets saved to %s", path)
	}
}

func initPresets() {
	presetsMu.Lock()
	defer presetsMu.Unlock()

	path := getPresetsFilePath()
	data, err := ioutil.ReadFile(path)
	if err == nil {
		var loaded map[int]PresetItem
		if err := json.Unmarshal(data, &loaded); err == nil {
			presets = loaded
			LogInfo("Presets", "Loaded %d presets from %s", len(presets), path)
			return
		}
	}

	// Fallback to default
	presets[1] = PresetItem{
		Title:   "SWR3",
		Subtext: "Pop music station",
		Icon:    "plugins/TuneIn/html/images/radiolocal.png",
		URL:     "https://liveradio.swr.de/sw282p3/swr3/",
	}

	// Save the default preset
	data, _ = json.MarshalIndent(presets, "", "  ")
	_ = ioutil.WriteFile(path, data, 0644)
	LogInfo("Presets", "Created default presets file at %s", path)
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
	sc, ok := streamingClients[clientId]
	if !ok && clientId != "" {
		sc = &StreamingClient{
			events:   make(chan []interface{}, 100),
			clientId: clientId,
		}
		streamingClients[clientId] = sc
		LogInfo("Client", "Auto-registered streaming client %s on the fly", clientId)
	}
	return sc
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

// defaultStreamThreshold returns the default buffer threshold in milliseconds
// for the given stream mode. AAC/OGG/FLAC need more buffer for decoder init.
func defaultStreamThreshold(mode byte) byte {
	switch mode {
	case 'a', 'o', 'f':
		return 200
	case 'p':
		return 50
	default:
		return 1
	}
}

// sendStrmCommand sends a strm packet telling the player to start/resume streaming
// from a specific port. The player connects to serverIp:serverPort and receives
// audio data in the format specified by mode (e.g. 'm' = MP3/mad, 'p' = PCM).
// command should be 's' for start or 'r' for resume.
func sendStrmCommand(conn net.Conn, command byte, serverPort uint16, mode byte, pcmSampleSize, pcmSampleRate, pcmChannels, pcmEndianness byte, threshold ...byte) error {
	thresh := byte(1)
	if len(threshold) > 0 {
		thresh = threshold[0]
	}
	payload := make([]byte, 28)
	payload[0] = command
	payload[1] = '1'                     // autostart (ASCII '1')
	payload[2] = mode
	payload[3] = pcmSampleSize
	payload[4] = pcmSampleRate
	payload[5] = pcmChannels
	payload[6] = pcmEndianness
	payload[7] = thresh                   // threshold (ms of audio to buffer before starting)
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

// sendAudg sends an audio gain packet matching LMS's full 22-byte format:
//   oldGainL (N), oldGainR (N), dvc (C), preamp (C), newGainL (N), newGainR (N), sequenceNumber (N).
// SqueezePlay requires this full packet to activate the audio output path.
func sendAudg(conn net.Conn, volume int) error {
	clientVolumeMu.Lock()
	v := clientVolume
	if volume >= 0 {
		v = volume
	}
	clientVolumeMu.Unlock()

	var newGain uint32
	if v <= 0 {
		newGain = 0
	} else {
		newGain = 65536 // 0 dB in Q16 fixed-point
	}

	payload := make([]byte, 22)
	binary.BigEndian.PutUint32(payload[0:4], 0)        // oldGain left
	binary.BigEndian.PutUint32(payload[4:8], 0)        // oldGain right
	payload[8] = 1                                      // dvc (digital volume control)
	payload[9] = 255                                    // preamp
	binary.BigEndian.PutUint32(payload[10:14], newGain) // newGain left
	binary.BigEndian.PutUint32(payload[14:18], newGain) // newGain right
	binary.BigEndian.PutUint32(payload[18:22], 0)       // sequenceNumber
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

func isFfmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

type ffmpegAacReader struct {
	upstream  io.Reader
	ffmpegCmd *exec.Cmd
	ffmpegOut io.Reader
	ffmpegIn  io.WriteCloser
}

func newFfmpegAacReader(upstream io.Reader, metaInt int, onMetadata func(string)) (*ffmpegAacReader, error) {
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "aac", "-i", "pipe:0", "-f", "adts", "pipe:1")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}

	icyR := &IcyReader{
		reader:      upstream,
		metaInt:     metaInt,
		bytesToNext: metaInt,
		onMetadata:  onMetadata,
	}

	go func() {
		defer stdin.Close()
		io.Copy(stdin, icyR)
	}()

	return &ffmpegAacReader{
		upstream:  upstream,
		ffmpegCmd: cmd,
		ffmpegOut: stdout,
		ffmpegIn:  stdin,
	}, nil
}

func (r *ffmpegAacReader) Read(p []byte) (n int, err error) {
	return r.ffmpegOut.Read(p)
}

func (r *ffmpegAacReader) Close() error {
	r.ffmpegIn.Close()
	return r.ffmpegCmd.Wait()
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
	streamDataPort uint16       // port the player connects to for audio data
	clientVolume   int          // last known volume 0-100 from player's setd cmd=5
	clientVolumeMu sync.Mutex
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
	streamDataPort = 0
	streamStopMu.Unlock()
}

// handleButtonPress dispatches preset button presses from SqueezePlay's
// CometD /slim/request channel. Called in a new goroutine.
func handleButtonPress(playerId string, btnName string, btnArg string) {
	slimConnMu.Lock()
	conn := slimConn
	slimConnMu.Unlock()
	if conn == nil {
		LogInfo("Button", "no SlimProto connection")
		return
	}

	// Combine btnName and btnArg if action is passed separately
	// e.g. btnName="1", btnArg="hold" -> "1.hold"
	// Also handle play/pause/play/unpause as button names
	fullName := btnName
	if btnArg != "" && !strings.Contains(btnName, ".") {
		fullName = btnName + "." + btnArg
	}

	// If btnName is a transport control, dispatch directly
	// For pause/onepause, check btnArg: "0" means resume, anything else means pause
	switch btnName {
	case "play", "unpause":
		resumePlayer(playerId, conn)
		return
	case "pause", "onepause":
		if btnArg == "0" {
			resumePlayer(playerId, conn)
		} else {
			pausePlayer(playerId)
		}
		return
	}

	var presetNum int
	var action string
	isValidPreset := false
	if n, err := fmt.Sscanf(fullName, "preset_%d.%s", &presetNum, &action); err == nil && n == 2 {
		isValidPreset = true
	} else if n, err := fmt.Sscanf(fullName, "%d.%s", &presetNum, &action); err == nil && n == 2 {
		isValidPreset = true
	}

	if isValidPreset {
		pb := getPlayerPlayback(playerId)
		if action == "hold" {
			pb.Mu.Lock()
			playing := pb.Playing
			title := pb.PlayingTitle
			icon := pb.PlayingIcon
			urlStr := pb.PlayingURL
			stationName := pb.StationName
			stationDesc := pb.StationDesc
			pb.Mu.Unlock()

			if playing && urlStr != "" {
				presetTitle := stationName
				if presetTitle == "" {
					presetTitle = title
				}
				presetsMu.Lock()
				presets[presetNum] = PresetItem{
					Title:   presetTitle,
					Subtext: stationDesc,
					Icon:    icon,
					URL:     urlStr,
				}
				presetsMu.Unlock()
				savePresets()
				LogInfo("Button", "Stored player %s stream to Preset %d: %s (%s)", playerId, presetNum, presetTitle, urlStr)
				publishPresetStored(playerId, presetNum)
			} else {
				LogInfo("Button", "Cannot store Preset %d: player is not playing anything", presetNum)
			}
			return
		} else if action == "single" {
			presetsMu.Lock()
			item, ok := presets[presetNum]
			presetsMu.Unlock()

			if ok && item.URL != "" {
				LogInfo("Button", "Recalling Preset %d: playing %s (%s)", presetNum, item.Title, item.URL)
				pb.Mu.Lock()
				pb.Playing = true
				pb.PlayingTitle = item.Title
				pb.PlayingDesc = item.Subtext
				pb.PlayingIcon = item.Icon
				pb.PlayingURL = item.URL
				pb.StationName = item.Title
				pb.StationDesc = item.Subtext
				pb.Mu.Unlock()

				go runURLStream(conn, item.URL, pb, playerId)

				go func() {
					time.Sleep(150 * time.Millisecond)
					publishPlayerStatus()
				}()
			} else {
				LogError("Button", "Recall Preset %d failed: not set", presetNum)
			}
			return
		}
	}

}

func handleJiveFavorites(respClid, playerId string, cmdArr []interface{}, paramsObj map[string]interface{}) {
	req := parseCLIRequest(cmdArr, paramsObj)
	if len(req.Args) == 0 {
		return
	}
	subCmd := req.Args[0]
	if subCmd == "set_preset" {
		keyStr := req.Params["key"]
		var presetNum int
		if _, err := fmt.Sscanf(keyStr, "%d", &presetNum); err == nil && presetNum >= 0 && presetNum <= 9 {
			urlStr := req.Params["favorites_url"]
			title := req.Params["favorites_title"]
			icon := req.Params["icon"]

			if urlStr != "" && title != "" {
				presetsMu.Lock()
				presets[presetNum] = PresetItem{
					Title:   title,
					Subtext: "",
					Icon:    icon,
					URL:     urlStr,
				}
				presetsMu.Unlock()
				savePresets()
				LogInfo("Presets", "Stored SqueezePlay player %s stream to Preset %d (from browse): %s (%s)", playerId, presetNum, title, urlStr)
				publishPresetStored(playerId, presetNum)
			} else {
				pb := getPlayerPlayback(playerId)
				pb.Mu.Lock()
				playing := pb.Playing
				pTitle := pb.PlayingTitle
				pIcon := pb.PlayingIcon
				pUrlStr := pb.PlayingURL
				stationName := pb.StationName
				stationDesc := pb.StationDesc
				pb.Mu.Unlock()

				if playing && pUrlStr != "" {
					presetTitle := stationName
					if presetTitle == "" {
						presetTitle = pTitle
					}
					presetsMu.Lock()
					presets[presetNum] = PresetItem{
						Title:   presetTitle,
						Subtext: stationDesc,
						Icon:    pIcon,
						URL:     pUrlStr,
					}
					presetsMu.Unlock()
					savePresets()
					LogInfo("Presets", "Stored SqueezePlay player %s stream to Preset %d (current track): %s (%s)", playerId, presetNum, presetTitle, pUrlStr)
					publishPresetStored(playerId, presetNum)
				} else {
					LogInfo("Presets", "Cannot store Preset %d: no item parameter and player is not playing anything", presetNum)
				}
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
func runURLStream(conn net.Conn, streamURL string, pb *PlayerPlaybackState, playerId string) {
	streamURL = resolveStreamURL(streamURL)
	if pb != nil {
		pb.Mu.Lock()
		pb.StreamURL = streamURL
		pb.PlayingURL = streamURL
		pb.Mu.Unlock()
	}
	streamStartTime := time.Now()

	// Start upstream fetch early so data may be ready when the player connects
	fetchCh := make(chan streamFetchResult, 1)
	go func() {
		insecureClient := newHTTPClient(0, 10*time.Second)
		req, err := http.NewRequest("GET", streamURL, nil)
		if err != nil {
			LogError("Stream", "request create failed: %v", err)
			fetchCh <- streamFetchResult{err: err}
			return
		}
		req.Header.Set("Icy-MetaData", "1")
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

		resp, err := insecureClient.Do(req)
		if err != nil {
			LogError("Stream", "upstream fetch failed after %v: %v", time.Since(streamStartTime), err)
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
		icyName := resp.Header.Get("icy-name")
		icyDesc := resp.Header.Get("icy-description")
		LogInfo("Stream", "upstream connected in %v, status=%s, content-type=%s, icy-metaint=%d, icy-name=%s", time.Since(streamStartTime), resp.Status, ct, icyMetaInt, icyName)
		fetchCh <- streamFetchResult{
			reader:      resp.Body,
			contentType: ct,
			httpStatus:  resp.Status,
			icyMetaInt:  icyMetaInt,
			icyName:     icyName,
			icyDesc:     icyDesc,
		}
	}()

	// Serialize with other stream/PCMTest requests to avoid races
	streamStartMu.Lock()
	defer streamStartMu.Unlock()

	// Tell the player to stop and flush its audio buffers
	if err := sendStrmStop(conn); err != nil {
		LogError("Stream", "sendStrmStop error: %v", err)
	}

	// Cancel any previous stream first
	stopCurrentStream()

	// Enable audio output — don't force volume, let the player use its knob position
	if err := sendAude(conn, true); err != nil {
		LogError("Stream", "sendAude error: %v", err)
	}
	if err := sendAudg(conn, -1); err != nil {
		LogError("Stream", "sendAudg error: %v", err)
	}
	listener, err := listenTCP4(":0")
	if err != nil {
		LogError("Stream", "listen error: %v", err)
		return
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	streamDataPort = port
	LogInfo("Stream", "t=%s listening on port %d for URL: %s", time.Since(streamStartTime), port, streamURL)

	// Register the listener BEFORE sendStrmCommand
	streamStopMu.Lock()
	streamStop = make(chan struct{})
	thisStop := streamStop
	streamDataLis = listener
	streamStopMu.Unlock()

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
			isActive := (streamStop == thisStop)
			if isActive {
				if streamStop != nil {
					close(streamStop)
					streamStop = nil
				}
				streamDataPort = 0
				streamDataLis = nil
			}
			streamStopMu.Unlock()
			if pb != nil {
				pb.Mu.Lock()
				if isActive {
					pb.Playing = false
				}
				pb.Mu.Unlock()
				publishPlayerStatus()
			}
		}()

		// Wait for the upstream fetch
		LogInfo("Stream", "t=%s waiting for upstream fetch...", time.Since(streamStartTime))
		result := <-fetchCh
		if result.err != nil {
			LogError("Stream", "http get error: %v", result.err)
			publishStreamError(playerId, conn, result.err)
			return
		}

		mode := byte('m')
		if result.contentType != "" {
			ct := strings.ToLower(result.contentType)
			switch {
			case strings.Contains(ct, "aac"):
				mode = 'a'
			case strings.Contains(ct, "ogg"):
				mode = 'o'
			case strings.Contains(ct, "flac"):
				mode = 'f'
			case strings.Contains(ct, "wav") || strings.Contains(ct, "pcm") || strings.Contains(ct, "l16"):
				mode = 'p'
			}
			LogInfo("Stream", "detected content-type=%s, using mode=%c", result.contentType, mode)
		}

		if err := sendStrmCommand(conn, 's', port, mode, 0, 0, 0, 0, defaultStreamThreshold(mode)); err != nil {
			listener.Close()
			LogError("Stream", "sendStrmCommand error: %v", err)
			return
		}
		LogInfo("Stream", "t=%s sent strm command mode=%c port=%d threshold=%dms", time.Since(streamStartTime), mode, port, defaultStreamThreshold(mode))

		if pb != nil && result.icyName != "" {
			pb.Mu.Lock()
			pb.StationName = result.icyName
			if pb.PlayingTitle == "" || strings.HasPrefix(pb.PlayingTitle, "http://") || strings.HasPrefix(pb.PlayingTitle, "https://") {
				pb.PlayingTitle = result.icyName
			}
			if pb.StationDesc == "" && result.icyDesc != "" {
				pb.StationDesc = result.icyDesc
			}
			if pb.PlayingDesc == "" && result.icyDesc != "" {
				pb.PlayingDesc = result.icyDesc
			}
			pb.Mu.Unlock()
			publishPlayerStatus()
		}
		upstreamTracker := &trackingReader{r: result.reader, count: 0}
		defer upstreamTracker.Close()

		onMeta := func(metaStr string) {
			streamStopMu.Lock()
			stillActive := (streamStop == thisStop)
			streamStopMu.Unlock()
			if !stillActive {
				return
			}
			title, artist := parseIcyMetadata(metaStr)
			if title != "" {
				LogInfo("ICY", "Song update: %s - %s", artist, title)
				if pb != nil {
					pb.Mu.Lock()
					pb.PlayingTitle = title
					pb.PlayingDesc = artist
					pb.Mu.Unlock()
					publishPlayerStatus()
				}
			}
		}

		var proxySource io.Reader = &IcyReader{
			reader:      upstreamTracker,
			metaInt:     result.icyMetaInt,
			bytesToNext: result.icyMetaInt,
			onMetadata:  onMeta,
		}

		if mode == 'a' && isFfmpegAvailable() {
			ffmpegR, err := newFfmpegAacReader(upstreamTracker, result.icyMetaInt, onMeta)
			if err == nil {
				proxySource = ffmpegR
				defer ffmpegR.Close()
			}
		}

		LogInfo("Stream", "t=%s upstream ready, proxying %s (icy-metaint=%d)", time.Since(streamStartTime), streamURL, result.icyMetaInt)

		for {
			select {
			case <-thisStop:
				LogInfo("Stream", "cancelled by new stream")
				return
			default:
			}

			playerConn, err := listener.Accept()
			if err != nil {
				streamStopMu.Lock()
				isActive := (streamStop == thisStop)
				streamStopMu.Unlock()
				if isActive {
					LogError("Stream", "accept error: %v", err)
				}
				return
			}

		func(c net.Conn) {
			defer c.Close()

			ct := "audio/mpeg"
			if result.contentType != "" {
				ct = result.contentType
			}
			headerStr := fmt.Sprintf("HTTP/1.0 200 OK\r\nContent-Type: %s\r\nConnection: close\r\n\r\n", ct)
			if _, err := c.Write([]byte(headerStr)); err != nil {
				LogError("Stream", "write header error: %v", err)
				return
			}
			LogInfo("Stream", "t=%s wrote HTTP headers content-type=%s mode=%c", time.Since(streamStartTime), ct, mode)

				totalWritten := int64(0)
				lastLog := time.Now()
				buf := make([]byte, 65536)
				firstLog := true
				for {
					select {
					case <-thisStop:
						LogInfo("Stream", "cancelled by new stream")
						return
					default:
					}

					n, err := proxySource.Read(buf)
					if n > 0 {
						if firstLog {
							LogDebug("Stream", "first %d bytes of content: % x", n, buf[:n])
							firstLog = false
						}
						if _, werr := c.Write(buf[:n]); werr != nil {
							pb.Mu.Lock()
							isPaused := pb.Paused
							pb.Mu.Unlock()
							if !isPaused {
								streamStopMu.Lock()
								stillActive := (streamStop == thisStop)
								streamStopMu.Unlock()
								if stillActive {
									LogError("Stream", "write error: %v", werr)
									if !strings.Contains(werr.Error(), "broken pipe") && !strings.Contains(werr.Error(), "connection reset by peer") {
										publishStreamError(playerId, conn, werr)
									}
								}
							} else {
								LogInfo("Stream", "write error (player paused): %v", werr)
							}
							return
						}
						totalWritten += int64(n)
						if time.Since(lastLog) > 5*time.Second {
							LogDebug("Stream", "wrote %d bytes so far", totalWritten)
							lastLog = time.Now()
						}
					}
					if err != nil {
						if err == io.EOF {
							LogInfo("Stream", "upstream EOF at offset %d, attempting to resume...", upstreamTracker.count)
							newReader, _, resumeErr := resumeUpstream(streamURL, upstreamTracker.count)
							if resumeErr == nil && newReader != nil {
								LogInfo("Stream", "successfully resumed upstream at offset %d", upstreamTracker.count)
								upstreamTracker.r.Close()
								upstreamTracker.r = newReader
								continue
							}
							LogDebug("Stream", "stream ended (cannot resume or EOF): total written %d bytes, resumeErr: %v", totalWritten, resumeErr)
							return
						}
						LogError("Stream", "read error: %v, attempting to resume...", err)
						newReader, _, resumeErr := resumeUpstream(streamURL, upstreamTracker.count)
						if resumeErr == nil && newReader != nil {
							LogError("Stream", "successfully resumed upstream after error at offset %d", upstreamTracker.count)
							upstreamTracker.r.Close()
							upstreamTracker.r = newReader
							continue
						}
						LogError("Stream", "read error final: %v, resumeErr: %v", err, resumeErr)
						publishStreamError(playerId, conn, err)
						return
					}
				}
			}(playerConn)
		}
	}()
}


type streamFetchResult struct {
	reader      io.ReadCloser
	contentType string
	httpStatus  string
	icyMetaInt  int
	icyName     string
	icyDesc     string
	err         error
}

func handleStreamStart(w *minimalResponseWriter, r *minimalRequest) {
	streamURL := r.query["url"]
	if streamURL != "" {
		streamURL = resolveStreamURL(streamURL)
	}

	slimConnMu.Lock()
	conn := slimConn
	slimConnMu.Unlock()
	if conn == nil {
		w.WriteHeader(http.StatusServiceUnavailable); w.Write([]byte("no SlimProto connection"))
		return
	}
	streamFile := r.query["file"]
	playerId := normalizePlayerId(r.query["player"])

	if streamURL != "" {
		pb := getPlayerPlayback(playerId)
		pb.Mu.Lock()
		pb.StreamURL = streamURL
		pb.PlayingURL = streamURL
		pb.Playing = true
		pb.Mu.Unlock()
	}

	// Start upstream fetch early so data may be ready when the player connects
	var fetchCh chan streamFetchResult
	streamStartTime := time.Now()
	if streamURL != "" {
		fetchCh = make(chan streamFetchResult, 1)
		go func() {
			insecureClient := newHTTPClient(0, 10*time.Second)
			resp, err := insecureClient.Get(streamURL)
			if err != nil {
				LogError("Stream", "upstream fetch failed after %v: %v", time.Since(streamStartTime), err)
				fetchCh <- streamFetchResult{err: err}
				return
			}
			ct := "audio/mpeg"
			if h := resp.Header.Get("Content-Type"); h != "" {
				ct = h
			}
			LogInfo("Stream", "upstream connected in %v, status=%s, content-type=%s", time.Since(streamStartTime), resp.Status, ct)
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
		LogError("Stream", "sendStrmStop error: %v", err)
	}

	// Cancel any previous stream first, so the player doesn't see the old
	// data connection drop while processing the new strm command.
	stopCurrentStream()

	// Enable audio output — don't force volume, let the player use its knob position
	if err := sendAude(conn, true); err != nil {
		LogError("Stream", "sendAude error: %v", err)
	}
	if err := sendAudg(conn, -1); err != nil {
		LogError("Stream", "sendAudg error: %v", err)
	}
	listener, err := listenTCP4(":0")
	if err != nil {
		LogError("Stream", "listen error: %v", err)
		w.WriteHeader(http.StatusInternalServerError); w.Write([]byte(err.Error()))
		return
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	streamDataPort = port

	if streamURL != "" {
		LogInfo("Stream", "t=%s listening on port %d for URL: %s", time.Since(streamStartTime), port, streamURL)
	} else {
		LogInfo("Stream", "listening on port %d for file: %s", port, streamFile)
	}

	// Register the listener BEFORE sendStrmCommand so the stream lifecycle is
	// fully set up when the player starts connecting.
	streamStopMu.Lock()
	streamStop = make(chan struct{})
	thisStop := streamStop
	streamDataLis = listener
	streamStopMu.Unlock()

	if err := sendStrmCommand(conn, 's', port, 'm', 0, 0, 0, 0); err != nil {
		listener.Close()
		LogError("Stream", "sendStrmCommand error: %v", err)
		w.WriteHeader(http.StatusInternalServerError); w.Write([]byte(err.Error()))
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
				streamDataPort = 0
				streamDataLis = nil
			}
			streamStopMu.Unlock()
		}()

		var reader io.ReadCloser
		if streamFile != "" {
			f, err := os.Open(streamFile)
			if err != nil {
				LogError("Stream", "open file error: %v", err)
				return
			}
			reader = f
			LogInfo("Stream", "player connected, serving file %s", streamFile)
		} else {
			// Wait for the upstream fetch started in the handler
			LogInfo("Stream", "t=%s waiting for upstream fetch...", time.Since(streamStartTime))
			result := <-fetchCh
			if result.err != nil {
				LogError("Stream", "http get error: %v", result.err)
				publishStreamError(playerId, conn, result.err)
				return
			}
			reader = result.reader
			LogInfo("Stream", "t=%s upstream ready, proxying %s", time.Since(streamStartTime), streamURL)
		}
		defer reader.Close()

		for {
			// Check if a new stream has cancelled us
			select {
			case <-thisStop:
				LogInfo("Stream", "cancelled by new stream")
				return
			default:
			}

			playerConn, err := listener.Accept()
			if err != nil {
				streamStopMu.Lock()
				isActive := (streamStop == thisStop)
				streamStopMu.Unlock()
				if isActive {
					LogError("Stream", "accept error: %v", err)
				}
				return
			}

			func(c net.Conn) {
				defer c.Close()

				// Write HTTP headers immediately so the player can start buffering
				headerStr := "HTTP/1.0 200 OK\r\nContent-Type: audio/mpeg\r\nConnection: close\r\n\r\n"
				if streamFile != "" {
					headerStr = "HTTP/1.0 200 OK\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n"
				}
				if _, err := c.Write([]byte(headerStr)); err != nil {
					LogError("Stream", "write header error: %v", err)
					return
				}
				LogInfo("Stream", "t=%s wrote HTTP headers", time.Since(streamStartTime))

				totalWritten := int64(0)
				lastLog := time.Now()
				buf := make([]byte, 65536)
				firstLog := true
				for {
					// Check if a new stream has cancelled us
					select {
					case <-thisStop:
						LogInfo("Stream", "cancelled by new stream")
						return
					default:
					}

					n, err := reader.Read(buf)
					if n > 0 {
						if firstLog {
							LogDebug("Stream", "first %d bytes of content: % x", n, buf[:n])
							firstLog = false
						}
						if _, werr := c.Write(buf[:n]); werr != nil {
							LogError("Stream", "write error: %v", werr)
							if !strings.Contains(werr.Error(), "broken pipe") {
								publishStreamError(playerId, conn, werr)
							}
							return
						}
						totalWritten += int64(n)
						if time.Since(lastLog) > 5*time.Second {
							LogDebug("Stream", "wrote %d bytes so far", totalWritten)
							lastLog = time.Now()
						}
					}
					if err != nil {
						if err == io.EOF {
							LogDebug("Stream", "stream ended, total written: %d bytes", totalWritten)
							return
						}
						LogError("Stream", "read error: %v", err)
						publishStreamError(playerId, conn, err)
						return
					}
				}
			}(playerConn)
		}
	}()

	if streamURL != "" {
		w.Header()["Content-Type"] = "text/plain"
		fmt.Fprintf(w, "streaming %s on port %d\n", streamURL, port)
	} else {
		w.Header()["Content-Type"] = "text/plain"
		fmt.Fprintf(w, "streaming file %s on port %d\n", streamFile, port)
	}
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
	LogInfo("TCP", "New SlimProto connection from %s", remote)

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
					LogInfo("TCP", "%s keepalive sent", remote)
				case <-done:
					return
				}
			}
		}()

	for {
		opcode, payload, err := readPacket(conn)
		if err != nil {
			if err != io.EOF {
				LogError("TCP", "%s read error: %v", remote, err)
			} else {
				LogInfo("TCP", "%s disconnected", remote)
			}
			return
		}

		switch opcode {
		case "HELO":
			h := parseHELO(payload)
			LogInfo("TCP", "HELO from %s  device=%d rev=%d mac=%s lang=%s caps=%q", remote, h.DeviceID, h.Revision, h.MAC, h.Lang, h.Capabilities)

			heloReceived = true
			clientMAC = h.MAC

			addConnectedPlayer(h.MAC, h.MAC, "squeezeplay", "player-"+h.MAC)
			pushServerstatusToAll(m)

			if err := sendVers(conn, m.Version); err != nil {
				LogError("TCP", "%s error sending vers: %v", remote, err)
				return
			}

			if err := sendSetdName(conn, m.Name); err != nil {
				LogError("TCP", "%s error sending setd name: %v", remote, err)
				return
			}

			LogInfo("TCP", "%s HELO complete, connection established", remote)

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
					LogError("TCP", "%s error sending SETD %d: %v", remote, kv.cmd, err)
					return
				}
				LogInfo("TCP", "%s SETD %d sent", remote, kv.cmd)
			}

			// Send initial strm metadata packet — SqueezePlay needs this
			// to initialize its audio pipeline.
			if err := sendStrm(conn, 0, 0, "", "", "", 0); err != nil {
				LogError("TCP", "%s error sending initial strm: %v", remote, err)
				return
			}
			LogInfo("TCP", "%s initial strm sent", remote)

		if streamDataPort != 0 {
			if err := sendStrmCommand(conn, 's', streamDataPort, 'm', 0, 0, 0, 0); err != nil {
				LogError("TCP", "%s error sending resume strm: %v", remote, err)
			} else {
					LogInfo("TCP", "%s resumed stream on port %d", remote, streamDataPort)
				}
		}

	case "aude":
		if !heloReceived {
			LogInfo("TCP", "%s aude before HELO, ignoring", remote)
			continue
		}
		LogInfo("TCP", "%s aude payload=%x", remote, payload)
		if err := sendAudg(conn, -1); err != nil {
			LogError("TCP", "%s error sending audg: %v", remote, err)
		}
		if err := sendAude(conn, true); err != nil {
			LogError("TCP", "%s error sending aude: %v", remote, err)
		}

	case "STAT":
			if !heloReceived {
				LogInfo("TCP", "%s STAT before HELO, ignoring", remote)
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
			LogInfo("TCP", "%s STAT event=%q (%d bytes)%s", remote, event, len(payload), extra)

		case "RESP":
			// Player echoes HTTP response headers from the data stream connection
			// back on the control connection. No action needed.
			if len(payload) > 0 {
				LogInfo("TCP", "%s RESP (%d bytes)", remote, len(payload))
			}

		case "BYE!":
			LogInfo("TCP", "%s BYE! received", remote)
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
			LogInfo("TCP", "%s DSCO reason=%d (stream disconnected, NOT control connection)", remote, reason)
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
						clientVolumeMu.Lock()
						clientVolume = vol
						clientVolumeMu.Unlock()
						LogInfo("TCP", "%s volume: %d", remote, vol)
					} else {
						LogInfo("TCP", "%s setd cmd=%d raw=%x", remote, cmd, payload)
					}
				default:
					LogInfo("TCP", "%s setd cmd=%d raw=%x", remote, cmd, payload)
				}
			} else {
				LogInfo("TCP", "%s setd (empty)", remote)
			}

		default:
			LogInfo("TCP", "%s unknown opcode %q (%d bytes) %x", remote, opcode, len(payload), payload)
		}
	}
}

// buildServerStatusData constructs the common serverstatus data payload.
func buildServerStatusData(m *ServerMetadata) map[string]interface{} {
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
		"version":          m.Version,
		"uuid":             m.UUID,
		"name":             m.Name,
		"ip":               m.IPAddress,
		"port":             m.JSONPort,
		"cliport":          m.CLIPort,
		"compatible":       true,
		"player count":     playerCount,
		"players_loop":     playersLoop,
		"canMusicFolder":   true,
		"canRandomPlay":    true,
		"canInternetRadio": true,
		"canRemoteStream":  true,
		"lang":             "EN",
	}
	return data
}

// pushServerstatusToAll pushes a serverstatus event to all streaming CometD clients.
func pushServerstatusToAll(m *ServerMetadata) {
	data := buildServerStatusData(m)

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
			LogInfo("CometD", "dropping serverstatus push for clientId=%s (channel full)", clientId)
		}
	}
}

// startTCPServer starts the TCP SlimProto control server.
func startTCPServer(metadata *ServerMetadata) {
	addr := fmt.Sprintf(":%d", DefaultPort)
	listener, err := listenTCP4(addr)
	if err != nil {
		LogError("TCP", "Error starting TCP listener on %s: %v", addr, err)
		os.Exit(1)
	}
	LogInfo("general", "Listening for SlimProto TCP connections on %s...", addr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			LogError("TCP", "Accept error: %v", err)
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
		LogInfo("general", "[%s] TLV request from %v tags=%v", mode, raddr, tags)
		rsp := buildTLVResponse(tags, m)
		conn.WriteToUDP(rsp, raddr)
		LogInfo("general", "[%s] TLV response sent to %v", mode, raddr)

	case 'd': // legacy discovery request
		LogInfo("general", "[%s] Legacy request from %v", mode, raddr)
		conn.WriteToUDP(buildLegacyResponse(m), raddr)
		LogInfo("general", "[%s] Legacy response sent to %v", mode, raddr)

	default:
		LogInfo("general", "[%s] Unknown UDP prefix 0x%02x from %v", mode, data[0], raddr)
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
func handleArtwork(w *minimalResponseWriter, r *minimalRequest) {
	path := r.path
	LogInfo("HTTP", "artwork request: %s", path)

	w.Header()["Content-Type"] = "image/png"
	w.Header()["Cache-Control"] = "max-age=86400"
	w.Header()["Content-Length"] = fmt.Sprintf("%d", len(placeholderPNG))
	w.WriteHeader(http.StatusOK)
	w.Write(placeholderPNG)
}

func handlePlugins(w *minimalResponseWriter, r *minimalRequest) {
	path := r.path
	if strings.Contains(path, "..") {
		w.WriteHeader(http.StatusForbidden); w.Write([]byte("Forbidden"))
		return
	}
	if strings.HasPrefix(path, "/plugins/") {
		// Try embedded assets first
		if data, ok := embeddedAssets[path]; ok {
			w.Header()["Content-Type"] = "image/png"
			w.Header()["Cache-Control"] = "max-age=86400"
			w.Header()["Content-Length"] = fmt.Sprintf("%d", len(data))
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}

		// Try cleaning size suffixes from query paths for embedded assets
		cleanPath := stripImageSizeSuffix(path)
		if cleanPath != path {
			if data, ok := embeddedAssets[cleanPath]; ok {
				w.Header()["Content-Type"] = "image/png"
				w.Header()["Cache-Control"] = "max-age=86400"
				w.Header()["Content-Length"] = fmt.Sprintf("%d", len(data))
				w.WriteHeader(http.StatusOK)
				w.Write(data)
				return
			}
		}

		// Fallback to local files if present
		localPath := "." + path
		if _, err := os.Stat(localPath); err == nil {
			serveLocalFile(w, r, localPath)
			return
		}
		cleanLocalPath := stripImageSizeSuffix(localPath)
		if cleanLocalPath != localPath {
			if _, err := os.Stat(cleanLocalPath); err == nil {
				serveLocalFile(w, r, cleanLocalPath)
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

type CachedImage struct {
	Body        []byte
	ContentType string
}

var (
	imageCacheMu    sync.RWMutex
	imageCache      = make(map[string]CachedImage)
	cacheTotalBytes = 0
)

func handleImageProxy(w *minimalResponseWriter, r *minimalRequest) {
	u := r.query["u"]
	if u == "" {
		handleArtwork(w, r)
		return
	}

	decodedURL, err := url.QueryUnescape(u)
	if err == nil {
		u = decodedURL
	}

	// Strip size suffixes like _40x40_m or _50x50 from remote image requests
	u = stripImageSizeSuffix(u)

	// Check cache first
	imageCacheMu.RLock()
	cached, found := imageCache[u]
	imageCacheMu.RUnlock()

	if found {
		LogInfo("ImageProxy", "[HIT] Serving from cache: %s", u)
		if cached.ContentType != "" {
			w.Header()["Content-Type"] = cached.ContentType
		}
		w.Header()["Content-Length"] = fmt.Sprintf("%d", len(cached.Body))
		w.WriteHeader(http.StatusOK)
		w.Write(cached.Body)
		return
	}

	LogInfo("ImageProxy", "[MISS] Fetching remote URL: %s (original: %s)", u, r.query["u"])

	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get(u)
	if err != nil {
		LogError("ImageProxy", "Fetch error: %v", err)
		handleArtwork(w, r)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		LogInfo("ImageProxy", "Remote server returned status %d", resp.StatusCode)
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		LogError("ImageProxy", "Error reading body: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	ct := resp.Header.Get("Content-Type")

	// Cache the image under a strict 2MB memory budget
	imageCacheMu.Lock()
	for cacheTotalBytes+len(bodyBytes) > 2*1024*1024 && len(imageCache) > 0 {
		var evictKey string
		for k := range imageCache {
			evictKey = k
			break // Pseudo-random eviction based on map order
		}
		cacheTotalBytes -= len(imageCache[evictKey].Body)
		delete(imageCache, evictKey)
	}
	if cacheTotalBytes+len(bodyBytes) <= 2*1024*1024 {
		imageCache[u] = CachedImage{
			Body:        bodyBytes,
			ContentType: ct,
		}
		cacheTotalBytes += len(bodyBytes)
		LogInfo("ImageProxy", "Cached new image: %s, size=%d bytes, cache total=%d bytes", u, len(bodyBytes), cacheTotalBytes)
	}
	imageCacheMu.Unlock()

	if ct != "" {
		w.Header()["Content-Type"] = ct
	}
	w.Header()["Content-Length"] = fmt.Sprintf("%d", len(bodyBytes))
	w.WriteHeader(http.StatusOK)
	w.Write(bodyBytes)
}


// startHTTPServer starts the HTTP CometD server (for JSON-RPC and CometD
func startHTTPServer(m *ServerMetadata) {
	LogInfo("general", "Starting HTTP server on port %s...", m.JSONPort)
	hl, err := listenTCP4(":"+m.JSONPort)
	if err != nil {
		LogError("general", "HTTP server listen error: %v", err)
		os.Exit(1)
	}
	serveHTTP(hl, func(w *minimalResponseWriter, r *minimalRequest) {
		switch {
		case r.path == "/stream/play":
			handleStreamStart(w, r)
		case r.path == "/cometd":
			handleCometd(w, r, m)
		case r.path == "/jsonrpc":
			handleJsonRpc(w, r, m)
		case r.path == "/imageproxy" || r.path == "/imageproxy/":
			handleImageProxy(w, r)
		case strings.HasPrefix(r.path, "/music/") || strings.HasPrefix(r.path, "/contributor/") || strings.HasPrefix(r.path, "/html/"):
			handleArtwork(w, r)
		case strings.HasPrefix(r.path, "/plugins/"):
			handlePlugins(w, r)
		default:
			if r.path != "/" && r.path != "/cometd" && r.path != "/jsonrpc" {
				handleArtwork(w, r)
				return
			}
			w.Header()["Content-Type"] = "application/json"
			w.Write([]byte(`{"ok":true}`))
		}
	})
}

// handleCometd handles CometD protocol requests
func handleCometd(w *minimalResponseWriter, r *minimalRequest, m *ServerMetadata) {
	if r.method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var reqs []CometdRequest
	if err := json.Unmarshal(r.body, &reqs); err != nil {
		LogError("CometD", "failed to decode request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	LogInfo("CometD", "from %s", r.remoteAddr)
	for i, req := range reqs {
		LogInfo("CometD", "<- req[%d] channel=%q clientId=%q", i, req.Channel, req.ClientId)
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

	if !isHandshake && clientId != "" {
		clientSessionsMu.Lock()
		_, ok := clientSessions[clientId]
		clientSessionsMu.Unlock()
		if !ok {
			LogInfo("CometD", "Unknown clientId=%s, advising client to handshake", clientId)
			resps := []CometdRequest{
				{
					Channel:    reqs[0].Channel,
					Successful: false,
					Id:         reqs[0].Id,
					ClientId:   clientId,
					Error:      "402::Unknown client",
					Advice: map[string]interface{}{
						"reconnect": "handshake",
					},
				},
			}
			body, _ := json.Marshal(resps)
			w.Header()["Content-Type"] = "application/json"
			w.Header()["Content-Length"] = fmt.Sprintf("%d", len(body))
			w.WriteHeader(http.StatusOK)
			w.Write(body)
			return
		}
	}

	// Generate responses and detect subscribe/request channels for event scheduling
	resps := make([]CometdRequest, 0, len(reqs))
	for _, req := range reqs {
		resp := cometdResponse(req, m, clientId)
		resps = append(resps, resp)
	}

	for i, resp := range resps {
		LogInfo("CometD", "-> resp[%d] channel=%q clientId=%q", i, resp.Channel, resp.ClientId)
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

func handleCometdNormal(w *minimalResponseWriter, resps []CometdRequest, isHandshake bool) {
	body, err := json.Marshal(resps)
	if err != nil {
		LogError("CometD", "marshal error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	LogInfo("CometD", "response body: %s", string(body))
	w.Header()["Content-Type"] = "application/json"
	w.Header()["Content-Length"] = fmt.Sprintf("%d", len(body))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func handleCometdStreaming(w *minimalResponseWriter, clientId string, initialResps []CometdRequest, sc *StreamingClient, m *ServerMetadata) {
	// Mark clientId on responses
	for i := range initialResps {
		if initialResps[i].ClientId == "" {
			initialResps[i].ClientId = clientId
		}
	}

	w.Header()["Content-Type"] = "application/json"
	w.Header()["Transfer-Encoding"] = "chunked"
	w.WriteHeader(http.StatusOK)

	// Write initial response
	enc := json.NewEncoder(w)
	if err := enc.Encode(initialResps); err != nil {
		LogError("CometD", "streaming initial write error: %v", err)
		return
	}
	w.Flush()
	LogInfo("CometD", "streaming started for clientId=%s", clientId)

	// Push first serverstatus event immediately so the client gets initial player data
	go func() {
		time.Sleep(50 * time.Millisecond)
		data := buildServerStatusData(m)

		sc.events <- []interface{}{
			map[string]interface{}{
				"channel": "/" + clientId + "/slim/serverstatus",
				"data":    data,
			},
		}
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case events, ok := <-sc.events:
			if !ok {
				LogInfo("CometD", "streaming channel closed for clientId=%s", clientId)
				return
			}
			if err := enc.Encode(events); err != nil {
				LogError("CometD", "streaming write error: %v", err)
				return
			}
			w.Flush()
		case <-ticker.C:
			// Send an empty JSON array to keep the chunked HTTP connection alive
			if _, err := w.Write([]byte("[]\n")); err != nil {
				LogError("CometD", "streaming keepalive write error for clientId=%s: %v", clientId, err)
				return
			}
			w.Flush()
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
		getClientSession(clid)
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
			respClid = extractClientID(respPath)
			}
		}
		if respClid == "" {
			respClid = clid
		}

		// Check if this is a serverstatus or status subscription and push event
		if req.Data != nil {
			LogInfo("CometD", "Slim subscribe: response=%q request=%+v", req.Data["response"], req.Data["request"])
			if requestArr, ok := req.Data["request"].([]interface{}); ok && len(requestArr) >= 2 {
				if args, ok := requestArr[1].([]interface{}); ok && len(args) > 0 {
					if cmd, ok := args[0].(string); ok {
						if cmd == "serverstatus" {
							done := make(chan struct{}, 1)
							pushServerStatus(respClid, req.Id, m, done)
							<-done
						} else if cmd == "status" {
							go func() {
								time.Sleep(50 * time.Millisecond)
								publishPlayerStatus()
							}()
						}
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
			LogInfo("CometD", "/slim/request data: %v", req.Data)
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
				respClid = extractClientID(respPath)
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
				} else if pFloat, ok := requestArr[0].(float64); ok {
					reqPlayerId = fmt.Sprintf("%.0f", pFloat)
				}
				reqPlayerId = normalizePlayerId(reqPlayerId)

				if cmdArr, ok := requestArr[1].([]interface{}); ok && len(cmdArr) > 0 {
					var paramsObj map[string]interface{}
					if len(requestArr) >= 3 {
						paramsObj, _ = requestArr[2].(map[string]interface{})
					}
					if cmd, ok := cmdArr[0].(string); ok {
						isRadioCmd := false
						if cmd == "radios" {
							cmdReq := parseCLIRequest(cmdArr, paramsObj)
							if len(cmdReq.Args) > 0 {
								subCmd := cmdReq.Args[0]
								if subCmd == "items" {
									handleRadioSubmenu(respClid, reqPlayerId, cmdReq, m)
								} else if subCmd == "playlist" {
									handleRadioPlay(respClid, reqPlayerId, cmdReq)
								} else {
									handleRadiosMenu(respClid, reqPlayerId, m)
								}
							} else {
								handleRadiosMenu(respClid, reqPlayerId, m)
							}
							isRadioCmd = true
						} else if cmd == "radiobrowser" {
							cmdReq := parseCLIRequest(cmdArr, paramsObj)
							if len(cmdReq.Args) > 0 {
								subCmd := cmdReq.Args[0]
								switch subCmd {
							case "topvote":
								limit := 100
								if len(cmdReq.Args) > 2 {
									if l, err := strconv.Atoi(cmdReq.Args[2]); err == nil && l > 0 && l <= 200 {
										limit = l
									}
								}
								if limit > 100 {
									limit = 100
								}
								handleRadioBrowserTopVote(respClid, reqPlayerId, limit)
								case "search":
									query := ""
									if s, ok := paramsObj["query"].(string); ok && s != "" && s != "__TAGGEDINPUT__" {
										query = s
									}
									if query == "" && len(cmdReq.Args) > 1 {
										arg := cmdReq.Args[1]
										if _, err := strconv.Atoi(arg); err != nil {
											query = arg
										}
									}
									handleRadioBrowserSearch(respClid, reqPlayerId, query)
								case "tags":
									handleRadioBrowserTags(respClid, reqPlayerId)
								case "tagstations":
									tag := ""
									if len(cmdReq.Args) > 1 {
										tag = cmdReq.Args[1]
									}
									if tag == "" {
										if s, ok := paramsObj["tag"].(string); ok {
											tag = s
										}
									}
									handleRadioBrowserTagStations(respClid, reqPlayerId, tag)
								case "countries":
									handleRadioBrowserCountries(respClid, reqPlayerId)
								case "countrystations":
									country := ""
									if len(cmdReq.Args) > 1 {
										country = cmdReq.Args[1]
									}
									if country == "" {
										if s, ok := paramsObj["country"].(string); ok {
											country = s
										}
									}
									handleRadioBrowserCountryStations(respClid, reqPlayerId, country)
							case "play":
								stationUUID := ""
								if s, ok := cmdReq.Params["stationuuid"]; ok && s != "" {
									stationUUID = s
								}
								if stationUUID == "" {
									for _, arg := range cmdReq.Args {
										if strings.HasPrefix(arg, "stationuuid:") {
											stationUUID = strings.TrimPrefix(arg, "stationuuid:")
											break
										}
									}
								}
								LogInfo("RadioBrowser", "Play request: args=%v params=%v stationUUID=%s", cmdReq.Args, cmdReq.Params, stationUUID)
								playRadioBrowserStation(respClid, reqPlayerId, stationUUID)
								default:
									handleRadioBrowserMenu(respClid, reqPlayerId)
								}
							} else {
								handleRadioBrowserMenu(respClid, reqPlayerId)
							}
							isRadioCmd = true
						} else {
							for _, sub := range defaultSubmenus {
								if sub["id"] == cmd {
									cmdReq := parseCLIRequest(cmdArr, paramsObj)
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
									btnName := ""
									switch v := cmdArr[1].(type) {
									case string:
										btnName = v
									case float64:
										btnName = fmt.Sprintf("%.0f", v)
									}
									btnArg := ""
									if len(cmdArr) >= 3 {
										switch v := cmdArr[2].(type) {
										case string:
											btnArg = v
										case float64:
											btnArg = fmt.Sprintf("%.0f", v)
										}
									}
									LogInfo("Button", "CometD button for player=%s: %s arg=%s", reqPlayerId, btnName, btnArg)
									go handleButtonPress(reqPlayerId, btnName, btnArg)
								}
							case "pause", "play", "onepause", "unpause":
								arg := ""
								if len(cmdArr) >= 2 {
									switch v := cmdArr[1].(type) {
									case string:
										arg = v
									case float64:
										arg = fmt.Sprintf("%.0f", v)
									}
								}
								LogInfo("Button", "CometD %s for player=%s arg=%s", cmd, reqPlayerId, arg)
								go handleButtonPress(reqPlayerId, cmd, arg)
							case "mode":
								if len(cmdArr) >= 2 {
									mode := ""
									switch v := cmdArr[1].(type) {
									case string:
										mode = v
									case float64:
										mode = fmt.Sprintf("%.0f", v)
									}
									LogInfo("Button", "CometD mode=%s for player=%s", mode, reqPlayerId)
									go handleButtonPress(reqPlayerId, mode, "")
								}
							case "jivefavorites":
								go handleJiveFavorites(respClid, reqPlayerId, cmdArr, paramsObj)
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
		LogInfo("Radio", "Dynamic main menu fetched successfully")
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
					"cmd": []interface{}{"radios", "items"},
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
							"cmd": []interface{}{"radios", "items"},
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
							"cmd": []interface{}{"radios", "items"},
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
		LogError("Radio", "Falling back to default static main menu due to error: %v", err)
		session.Mu.Lock()
		for _, sub := range defaultSubmenus {
			itemID := fmt.Sprintf("item_%d", session.NextID)
			session.NextID++

			isSearch, _ := sub["isSearch"].(bool)

			if isSearch {
				items = append(items, map[string]interface{}{
					"id":   itemID,
					"text": sub["text"],
					"type": "search",
					"icon": sub["icon"],
					"actions": map[string]interface{}{
						"go": map[string]interface{}{
							"cmd": []interface{}{"radios", "items"},
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
							"cmd": []interface{}{"radios", "items"},
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

	session.Mu.Lock()
	rbItemID := fmt.Sprintf("item_%d", session.NextID)
	session.NextID++
	items = append(items, map[string]interface{}{
		"id":     rbItemID,
		"text":   "Radio Browser",
		"weight": len(items) * 10,
		"icon":   "/plugins/TuneIn/html/images/radio.png",
		"actions": map[string]interface{}{
			"go": map[string]interface{}{
				"cmd": []interface{}{"radiobrowser"},
			},
		},
	})
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

func getBasePresetActions() map[string]interface{} {
	actions := map[string]interface{}{}
	for preset := 0; preset <= 9; preset++ {
		key := fmt.Sprintf("set-preset-%d", preset)
		actions[key] = map[string]interface{}{
			"player":      0,
			"cmd":         []interface{}{"jivefavorites", "set_preset", fmt.Sprintf("key:%d", preset)},
			"itemsParams": "presetParams",
		}
	}
	return actions
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
		LogInfo("Radio", "Fetching OPML feed URL: %s", feedURL)
		opml, err := fetchOPML(feedURL)
		if err != nil {
			LogError("Radio", "Error fetching subfeed: %v", err)
			return
		}
		feedItems = opml.Items
	}

	var items []map[string]interface{}
	session.Mu.Lock()
	hasPlayable := false

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

			presetParams := map[string]interface{}{
				"favorites_url":   out.URL,
				"favorites_title": out.Text,
				"favorites_type":  "audio",
				"icon":            cleanIconURL(out.Image),
			}
			hasPlayable = true

			items = append(items, map[string]interface{}{
				"id":           itemID,
				"text":         out.Text,
				"subtext":      subtext,
				"type":         "audio",
				"icon":         cleanIconURL(out.Image),
				"presetParams": presetParams,
				"actions": map[string]interface{}{
					"go": map[string]interface{}{
						"cmd": []interface{}{"radios", "playlist", "play"},
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
						"cmd": []interface{}{"radios", "items"},
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
						"cmd": []interface{}{"radios", "items"},
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
	if hasPlayable {
		data["base"] = map[string]interface{}{
			"actions": getBasePresetActions(),
		}
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
		LogError("Radio", "Play error: item %s not found in cache", itemID)
		return
	}

	pb := getPlayerPlayback(playerId)
	pb.Mu.Lock()
	pb.Playing = true
	pb.PlayingURL = streamURL
	if hasMeta {
		pb.PlayingTitle = meta.Title
		pb.PlayingDesc = meta.Subtext
		pb.PlayingIcon = meta.Icon
		pb.StationName = meta.Title
		pb.StationDesc = meta.Subtext
	} else {
		pb.PlayingTitle = streamURL
		pb.PlayingDesc = ""
		pb.PlayingIcon = ""
		pb.StationName = streamURL
		pb.StationDesc = ""
	}
	pb.Mu.Unlock()

	slimConnMu.Lock()
	conn := slimConn
	slimConnMu.Unlock()

	if conn == nil {
		LogError("Radio", "Play error: no SlimProto connection")
		return
	}

	LogInfo("Radio", "Tuning to station ID=%s, URL=%s", itemID, streamURL)
	go runURLStream(conn, streamURL, pb, playerId)

	buildStatusResponse(respClid, playerId, nil, globalServerMetadata)

	go func() {
		time.Sleep(150 * time.Millisecond)
		publishPlayerStatus()
	}()
}

func buildMenuResponse(respClid, playerId string, m *ServerMetadata) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		LogInfo("Menu", "no streaming client found for %s", respClid)
		return
	}

	data := map[string]interface{}{
		"count":     1,
		"offset":    0,
		"item_loop": []interface{}{
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
	LogInfo("Menu", "prepared menu response for player=%s clientId=%s", playerId, respClid)
}

func publishPresetStored(playerId string, presetNum int) {
	pb := getPlayerPlayback(playerId)
	pb.Mu.Lock()
	icon := pb.PlayingIcon
	title := pb.PlayingTitle
	stationName := pb.StationName
	stationDesc := pb.StationDesc
	pb.Mu.Unlock()

	presetTitle := stationName
	if presetTitle == "" {
		presetTitle = title
	}

	if icon == "" {
		icon = "/plugins/TuneIn/html/images/radiolocal.png"
	}

	textLines := []interface{}{
		"Preset Stored",
		fmt.Sprintf("Preset #%d: %s", presetNum, presetTitle),
	}
	if stationDesc != "" {
		textLines = append(textLines, stationDesc)
	}

	displayData := map[string]interface{}{
		"display": map[string]interface{}{
			"type":     "mixed",
			"duration": 2000,
			"style":    "favorite",
			"text":     textLines,
			"icon":     icon,
			"icon-id":  icon,
		},
	}

	streamingClientsMu.Lock()
	defer streamingClientsMu.Unlock()

	for clid, sc := range streamingClients {
		select {
		case sc.events <- []interface{}{
			map[string]interface{}{
				"channel": "/" + clid + "/slim/displaystatus/" + playerId,
				"data":    displayData,
			},
		}:
			LogInfo("DisplayStatus", "Published preset stored popup to client=%s player=%s: Preset #%d stored", clid, playerId, presetNum)
		default:
			// Avoid blocking
		}
	}
}

func publishStreamError(playerId string, conn net.Conn, err error) {
	errMsg := err.Error()
	if len(errMsg) > 80 {
		errMsg = errMsg[:80]
	}

	title := "Stream Error"
	if strings.Contains(errMsg, "no such host") || strings.Contains(errMsg, "lookup ") {
		title = "Station Offline"
		errMsg = "Radio station is not available"
	} else if strings.Contains(errMsg, "ICY") || strings.Contains(errMsg, "malformed HTTP") {
		title = "Stream Error"
		errMsg = "Server returned incompatible response"
	}

	if conn != nil {
		sendStrm(conn, 0, 0, title, errMsg, "", 5000)
	}

	displayData := map[string]interface{}{
		"display": map[string]interface{}{
			"type":     "error",
			"duration": 5000,
			"style":    "error",
			"text": []interface{}{
				title,
				errMsg,
			},
		},
	}

	streamingClientsMu.Lock()
	defer streamingClientsMu.Unlock()

	for clid, sc := range streamingClients {
		select {
		case sc.events <- []interface{}{
			map[string]interface{}{
				"channel": "/" + clid + "/slim/displaystatus/" + playerId,
				"data":    displayData,
			},
		}:
			LogInfo("DisplayStatus", "Published stream error popup to client=%s player=%s: %s - %s", clid, playerId, title, errMsg)
		default:
		}
	}
}

func publishPlayerStatus() {
	m := globalServerMetadata
	if m == nil {
		return
	}

	streamingClientsMu.Lock()
	clientsCopy := make(map[string]*StreamingClient, len(streamingClients))
	for k, v := range streamingClients {
		clientsCopy[k] = v
	}
	streamingClientsMu.Unlock()

	for clid, sc := range clientsCopy {
		players := getConnectedPlayers()
		for _, p := range players {
		pb := getPlayerPlayback(p.MAC)
		pb.Mu.Lock()
		playing := pb.Playing
		paused := pb.Paused
		title := pb.PlayingTitle
		desc := pb.PlayingDesc
		icon := pb.PlayingIcon
		urlStr := pb.PlayingURL
		stationName := pb.StationName
		pb.Mu.Unlock()

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
					"text":        stationName,
					"artwork_url": icon,
					"icon":        icon,
					"icon-id":     icon,
					"url":         urlStr,
					"duration":    0,
					"params": map[string]interface{}{
						"track_id": 0,
					},
				},
			}
		} else if paused {
			mode = "pause"
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
					"text":        stationName,
					"artwork_url": icon,
					"icon":        icon,
					"icon-id":     icon,
					"url":         urlStr,
					"duration":    0,
					"params": map[string]interface{}{
						"track_id": 0,
					},
				},
			}
		}

		statusData := map[string]interface{}{
			"player_name":        p.Name,
			"name":               p.Name,
			"model":              p.Model,
			"uuid":               p.UUID,
			"connected":          1,
			"player_connected":   1,
			"player_ip":          m.IPAddress,
			"mode":               mode,
			"power":              1,
			"time":               0,
			"rate":               1,
			"playlist_timestamp": time.Now().Unix(),
			"playlist_cur_index": 0,
			"playlist_tracks":    count,
			"count":              count,
			"offset":             0,
			"playlist shuffle":   0,
			"playlist repeat":    0,
			"remote":             remote,
			"current_title":      currentTitle,
		}
		if playing || paused {
			statusData["item_loop"] = itemLoop
			statusData["playlist_loop"] = itemLoop
		}

			select {
			case sc.events <- []interface{}{
				map[string]interface{}{
					"channel": "/" + clid + "/slim/playerstatus/" + p.MAC,
					"data":    statusData,
				},
			}:
				LogInfo("CometD", "Published playerstatus for client=%s player=%s (mode=%s)", clid, p.MAC, mode)
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
		LogInfo("Status", "no streaming client found for %s", respClid)
		return
	}

	players := getConnectedPlayers()
	playerName := ""
	playerModel := ""
	playerUUID := ""
	for _, p := range players {
		if p.MAC == playerId {
			playerName = p.Name
			playerModel = p.Model
			playerUUID = p.UUID
			break
		}
	}
	if playerName == "" {
		playerName = playerId
	}

	pb := getPlayerPlayback(playerId)
	pb.Mu.Lock()
	playing := pb.Playing
	paused := pb.Paused
	title := pb.PlayingTitle
	desc := pb.PlayingDesc
	icon := pb.PlayingIcon
	urlStr := pb.PlayingURL
	stationName := pb.StationName
	pb.Mu.Unlock()

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
				"track":       "",
				"artist":      desc,
				"album":       "",
				"text":        stationName,
				"artwork_url": icon,
				"icon":        icon,
				"icon-id":     icon,
				"url":         urlStr,
				"duration":    0,
				"params": map[string]interface{}{
					"track_id": 0,
				},
			},
		}
	} else if paused {
		mode = "pause"
		count = 1
		remote = 1
		currentTitle = title
		itemLoop = []interface{}{
			map[string]interface{}{
				"id":          0,
				"title":       title,
				"track":       "",
				"artist":      desc,
				"album":       "",
				"text":        stationName,
				"artwork_url": icon,
				"icon":        icon,
				"icon-id":     icon,
				"url":         urlStr,
				"duration":    0,
				"params": map[string]interface{}{
					"track_id": 0,
				},
			},
		}
	}

	data := map[string]interface{}{
		"player_name":        playerName,
		"name":               playerName,
		"model":              playerModel,
		"uuid":               playerUUID,
		"connected":          1,
		"player_connected":   1,
		"player_ip":          m.IPAddress,
		"mode":               mode,
		"power":              1,
		"time":               0,
		"rate":               1,
		"playlist_timestamp": time.Now().Unix(),
		"playlist_cur_index": 0,
		"playlist_tracks":    count,
		"count":              count,
		"offset":             0,
		"playlist shuffle":   0,
		"playlist repeat":    0,
		"remote":             remote,
		"current_title":      currentTitle,
	}
	if playing || paused {
		data["item_loop"] = itemLoop
		data["playlist_loop"] = itemLoop
	}

	sc.pendingMu.Lock()
	sc.pendingResponse = data
	sc.pendingMu.Unlock()
	LogInfo("Status", "prepared status response for player=%s clientId=%s (mode=%s)", playerId, respClid, mode)
}

func buildPlaylistResponse(respClid, playerId string, cmdArr []interface{}, m *ServerMetadata) {
	sc := getStreamingClient(respClid)
	if sc == nil {
		LogInfo("Playlist", "no streaming client found for %s", respClid)
		return
	}

	pb := getPlayerPlayback(playerId)
	pb.Mu.Lock()
	playing := pb.Playing
	title := pb.PlayingTitle
	desc := pb.PlayingDesc
	icon := pb.PlayingIcon
	urlStr := pb.PlayingURL
	stationName := pb.StationName
	pb.Mu.Unlock()

	var playlistLoop []interface{}
	if playing {
		playlistLoop = []interface{}{
			map[string]interface{}{
				"id":          0,
				"title":       title,
				"track":       "",
				"artist":      desc,
				"album":       "",
				"text":        stationName,
				"artwork_url": icon,
				"icon":        icon,
				"icon-id":     icon,
				"url":         urlStr,
				"duration":    0,
				"params": map[string]interface{}{
					"track_id": 0,
				},
			},
		}
	}

	data := map[string]interface{}{
		"playlist_loop": playlistLoop,
		"count":         len(playlistLoop),
		"offset":        0,
	}

	sc.pendingMu.Lock()
	sc.pendingResponse = data
	sc.pendingMu.Unlock()
	LogInfo("Playlist", "prepared playlist response for player=%s clientId=%s (count=%d)", playerId, respClid, len(playlistLoop))
}

// buildSlimRequestResponse returns the appropriate CometD response data for a
// /slim/request command. It returns cached data if buildMenuResponse or
// buildStatusResponse have prepared it, or a default empty response otherwise.
func buildSlimRequestResponse(respClid, playerId string) map[string]interface{} {
	sc := getStreamingClient(respClid)
	if sc == nil {
		LogInfo("RadioBrowser", "buildSlimRequestResponse: no streaming client for %s", respClid)
		return map[string]interface{}{"response": []interface{}{}}
	}

	sc.pendingMu.Lock()
	data := sc.pendingResponse
	sc.pendingResponse = nil
	sc.pendingMu.Unlock()

	if data != nil {
		LogInfo("RadioBrowser", "buildSlimRequestResponse: returning data with count=%v", data["count"])
		return data
	}
	LogInfo("RadioBrowser", "buildSlimRequestResponse: no pending data for %s", respClid)
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
		LogInfo("CometD", "no streaming client found for %s", respClid)
		if done != nil {
			done <- struct{}{}
		}
		return
	}

	go func() {
		time.Sleep(50 * time.Millisecond)

		data := buildServerStatusData(m)

		sc.events <- []interface{}{
			map[string]interface{}{
				"channel": "/" + respClid + "/slim/serverstatus",
				"id":      id,
				"data":    data,
			},
		}
		if done != nil {
			done <- struct{}{}
		}
	}()
}

// handleJsonRpc handles JSON-RPC protocol requests
func handleJsonRpc(w *minimalResponseWriter, r *minimalRequest, m *ServerMetadata) {
	var req JsonRpcRequest
	if err := json.Unmarshal(r.body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	LogInfo("JSON-RPC", "method=%s params=%v", req.Method, req.Params)

	switch req.Method {
	case "slim.subscribe":
		// params[0] is the channel path like "/{clientId}/slim/serverstatus"
		if len(req.Params) > 0 {
			if channel, ok := req.Params[0].(string); ok {
				clientId := extractClientID(channel)
				if clientId != "" {
					if strings.Contains(channel, "serverstatus") {
						pushServerStatus(clientId, req.Id, m, nil)
					}
				}
			}
		}
	}

	resp := JsonRpcResponse{Id: req.Id, Result: make(map[string]interface{})}
	w.Header()["Content-Type"] = "application/json"
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
	configFlag := flag.String("config", "", "Path to JSON config file for logging settings")
	flag.Parse()

	name := *nameFlag
	uuid := *uuidFlag
	jsonPort := *jsonPortFlag
	cliPort := *cliPortFlag
	version := *versionFlag
	configPath := *configFlag

	InitLogging(configPath)

	// Set local IP address to localhost for discovery
	ipAddress := "127.0.0.1"

	metadata := &ServerMetadata{
		IPAddress: ipAddress,
		Name:      name,
		UUID:      uuid,
		JSONPort:  jsonPort,
		CLIPort:   cliPort,
		Version:   version,
	}
	globalServerMetadata = metadata

	LogInfo("general", "==================================================")
	LogInfo("general", "            SlimProto Server (Go)")
	LogInfo("general", "==================================================")
	LogInfo("general", "  Name    : %s", metadata.Name)
	LogInfo("general", "  IP      : %s", metadata.IPAddress)
	LogInfo("general", "  UUID    : %s", metadata.UUID)
	LogInfo("general", "  JSON    : %s", metadata.JSONPort)
	LogInfo("general", "  Version : %s", metadata.Version)
	LogInfo("general", "--------------------------------------------------")

	// Start NTP background sync
	syncTimeWithNTP()

	// Start HTTP CometD server (required for client discovery)
	go startHTTPServer(metadata)

	// Start TCP SlimProto control server (port 3483)
	go startTCPServer(metadata)

	// Setup UDP discovery for network discovery (localhost only)
	localAddr := fmt.Sprintf("127.0.0.1:%d", DefaultPort)
	broadcastConn, err := listenUDP4(DefaultPort)
	if err != nil {
		LogWarn("general", "Warning: UDP discovery listener failed: %v", err)
	} else {
		defer broadcastConn.Close()
		LogInfo("general", "Listening for UDP discovery on %s...", localAddr)
		go startUDPListener(broadcastConn, broadcastConn, metadata, "Broadcast")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	LogInfo("general", "Shutting down. Goodbye!")
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
		Addr: [4]byte{0, 0, 0, 0},
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

func syncTimeWithNTP() {
	go func() {
		// Sync time on startup and then periodically
		for {
			err := performNTPSync()
			if err != nil {
				LogWarn("NTP", "time sync failed: %v, retrying in 30 seconds...", err)
				time.Sleep(30 * time.Second)
			} else {
				LogInfo("NTP", "time sync successful, system clock updated")
				time.Sleep(1 * time.Hour)
			}
		}
	}()
}

func performNTPSync() error {
	conn, err := net.Dial("udp", "pool.ntp.org:123")
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}

	req := make([]byte, 48)
	req[0] = 0x1B // LI = 0, VN = 3, Mode = 3 (client)

	_, err = conn.Write(req)
	if err != nil {
		return err
	}

	resp := make([]byte, 48)
	_, err = conn.Read(resp)
	if err != nil {
		return err
	}

	secBytes := resp[40:44]
	seconds := uint32(secBytes[0])<<24 | uint32(secBytes[1])<<16 | uint32(secBytes[2])<<8 | uint32(secBytes[3])

	const ntpEpochOffset = 2208988800
	if seconds < ntpEpochOffset {
		return fmt.Errorf("invalid NTP timestamp: %d", seconds)
	}

	unixSecs := int64(seconds - ntpEpochOffset)

	tv := syscall.NsecToTimeval(unixSecs * 1e9)
	if err := syscall.Settimeofday(&tv); err != nil {
		return fmt.Errorf("settimeofday failed: %v", err)
	}

	return nil
}

type trackingReader struct {
	r     io.ReadCloser
	count int64
}

func (tr *trackingReader) Read(p []byte) (int, error) {
	n, err := tr.r.Read(p)
	tr.count += int64(n)
	return n, err
}

func (tr *trackingReader) Close() error {
	return tr.r.Close()
}

func resumeUpstream(streamURL string, offset int64) (io.ReadCloser, int, error) {
	req, err := http.NewRequest("GET", streamURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Icy-MetaData", "1")
	req.Header.Set("User-Agent", "SlimProto/1.0")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))

	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}

	if resp.StatusCode == http.StatusPartialContent {
		icyMetaInt := 0
		if icyMetaIntStr := resp.Header.Get("icy-metaint"); icyMetaIntStr != "" {
			if mi, err := strconv.Atoi(icyMetaIntStr); err == nil {
				icyMetaInt = mi
			}
		}
		return resp.Body, icyMetaInt, nil
	}

	resp.Body.Close()
	return nil, 0, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
}
