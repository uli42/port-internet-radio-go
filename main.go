package main

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
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
func handleButtonPress(btnName string) {
	slimConnMu.Lock()
	conn := slimConn
	slimConnMu.Unlock()
	if conn == nil {
		fmt.Printf("[Button] no SlimProto connection\n")
		return
	}
	switch btnName {
	case "preset_1.single":
		fmt.Printf("[Button] preset 1 → SWR3 stream\n")
		go runURLStream(conn, "https://liveradio.swr.de/sw282p3/swr3/")
	case "preset_2.single":
		fmt.Printf("[Button] preset 2 → PCM test\n")
		go runPCMTest(conn)
	default:
		fmt.Printf("[Button] unhandled button: %s\n", btnName)
	}
}

// runURLStream starts streaming the given URL to the player.
// Runs entirely in a new goroutine (called from handleButtonPress and direct HTTP).
func runURLStream(conn net.Conn, streamURL string) {
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
	listener, err := net.Listen("tcp4", ":0")
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
		reader := result.reader
		fmt.Printf("[Stream] t=%s upstream ready, proxying %s\n", time.Since(streamStartTime), streamURL)

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

	listener, err := net.Listen("tcp4", ":0")
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
	err         error
}

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
	listener, err := net.Listen("tcp4", ":0")
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

	listener, err := net.Listen("tcp4", ":0")
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
	listener, err := net.Listen("tcp4", addr)
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
	hl, err := net.Listen("tcp4", ":"+m.JSONPort)
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
		respChannel := "/slim/request"
		if req.Data != nil {
			fmt.Printf("[CometD] /slim/request data: %v\n", req.Data)
			if respPath, ok := req.Data["response"].(string); ok {
				respChannel = respPath
				if matches := regexp.MustCompile(`/([0-9a-f]{8})/`).FindStringSubmatch(respPath); len(matches) > 1 {
					respClid = matches[1]
				}
			}
		}
		if respClid == "" {
			respClid = clid
		}

		// Check the request command to determine response format.
		// Request format: [playerId, [command, args...]]
		isMenuRequest := false
		if req.Data != nil {
			if requestArr, ok := req.Data["request"].([]interface{}); ok && len(requestArr) >= 2 {
				if cmdArr, ok := requestArr[1].([]interface{}); ok && len(cmdArr) > 0 {
					if cmd, ok := cmdArr[0].(string); ok && cmd == "menu" {
						isMenuRequest = true
					}
				}
			}
		}

		// Handle button commands from SqueezePlay (preset button presses).
		// The request format is: [MAC, ["button", "preset_N.single"]]
		if req.Data != nil {
			if requestArr, ok := req.Data["request"].([]interface{}); ok && len(requestArr) >= 2 {
				if cmdArr, ok := requestArr[1].([]interface{}); ok && len(cmdArr) >= 2 {
					if cmd, ok := cmdArr[0].(string); ok && cmd == "button" {
						if btnName, ok := cmdArr[1].(string); ok {
							fmt.Printf("[Button] CometD button: %s\n", btnName)
							go handleButtonPress(btnName)
						}
					}
				}
			}
		}

		respData := map[string]interface{}{
			"response": []interface{}{},
		}
		if isMenuRequest {
			respData = map[string]interface{}{
				"item_loop": []interface{}{
					map[string]interface{}{
						"id":   "nowPlaying",
						"text": "Now Playing",
						"actions": map[string]interface{}{
							"go": map[string]interface{}{
								"cmd": []interface{}{"applet", "NowPlaying", "play"},
							},
						},
						"window": map[string]interface{}{
							"windowId": "nowPlaying",
						},
					},
				},
			}
		}

		return CometdRequest{
			Channel:    respChannel,
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
		fmt.Printf("Warning: UDP discovery address error: %v\n", err)
	} else {
		broadcastConn, err := net.ListenUDP("udp4", udpAddr)
		if err != nil {
			fmt.Printf("Warning: UDP discovery listener failed: %v\n", err)
		} else {
			defer broadcastConn.Close()
			fmt.Printf("Listening for UDP discovery on %s...\n", localAddr)
			go startUDPListener(broadcastConn, broadcastConn, metadata, "Broadcast")
		}
	}

	// Start UDP multicast listener
	mcastAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", MulticastGroup, DefaultPort))
	if err == nil {
		if mcastConn, err := net.ListenMulticastUDP("udp4", nil, mcastAddr); err == nil {
			defer mcastConn.Close()
			fmt.Printf("Listening for UDP multicast on %s:%d...\n", MulticastGroup, DefaultPort)
			go startUDPListener(mcastConn, mcastConn, metadata, "Multicast")
		} else {
			fmt.Printf("Warning: multicast listener failed: %v\n", err)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nShutting down. Goodbye!")
}
