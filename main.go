package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
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
	Channel    string                 `json:"channel"`
	ClientId   string                 `json:"clientId,omitempty"`
	Id         interface{}            `json:"id,omitempty"`
	Successful bool                   `json:"successful"`
	Advice     map[string]interface{} `json:"advice,omitempty"`
	Data       map[string]interface{} `json:"data,omitempty"`
}

type CometdResponse struct {
	Channel    string                 `json:"channel"`
	Successful bool                   `json:"successful"`
	Id         interface{}            `json:"id,omitempty"`
	ClientId   string                 `json:"clientId,omitempty"`
	Data       map[string]interface{} `json:"data,omitempty"`
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

	heloReceived := false
	strm28Sent := false

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

			if err := sendVers(conn, m.Version); err != nil {
				fmt.Printf("[TCP] %s error sending vers: %v\n", remote, err)
				return
			}

			if err := sendSetdName(conn, m.Name); err != nil {
				fmt.Printf("[TCP] %s error sending setd name: %v\n", remote, err)
				return
			}

			fmt.Printf("[TCP] %s HELO complete, connection established\n", remote)

			// Immediately send strm #28 after HELO - critical for client compatibility
			if !strm28Sent {
				strm28Sent = true
				if err := sendStrm(conn, 's', 28, "Track 28", "Artist 28", "Album 28", 300); err != nil {
					fmt.Printf("[TCP] %s error sending strm #28: %v\n", remote, err)
				} else {
					fmt.Printf("[TCP] %s strm #28 sent to client\n", remote)
				}
			}

		case "STAT":
			if !heloReceived {
				fmt.Printf("[TCP] %s STAT before HELO, ignoring\n", remote)
				continue
			}
			event := ""
			if len(payload) >= 4 {
				event = strings.TrimRight(string(payload[0:4]), "\x00")
			}
			fmt.Printf("[TCP] %s STAT event=%q\n", remote, event)

		case "BYE!":
			fmt.Printf("[TCP] %s BYE! received\n", remote)
			return

		case "DSCO":
			reason := uint8(0)
			if len(payload) >= 1 {
				reason = payload[0]
			}
			fmt.Printf("[TCP] %s DSCO reason=%d\n", remote, reason)
			return

		default:
			fmt.Printf("[TCP] %s unknown opcode %q (%d bytes)\n", remote, opcode, len(payload))
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

// startHTTPServer starts the HTTP CometD server (for JSON-RPC and CometD
func startHTTPServer(m *ServerMetadata) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cometd", func(w http.ResponseWriter, r *http.Request) {
		handleCometd(w, r, m)
	})
	mux.HandleFunc("/jsonrpc", func(w http.ResponseWriter, r *http.Request) {
		handleJsonRpc(w, r, m)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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

	for i, req := range reqs {
		fmt.Printf("[CometD] <- req[%d] channel=%q clientId=%q\n", i, req.Channel, req.ClientId)
	}

	resps := make([]CometdRequest, 0, len(reqs))
	for _, req := range reqs {
		resp := cometdResponse(req, m)
		resps = append(resps, resp)
	}
	for i, resp := range resps {
		fmt.Printf("[CometD] -> resp[%d] channel=%q clientId=%q\n", i, resp.Channel, resp.ClientId)
	}

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

func cometdResponse(req CometdRequest, m *ServerMetadata) CometdRequest {
	switch req.Channel {
	case "/meta/handshake":
		return CometdRequest{
			Channel:    "/meta/handshake",
			Successful: true,
			Id:         req.Id,
			ClientId:   "go-cometd-client-id",
			Advice: map[string]interface{}{
				"reconnect": "retry",
				"interval":  0,
			},
			Data: map[string]interface{}{
				"version":    "1.0",
				"minimumVersion": "1.0",
				"supportedConnectionTypes": []string{"streaming"},
			},
		}

	case "/meta/connect":
		return CometdRequest{
			Channel:    "/meta/connect",
			Successful: true,
			Id:         req.Id,
			ClientId:   req.ClientId,
			Advice: map[string]interface{}{
				"reconnect": "retry",
				"interval":  0,
			},
			Data: map[string]interface{}{
				"reconnect": "retry",
				"interval":  0,
			},
		}

	case "/meta/subscribe":
		return CometdRequest{
			Channel:    "/meta/subscribe",
			Successful: true,
			Id:         req.Id,
			ClientId:   req.ClientId,
		}

	case "/meta/disconnect":
		return CometdRequest{
			Channel:    "/meta/disconnect",
			Successful: true,
			Id:         req.Id,
			ClientId:   req.ClientId,
		}

	case "/slim/serverstatus":
		return CometdRequest{
			Channel:    "/slim/serverstatus",
			Successful: true,
			Id:         req.Id,
			ClientId:   req.ClientId,
			Data: map[string]interface{}{
				"version":  "9.0",
				"uuid":     m.UUID,
				"name":     m.Name,
				"ip":       m.IPAddress,
				"port":     m.JSONPort,
				"cliport":  m.CLIPort,
				"compatible": true,
			},
		}

	default:
		return CometdRequest{
			Channel:    req.Channel,
			Successful: true,
			Id:         req.Id,
			ClientId:   req.ClientId,
		}
	}
}

// handleJsonRpc handles JSON-RPC protocol requests
func handleJsonRpc(w http.ResponseWriter, r *http.Request, m *ServerMetadata) {
	var req JsonRpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	fmt.Printf("[JSON-RPC] method=%s params=%v\n", req.Method, req.Params)
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