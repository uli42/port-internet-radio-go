package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// minimalRequest is a lightweight HTTP request parser
type minimalRequest struct {
	method    string
	path      string
	query     map[string]string
	header    map[string]string
	body      []byte
	rawBody   bool
	remoteAddr string
}

type minimalResponseWriter struct {
	conn   net.Conn
	status int
	header map[string]string
	wrote  bool
}

func (w *minimalResponseWriter) Header() map[string]string {
	if w.header == nil {
		w.header = make(map[string]string)
	}
	return w.header
}

func (w *minimalResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *minimalResponseWriter) Write(data []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
		status := 200
		if w.status != 0 {
			status = w.status
		}
		header := ""
		for k, v := range w.header {
			header += fmt.Sprintf("%s: %s\r\n", k, v)
		}
		fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\n%s\r\n", status, statusText(status), header)
	}
	return w.conn.Write(data)
}

func (w *minimalResponseWriter) Flush() error {
	return nil
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 400:
		return "Bad Request"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 500:
		return "Internal Server Error"
	case 503:
		return "Service Unavailable"
	default:
		return "OK"
	}
}

func readRequest(conn net.Conn) (*minimalRequest, error) {
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}

	data := string(buf[:n])
	lines := strings.Split(data, "\r\n")
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty request")
	}

	reqLine := strings.Split(lines[0], " ")
	if len(reqLine) < 2 {
		return nil, fmt.Errorf("invalid request line")
	}

	req := &minimalRequest{
		method:     reqLine[0],
		path:       reqLine[1],
		header:     make(map[string]string),
		remoteAddr: conn.RemoteAddr().String(),
	}

	for i := 1; i < len(lines) && lines[i] != ""; i++ {
		parts := strings.SplitN(lines[i], ":", 2)
		if len(parts) == 2 {
			req.header[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}

	if idx := strings.Index(req.path, "?"); idx != -1 {
		req.query = make(map[string]string)
		for _, pair := range strings.Split(req.path[idx+1:], "&") {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) == 2 {
				req.query[kv[0]] = kv[1]
			}
		}
		req.path = req.path[:idx]
	}

	if cl := req.header["Content-Length"]; cl != "" {
		if length, err := strconv.Atoi(cl); err == nil && length > 0 {
			req.body = make([]byte, length)
			_, err := io.ReadFull(conn, req.body)
			if err != nil {
				return nil, err
			}
			req.rawBody = true
		}
	}

	return req, nil
}

// serveHTTP starts the minimal HTTP server
func serveHTTP(ln net.Listener, handler func(*minimalResponseWriter, *minimalRequest)) {
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			defer c.Close()
			for {
				req, err := readRequest(c)
				if err != nil {
					return
				}
				w := &minimalResponseWriter{conn: c}
				handler(w, req)
			}
		}(conn)
	}
}

func serveLocalFile(w *minimalResponseWriter, r *minimalRequest, path string) {
	f, err := os.Open(path)
	if err != nil {
		w.WriteHeader(404)
		w.Write([]byte("not found"))
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		w.WriteHeader(500)
		w.Write([]byte("stat error"))
		return
	}

	ext := ""
	if idx := strings.LastIndex(path, "."); idx != -1 {
		ext = path[idx:]
	}
	ct := "application/octet-stream"
	switch strings.ToLower(ext) {
	case ".html":
		ct = "text/html"
	case ".css":
		ct = "text/css"
	case ".js":
		ct = "application/javascript"
	case ".png":
		ct = "image/png"
	case ".jpg", ".jpeg":
		ct = "image/jpeg"
	case ".gif":
		ct = "image/gif"
	case ".svg":
		ct = "image/svg+xml"
	case ".ico":
		ct = "image/x-icon"
	case ".json":
		ct = "application/json"
	case ".xml":
		ct = "application/xml"
	case ".txt":
		ct = "text/plain"
	case ".lua":
		ct = "text/plain"
	}

	w.Header()["Content-Type"] = ct
	w.Header()["Content-Length"] = fmt.Sprintf("%d", info.Size())
	w.WriteHeader(200)
	io.Copy(w, f)
}
