# SlimProto Server-Side Discovery Protocol Research

This document provides a comprehensive breakdown of the Squeezebox / SlimProto discovery protocol used by players (clients) to automatically locate a music server (like Lyrion Music Server or custom SlimProto implementations) on a local network.

---

## 1. Protocol Architecture & Network Topology

SlimProto discovery operates on a **UDP** broadcast/multicast model. Once a client discovers the server's IP address and control ports via UDP, it establishes a persistent **TCP** connection on port `3483` to begin the main SlimProto control flow (which starts with a `HELO` packet).

### Key Network Parameters
*   **Port:** `3483` (both TCP and UDP).
*   **Multicast Address:** `239.255.255.250` (SSDP multicast group, used by some modern controllers/players).
*   **Broadcast Address:** `255.255.255.255` (standard UDP broadcast, used by Squeezelite and hardware players).

---

## 2. Discovery Packet Protocols

There are two discovery mechanisms supported by the SlimProto server ecosystem: **Legacy SLIMP3 Discovery** (using fixed-width packets) and **Modern TLV Discovery** (using Type-Length-Value tags).

### A. Legacy Discovery Protocol
This protocol is a legacy artifact of the original SLIMP3 hardware and early Squeezebox models. It is still supported for backward compatibility and is used by players that do not need detailed server metadata.

#### Client Request Payload (`'d'`)
The client broadcasts an **18-byte** packet containing its hardware information.

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|    'd' (0x64) |   Reserved    |   Device ID   | Firmware Rev. |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                           Reserved                            |
+                                                               +
|                                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                      Client MAC Address (6 bytes)             |
|                               +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

*   **Byte 0:** ASCII Character `'d'` (`0x64`), indicating a legacy discovery request.
*   **Byte 1:** Pad/Reserved byte.
*   **Byte 2:** Device ID (unsigned 8-bit integer). E.g., `2` for Squeezebox, `8` for Squeezeslave.
*   **Byte 3:** Firmware revision.
*   **Bytes 4–11:** 8 reserved padding bytes.
*   **Bytes 12–17:** Client MAC Address (6 bytes).

#### Server Response Payload (`'D'`)
The server responds to the client's source address with a unicast **17-byte** packet.

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|    'D' (0x44) |                                               |
+-+-+-+-+-+-+-+-+                                               +
|                                                               |
+                   Server IP Address String (16 bytes)         +
|                                                               |
+                               +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

*   **Byte 0:** ASCII Character `'D'` (`0x44`), indicating a legacy discovery response.
*   **Bytes 1–16:** Server IP Address represented as a human-readable ASCII string (e.g., `"192.168.1.100"`), padded with NUL bytes (`\x00`) to exactly 16 bytes.
    *   *Note:* Historically, some documentations indicated bytes 2-5 carried the binary IP address and bytes 6-7 carried the port. However, modern implementations like `aioslimproto` and `squeezelite` use/expect the 16-byte IP address string.

---

### B. Modern TLV Discovery Protocol
The modern discovery protocol allows clients to query specific server parameters and metadata (such as server name, version, and Web/JSON-RPC/CLI API ports) using Type-Length-Value structures.

#### Client Request Payload (`'e'`)
The client broadcasts a packet beginning with `'e'` (`0x65`), followed by a list of empty TLV tags requesting specific parameters.

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|    'e' (0x65) |             TLV Tag 1 (4-byte ASCII)          |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|               |  Length (0x00)|             TLV Tag 2 ...     |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

*   **Byte 0:** ASCII Character `'e'` (`0x65`), indicating a TLV discovery request.
*   **TLV Blocks:** Each block consists of:
    *   **Type (4 bytes):** ASCII tag (e.g., `"NAME"`, `"IPAD"`).
    *   **Length (1 byte):** Unsigned length of the value. For request datagrams, this is typically `0x00` since the client is only querying the server.
    *   **Value (0–255 bytes):** Only present if `Length` > 0.

#### Server Response Payload (`'E'`)
The server replies to the client's source address with a unicast packet starting with `'E'` (`0x45`), containing the populated TLV data for the tags requested by the client.

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|    'E' (0x45) |             TLV Tag 1 (4-byte ASCII)          |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|               | Value Length  |          Value Bytes          |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

*   **Byte 0:** ASCII Character `'E'` (`0x45`), indicating a TLV discovery response.
*   **TLV Blocks:** Populated with the requested parameters.
    *   **Type (4 bytes):** E.g., `"NAME"`, `"IPAD"`, `"JSON"`, etc.
    *   **Length (1 byte):** The length $L$ of the value string.
    *   **Value ($L$ bytes):** The string representation of the parameter.

#### Supported TLV Tags
The server should resolve and populate the following tags if requested:

| Tag | Description | Example Response Value |
| :--- | :--- | :--- |
| **`NAME`** | Server Hostname/Friendly Name | `"My Music Server"` |
| **`IPAD`** | Server Local IP Address | `"192.168.1.100"` |
| **`JSON`** | Server JSON-RPC HTTP port | `"9000"` (returned as string) |
| **`CLIP`** | Server CLI port | `"9090"` (returned as string) |
| **`VERS`** | Server software version | `"7.999.999"` |
| **`UUID`** | Unique Identifier for the server | `"slimproto-uuid-1234"` |

---

## 3. Server-Side Reference Implementations

Below are reference implementations for handling SlimProto server-side discovery.

### Python Asyncio Implementation
This script launches an asynchronous UDP server binding to `0.0.0.0:3483` and joining the multicast group `239.255.255.250`. It handles both legacy (`'d'`) and TLV (`'e'`) requests.

```python
import asyncio
import socket
import struct
from collections import OrderedDict

class SlimProtoDiscoveryProtocol(asyncio.DatagramProtocol):
    def __init__(self, ip_address, name="SlimServer", uuid="slimproto-uuid", json_port=9000, cli_port=9090):
        self.ip_address = ip_address
        self.name = name
        self.uuid = uuid
        self.json_port = str(json_port)
        self.cli_port = str(cli_port)
        self.transport = None

    def connection_made(self, transport):
        self.transport = transport
        sock = self.transport.get_extra_info("socket")
        # Join multicast group
        mgroup = socket.inet_aton("239.255.255.250")
        mreq = struct.pack("4sL", mgroup, socket.INADDR_ANY)
        sock.setsockopt(socket.IPPROTO_IP, socket.IP_ADD_MEMBERSHIP, mreq)
        print("SlimProto UDP discovery listener running...")

    def datagram_received(self, data: bytes, addr):
        if not data:
            return

        try:
            # 1. TLV Discovery Request (starts with 'e')
            if data.startswith(b"e"):
                payload = data[1:].decode("utf-8", errors="ignore")
                requested_tags = self.parse_tlv_tags(payload)
                response_data = self.build_tlv_response(requested_tags)
                self.send_tlv_response(response_data, addr)
                print(f"Sent TLV discovery response to {addr}")

            # 2. Legacy Discovery Request (starts with 'd')
            elif data.startswith(b"d"):
                self.send_legacy_response(addr)
                print(f"Sent legacy discovery response to {addr}")
                
        except Exception as e:
            print(f"Error parsing discovery datagram: {e}")

    def parse_tlv_tags(self, data: str) -> list[str]:
        tags = []
        idx = 0
        while idx <= len(data) - 5:
            key = data[idx : idx + 4]
            length = ord(data[idx + 4])
            tags.append(key)
            idx += 5 + length
        return tags

    def build_tlv_response(self, requested_tags) -> OrderedDict:
        response = OrderedDict()
        for tag in requested_tags:
            if tag == "NAME":
                response[tag] = self.name
            elif tag == "IPAD":
                response[tag] = self.ip_address
            elif tag == "JSON":
                response[tag] = self.json_port
            elif tag == "CLIP":
                response[tag] = self.cli_port
            elif tag == "VERS":
                response[tag] = "7.999.999"
            elif tag == "UUID":
                response[tag] = self.uuid
        return response

    def send_tlv_response(self, response_data, addr):
        parts = [b"E"]
        for key, val in response_data.items():
            val_bytes = val.encode("utf-8")[:255]
            parts.append(key.encode("utf-8"))
            parts.append(bytes([len(val_bytes)]))
            parts.append(val_bytes)
        self.transport.sendto(b"".join(parts), addr)

    def send_legacy_response(self, addr):
        # Truncated & padded to 16 bytes
        ip_bytes = self.ip_address[:16].encode("ascii")
        ip_bytes += b"\x00" * (16 - len(ip_bytes))
        dgram = struct.pack("!c16s", b"D", ip_bytes)
        self.transport.sendto(dgram, addr)

async def main():
    loop = asyncio.get_running_loop()
    # Replace with server's local network interface IP
    server_ip = "192.168.1.100" 
    
    transport, protocol = await loop.create_datagram_endpoint(
        lambda: SlimProtoDiscoveryProtocol(server_ip),
        local_addr=("0.0.0.0", 3483)
    )
    
    try:
        await asyncio.sleep(3600)  # Keep running
    finally:
        transport.close()

if __name__ == "__main__":
    asyncio.run(main())
```

---

### Go Implementation
A native Go server using the `net` package to listen on both broadcast and multicast addresses.

```go
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
)

const (
	Port           = ":3483"
	MulticastGroup = "239.255.255.250"
)

type ServerMetadata struct {
	IPAddress string
	Name      string
	UUID      string
	JSONPort  string
	CLIPort   string
}

func main() {
	metadata := ServerMetadata{
		IPAddress: "192.168.1.100", // Replace with actual interface IP
		Name:      "GoSlimServer",
		UUID:      "goslimproto-uuid",
		JSONPort:  "9000",
		CLIPort:   "9090",
	}

	addr, err := net.ResolveUDPAddr("udp", "0.0.0.0"+Port)
	if err != nil {
		panic(err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	// Join multicast group on default interface
	multicastIP := net.ParseIP(MulticastGroup)
	if err := joinMulticast(conn, multicastIP); err != nil {
		fmt.Printf("Warning: Failed to join multicast group: %v\n", err)
	}

	fmt.Println("SlimProto Go UDP discovery listener running...")
	buf := make([]byte, 1024)

	for {
		n, raddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Printf("Read error: %v\n", err)
			continue
		}

		if n == 0 {
			continue
		}

		switch buf[0] {
		case 'e':
			// TLV Request
			tags := parseTLVs(buf[1:n])
			resp := buildTLVResponse(tags, &metadata)
			_, _ = conn.WriteToUDP(resp, raddr)
			fmt.Printf("Sent TLV response to %v\n", raddr)
		case 'd':
			// Legacy Request
			resp := buildLegacyResponse(&metadata)
			_, _ = conn.WriteToUDP(resp, raddr)
			fmt.Printf("Sent Legacy response to %v\n", raddr)
		}
	}
}

func joinMulticast(conn *net.UDPConn, group net.IP) error {
	// Simple join on the interface
	return nil // Interface specific configurations vary by platform
}

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

func buildTLVResponse(tags []string, m *ServerMetadata) []byte {
	resp := []byte{'E'}
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
			val = "7.999.999"
		case "UUID":
			val = m.UUID
		}

		if val != "" {
			resp = append(resp, []byte(tag)...)
			valLen := len(val)
			if valLen > 255 {
				valLen = 255
			}
			resp = append(resp, byte(valLen))
			resp = append(resp, []byte(val[:valLen])...)
		}
	}
	return resp
}

func buildLegacyResponse(m *ServerMetadata) []byte {
	resp := make([]byte, 17)
	resp[0] = 'D'
	ipStr := m.IPAddress
	if len(ipStr) > 16 {
		ipStr = ipStr[:16]
	}
	copy(resp[1:], []byte(ipStr))
	// Pad remaining bytes with 0
	for i := 1 + len(ipStr); i < 17; i++ {
		resp[i] = 0
	}
	return resp
}
```

---

## 4. Key Implementation Guidelines

1.  **Robust Character Decoding:** When parsing incoming UDP discovery datagrams, **never** decode the raw payload as UTF-8 directly before checking the prefix. SliMP3 players send binary bytes in the `'d'` legacy request which will crash standard `.decode("utf-8")` methods. Perform prefix checks on raw bytes (`b'e'` or `b'd'`) and safely catch any decode exceptions.
2.  **IP Binding:** Bind the socket to `0.0.0.0` to receive both local broadcasts (`255.255.255.255`) and multicast packets, but ensure the response payload contains the server's specific local IP interface address rather than `0.0.0.0`.
3.  **Port Sensitivity:** Squeezebox players operate strictly on UDP port `3483` for discovery. If your server is running in a Docker container, you **must** expose port `3483` for both TCP and UDP.
