package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const (
	DefaultPort = 3483
)

type ServerMetadata struct {
	IPAddress string
	Name      string
	UUID      string
	Version   string
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
			fmt.Printf("[TCP] HELO from %s received\n", remote)
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
				go func() {
					if err := sendStrm(conn, 's', 28, "Track 28", "Artist 28", "Album 28", 300); err != nil {
						fmt.Printf("[TCP] %s error sending strm #28: %v\n", remote, err)
					} else {
						fmt.Printf("[TCP] %s strm #28 sent to client\n", remote)
					}
				}()
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

func main() {
	portFlag := flag.Int("port", DefaultPort, "UDP/TCP Port for SlimProto")
	nameFlag := flag.String("name", "GoSlimServer", "Friendly name of the server")
	uuidFlag := flag.String("uuid", "goslimserver-uuid-1234", "UUID of the server")
	versionFlag := flag.String("version", "9.0", "Server version string")
	flag.Parse()

	metadata := &ServerMetadata{
		Name:    *nameFlag,
		UUID:    *uuidFlag,
		Version: *versionFlag,
	}

	fmt.Println("==================================================")
	fmt.Println("            SlimProto Server (Go)")
	fmt.Println("==================================================")
	fmt.Printf("  Name    : %s\n", metadata.Name)
	fmt.Printf("  UUID    : %s\n", metadata.UUID)
	fmt.Printf("  Version : %s\n", metadata.Version)
	fmt.Println("--------------------------------------------------")

	_ = portFlag // Fixed port is handled by DefaultPort constant above

	go startTCPServer(metadata)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nShutting down. Goodbye!")
}