package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	DefaultPort    = 3483
	MulticastGroup = "239.255.255.250"
)

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
	events   chan []interface{}
	clientId string
}

var (
	streamingClients   = make(map[string]*StreamingClient)
	streamingClientsMu sync.Mutex

	connectedPlayers   = make(map[string]PlayerInfo)
	connectedPlayersMu sync.RWMutex

	slimConn   net.Conn
	slimConnMu sync.Mutex
)

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
		events:   make(chan []interface{}, 100),
		clientId: clientId,
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
// audio data in the format specified by mode (e.g. 'm' = MP3/mad).
func sendStrmCommand(conn net.Conn, serverPort uint16) error {
	payload := make([]byte, 28)
	payload[0] = 's'                     // command: start
	payload[1] = '1'                     // autostart (ASCII '1' = 0x31)
	payload[2] = 'm'                     // mode: MP3 (mad decoder)
	payload[3] = 0                       // pcmSampleSize
	payload[4] = 0                       // pcmSampleRate
	payload[5] = 0                       // pcmChannels
	payload[6] = 0                       // pcmEndianness
	payload[7] = 0                       // threshold
	payload[8] = 0                       // spdifEnable
	payload[9] = 0                       // transitionPeriod (1 byte)
	payload[10] = 0                      // transitionType
	payload[11] = 0                      // flags
	payload[12] = 0                      // outputThreshold
	payload[13] = 0                      // slaves
	// bytes 14-17: replayGain = 0 (4 bytes)
	// bytes 18-19: serverPort (2 bytes BE)
	binary.BigEndian.PutUint16(payload[18:20], serverPort)
	// bytes 20-23: serverIp = 0 (use current server IP)
	// bytes 24-27: reserved
	return sendPacket(conn, "strm", payload)
}

// sendAudg sends an audio gain packet setting volume to max.
func sendAudg(conn net.Conn) error {
	payload := make([]byte, 8)
	// The player expects the gain pre-shifted left by 9.
	// LMS clamps 0-100 then sends (value << 9); 100 << 9 = 51200 is max.
	binary.BigEndian.PutUint32(payload[0:4], 51200) // gainL = max
	binary.BigEndian.PutUint32(payload[4:8], 51200) // gainR = max
	return sendPacket(conn, "audg", payload)
}

// sendStrmKeepalive sends a strm timestamp/keepalive packet.
func sendStrmKeepalive(conn net.Conn) error {
	payload := make([]byte, 28)
	payload[0] = 't' // timestamp command (keepalive)
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

// handleStreamStart starts streaming a URL or local test file to the SqueezePlay player.
func handleStreamStart(w http.ResponseWriter, r *http.Request) {
	streamURL := r.URL.Query().Get("url")
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

	// Set max volume before streaming
	if err := sendAudg(conn); err != nil {
		fmt.Printf("[Stream] sendAudg error: %v\n", err)
	}
	// Enable audio output
	if err := sendAude(conn, true); err != nil {
		fmt.Printf("[Stream] sendAude error: %v\n", err)
	}

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Printf("[Stream] listen error: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)

	if streamURL != "" {
		fmt.Printf("[Stream] listening on port %d for URL: %s\n", port, streamURL)
	} else {
		fmt.Printf("[Stream] listening on port %d for file: %s\n", port, streamFile)
	}

	if err := sendStrmCommand(conn, port); err != nil {
		listener.Close()
		fmt.Printf("[Stream] sendStrmCommand error: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Start keepalive goroutine (strm 't' every ~4s)
	keepaliveStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := sendStrmKeepalive(conn); err != nil {
					return
				}
			case <-keepaliveStop:
				return
			}
		}
	}()

	go func() {
		defer listener.Close()
		defer close(keepaliveStop)

		playerConn, err := listener.Accept()
		if err != nil {
			fmt.Printf("[Stream] accept error: %v\n", err)
			return
		}
		defer playerConn.Close()

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
			resp, err := http.Get(streamURL)
			if err != nil {
				fmt.Printf("[Stream] http get error: %v\n", err)
				return
			}
			defer resp.Body.Close()
			reader = resp.Body
			fmt.Printf("[Stream] player connected, proxying %s\n", streamURL)
			fmt.Printf("[Stream] HTTP status=%s, content-type=%s\n", resp.Status, resp.Header.Get("Content-Type"))
			for k, v := range resp.Header {
				fmt.Printf("[Stream]   %s: %s\n", k, v)
			}
		}

		totalWritten := int64(0)
		lastLog := time.Now()
		buf := make([]byte, 65536)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
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

		case "BYE!":
			fmt.Printf("[TCP] %s BYE! received\n", remote)
			if clientMAC != "" {
				removeConnectedPlayer(clientMAC)
				pushServerstatusToAll(m)
			}
			return

		case "DSCO":
			reason := uint8(0)
			if len(payload) >= 1 {
				reason = payload[0]
			}
			fmt.Printf("[TCP] %s DSCO reason=%d\n", remote, reason)
			if clientMAC != "" {
				removeConnectedPlayer(clientMAC)
				pushServerstatusToAll(m)
			}
			return

		default:
			fmt.Printf("[TCP] %s unknown opcode %q (%d bytes)\n", remote, opcode, len(payload))
		}
	}

	// Connection lost — clean up player if not already removed via BYE!/DSCO
	if clientMAC != "" {
		removeConnectedPlayer(clientMAC)
		pushServerstatusToAll(m)
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
	listener, err := net.Listen("tcp", addr)
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

// startHTTPServer starts the HTTP CometD server (for JSON-RPC and CometD
func startHTTPServer(m *ServerMetadata) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/play", handleStreamStart)
	mux.HandleFunc("/cometd", func(w http.ResponseWriter, r *http.Request) {
		handleCometd(w, r, m)
	})
	mux.HandleFunc("/jsonrpc", func(w http.ResponseWriter, r *http.Request) {
		handleJsonRpc(w, r, m)
	})
	mux.HandleFunc("/music/", handleArtwork)
	mux.HandleFunc("/contributor/", handleArtwork)
	mux.HandleFunc("/imageproxy/", handleArtwork)
	mux.HandleFunc("/html/", handleArtwork)
	mux.HandleFunc("/plugins/", handleArtwork)
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
	if err := http.ListenAndServe(":"+m.JSONPort, mux); err != nil {
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
		case <-time.After(180 * time.Second):
			fmt.Printf("[CometD] streaming timeout for clientId=%s\n", clientId)
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
		if req.Data != nil {
			fmt.Printf("[CometD] /slim/request data: %v\n", req.Data)
			if respPath, ok := req.Data["response"].(string); ok {
				if matches := regexp.MustCompile(`/([0-9a-f]{8})/`).FindStringSubmatch(respPath); len(matches) > 1 {
					respClid = matches[1]
				}
			}
		}
		if respClid == "" {
			respClid = clid
		}

		// Check the request command to determine response format
		isMenuRequest := false
		if req.Data != nil {
			if requestArr, ok := req.Data["request"].([]interface{}); ok && len(requestArr) > 0 {
				if cmd, ok := requestArr[0].(string); ok && cmd == "menu" {
					isMenuRequest = true
				}
			}
		}

		respData := map[string]interface{}{
			"response": []interface{}{},
		}
		if isMenuRequest {
			respData = map[string]interface{}{
				"item_loop": []interface{}{},
			}
		}

		return CometdRequest{
			Channel:    "/slim/request",
			Successful: true,
			Id:         req.Id,
			ClientId:   respClid,
			Data:       respData,
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

	// Setup UDP discovery for network discovery
	localAddr := fmt.Sprintf("0.0.0.0:%d", DefaultPort)
	udpAddr, err := net.ResolveUDPAddr("udp4", localAddr)
	if err != nil {
		panic(err)
	}
	broadcastConn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		panic(err)
	}
	defer broadcastConn.Close()
	fmt.Printf("Listening for UDP discovery on %s...\n", localAddr)

	// Start UDP broadcast listener
	go startUDPListener(broadcastConn, broadcastConn, metadata, "Broadcast")

	// Start UDP multicast listener
	mcastAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", MulticastGroup, DefaultPort))
	if err == nil {
		if mcastConn, err := net.ListenMulticastUDP("udp4", nil, mcastAddr); err == nil {
			defer mcastConn.Close()
			fmt.Printf("Listening for UDP multicast on %s:%d...\n", MulticastGroup, DefaultPort)
			go startUDPListener(mcastConn, broadcastConn, metadata, "Multicast")
		} else {
			fmt.Printf("Warning: multicast listener failed: %v\n", err)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nShutting down. Goodbye!")
}
