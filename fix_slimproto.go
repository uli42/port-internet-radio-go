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
	Channel                  string                 `json:"channel"`
	Version                  string                 `json:"version,omitempty"`
	MinimumVersion           string                 `json:"minimumVersion,omitempty"`
	ClientId                 string                 `json:"clientId,omitempty"`
	ConnectionType           string                 `json:"connectionType,omitempty"`
	Subscription             string                 `json:"subscription,omitempty"`
	Id                       interface{}            `json:"id,omitempty"`
	SupportedConnectionTypes []string               `json:"supportedConnectionTypes,omitempty"`
	Ext                      map[string]interface{} `json:"ext,omitempty"`
	Data                     map[string]interface{} `json:"data,omitempty"`
}

type CometdResponse struct {
	Channel                  string                 `json:"channel"`
	Version                  string                 `json:"version,omitempty"`
	SupportedConnectionTypes []string               `json:"supportedConnectionTypes,omitempty"`
	ClientId                 string                 `json:"clientId,omitempty"`
	Subscription             string                 `json:"subscription,omitempty"`
	Successful               bool                   `json:"successful"`
	Id                       interface{}            `json:"id,omitempty"`
	Error                    string                 `json:"error,omitempty"`
	Advice                   map[string]interface{} `json:"advice,omitempty"`
	Ext                      map[string]interface{} `json:"ext,omitempty"`
	Data                     map[string]interface{} `json:"data,omitempty"`
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

func sendPacket(w io.Writer, opcode string, payload []byte) error {
	packetLen := 4 + len(payload)
	buf := make([]byte, 2+packetLen)
	binary.BigEndian.PutUint16(buf[0:2], uint16(packetLen))
	copy(buf[2:6], []byte(opcode))
	copy(buf[6:], payload)
	_, err := w.Write(buf)
	return err
}

func sendVers(conn net.Conn, version string) error {
	return sendPacket(conn, "vers", []byte(version))
}

func sendSetdName(conn net.Conn, name string) error {
	payload := make([]byte, 1+len(name))
	payload[0] = 0
	copy(payload[1:], []byte(name))
	return sendPacket(conn, "setd", payload)
}

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

			// Immediately send strm #28 after HELO
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

	go startUDPListener(broadcastConn, broadcastConn, metadata, "Broadcast")

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
