package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"wormhole-server/pkg/tunnel"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type Server struct {
	tunnelManager *tunnel.TunnelManager
	clients       map[string]*Client
	clientsMutex  sync.RWMutex
}

type Client struct {
	conn         *websocket.Conn
	tunnel       *tunnel.Tunnel
	requests     map[string]chan *tunnel.Message
	mutex        sync.RWMutex
	writeMutex   sync.Mutex          // Protects WebSocket writes
	requestQueue chan *QueuedRequest // Queue for handling concurrent requests
}

type QueuedRequest struct {
	msg      *tunnel.Message
	respChan chan *tunnel.Message
}

func NewServer() *Server {
	return &Server{
		tunnelManager: tunnel.NewTunnelManager(),
		clients:       make(map[string]*Client),
	}
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade connection: %v", err)
		return
	}
	defer conn.Close()

	// Set up ping handler
	conn.SetPingHandler(func(message string) error {
		log.Printf("Received ping from client, sending pong")
		return conn.WriteMessage(websocket.PongMessage, []byte(message))
	})

	subdomain := r.URL.Query().Get("subdomain")
	if subdomain == "" {
		log.Printf("No subdomain provided")
		return
	}

	client := &Client{
		conn:     conn,
		requests: make(map[string]chan *tunnel.Message),
	}

	localURL := r.URL.Query().Get("local")
	if localURL == "" {
		localURL = "http://localhost:3000"
	}

	t := &tunnel.Tunnel{
		ID:        generateID(),
		Subdomain: subdomain,
		LocalURL:  localURL,
		RemoteURL: fmt.Sprintf("http://%s.localhost:%s", subdomain, getPort()),
	}

	client.tunnel = t
	s.tunnelManager.AddTunnel(t)

	s.clientsMutex.Lock()
	s.clients[subdomain] = client
	s.clientsMutex.Unlock()

	log.Printf("Client connected for subdomain: %s", subdomain)

	for {
		var msg tunnel.Message
		err := conn.ReadJSON(&msg)
		if err != nil {
			log.Printf("Error reading message: %v", err)
			break
		}

		log.Printf("Server WebSocket: Received message type: %s, ID: %s", msg.Type, msg.ID)

		if msg.Type == "request" {
			log.Printf("Server WebSocket: Processing request: %s %s", msg.Method, msg.URL)
			go s.handleClientRequest(client, &msg)
		} else {
			log.Printf("Server WebSocket: Handling response message for ID: %s, status: %d", msg.ID, msg.Status)
			client.mutex.RLock()
			if respChan, exists := client.requests[msg.ID]; exists {
				log.Printf("Server WebSocket: Found pending request, sending response to HTTP handler")
				respChan <- &msg
			} else {
				log.Printf("Server WebSocket: No pending request found for ID: %s", msg.ID)
			}
			client.mutex.RUnlock()
		}
	}

	s.clientsMutex.Lock()
	delete(s.clients, subdomain)
	s.clientsMutex.Unlock()
	s.tunnelManager.RemoveTunnel(subdomain)
	log.Printf("Client disconnected for subdomain: %s", subdomain)
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		http.Error(w, "Invalid subdomain", http.StatusBadRequest)
		return
	}

	subdomain := parts[0]
	log.Printf("Server HTTP: Received request %s %s from %s (Host: %s)", r.Method, r.URL.RequestURI(), r.RemoteAddr, r.Host)

	s.clientsMutex.RLock()
	client, exists := s.clients[subdomain]
	s.clientsMutex.RUnlock()

	if !exists {
		log.Printf("Server HTTP: Tunnel not found for subdomain: %s", subdomain)
		http.Error(w, "Tunnel not found", http.StatusNotFound)
		return
	}

	requestID := generateID()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}

	msg := tunnel.HTTPRequestToMessage(r, requestID)
	msg.Body = body
	msg.Type = "request"

	client.mutex.Lock()
	client.requests[requestID] = make(chan *tunnel.Message, 1)
	client.mutex.Unlock()

	log.Printf("Server HTTP: Sending request to client via WebSocket: %s %s (ID: %s)", msg.Method, msg.URL, requestID)

	// Use a mutex to prevent concurrent WebSocket writes which can cause corruption
	client.writeMutex.Lock()
	client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = client.conn.WriteJSON(msg)
	client.writeMutex.Unlock()
	if err != nil {
		log.Printf("Server HTTP: Failed to send WebSocket message: %v", err)
		// Clean up the request channel on failure
		client.mutex.Lock()
		delete(client.requests, requestID)
		client.mutex.Unlock()
		http.Error(w, "Failed to forward request", http.StatusInternalServerError)
		return
	}
	log.Printf("Server HTTP: Request sent to client, waiting for response...")

	select {
	case response := <-client.requests[requestID]:
		log.Printf("Server HTTP: Received response from client: status %d, body length %d", response.Status, len(response.Body))
		client.mutex.Lock()
		delete(client.requests, requestID)
		client.mutex.Unlock()

		for key, value := range response.Headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(response.Status)
		w.Write(response.Body)
		log.Printf("Server HTTP: Response sent to browser: status %d", response.Status)

	case <-time.After(15 * time.Second):
		log.Printf("Server HTTP: Request %s timed out after 15 seconds", requestID)
		client.mutex.Lock()
		delete(client.requests, requestID)
		client.mutex.Unlock()
		http.Error(w, "Request timeout", http.StatusRequestTimeout)
	}
}

func (s *Server) handleClientRequest(client *Client, msg *tunnel.Message) {
	log.Printf("Server: Forwarding request %s %s to local service", msg.Method, msg.URL)

	localURL := client.tunnel.LocalURL
	if localURL == "" {
		localURL = "http://localhost:3000"
	}

	// Better URL construction to handle various path formats
	var fullURL string
	if strings.HasPrefix(msg.URL, "/") {
		// Absolute path - just append to base URL
		fullURL = strings.TrimSuffix(localURL, "/") + msg.URL
	} else {
		// Relative path - ensure proper joining
		fullURL = strings.TrimSuffix(localURL, "/") + "/" + msg.URL
	}
	log.Printf("Server: Original URL: %s -> Local URL: %s", msg.URL, fullURL)

	var body io.Reader
	if len(msg.Body) > 0 {
		body = bytes.NewReader(msg.Body)
	}

	req, err := http.NewRequest(msg.Method, fullURL, body)
	if err != nil {
		log.Printf("Server: Failed to create request: %v", err)
		response := &tunnel.Message{
			Type:   "response",
			ID:     msg.ID,
			Status: 502,
			Headers: map[string]string{
				"Content-Type": "text/plain",
			},
			Body: []byte("Bad Gateway: " + err.Error()),
		}
		client.conn.WriteJSON(response)
		return
	}

	for key, value := range msg.Headers {
		if strings.ToLower(key) != "host" && strings.ToLower(key) != "origin" {
			req.Header.Set(key, value)
		}
	}

	// Set Origin header to match the local URL to avoid CSRF protection issues
	if localURL != "" {
		req.Header.Set("Origin", localURL)
	}

	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("Server: Failed to make local request: %v", err)
		response := &tunnel.Message{
			Type:   "response",
			ID:     msg.ID,
			Status: 502,
			Headers: map[string]string{
				"Content-Type": "text/plain",
			},
			Body: []byte("Bad Gateway: " + err.Error()),
		}
		client.conn.WriteJSON(response)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Server: Failed to read response body: %v", err)
		return
	}

	headers := make(map[string]string)
	for key, values := range resp.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	log.Printf("Server: Sending response back to client: status %d", resp.StatusCode)
	response := &tunnel.Message{
		Type:    "response",
		ID:      msg.ID,
		Status:  resp.StatusCode,
		Headers: headers,
		Body:    respBody,
	}

	// Use write mutex for WebSocket response as well
	client.writeMutex.Lock()
	client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = client.conn.WriteJSON(response)
	client.writeMutex.Unlock()
	if err != nil {
		log.Printf("Server: Failed to send response: %v", err)
	}
}

func generateID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func getPort() string {
	var port string

	// Check command line flag first
	portFlag := flag.Lookup("port")
	if portFlag != nil && portFlag.Value.String() != "" {
		port = portFlag.Value.String()
	}

	// Check environment variable if no flag provided
	if port == "" {
		port = os.Getenv("PORT")
	}

	// Default to 8080 if nothing specified
	if port == "" {
		port = "8080"
	}

	// Validate port number
	if _, err := strconv.Atoi(port); err != nil {
		log.Printf("Invalid port number %s, using default 8080", port)
		port = "8080"
	}

	return port
}

func main() {
	flag.String("port", "", "Port to run the server on (default: 8080, can also use PORT env var)")
	flag.Parse()

	server := NewServer()
	port := getPort()

	http.HandleFunc("/ws", server.handleWebSocket)
	http.HandleFunc("/", server.handleHTTP)

	log.Printf("Wormhole server starting on :%s", port)
	log.Printf("WebSocket endpoint: ws://localhost:%s/ws", port)
	log.Printf("HTTP tunnels: http://{subdomain}.localhost:%s", port)

	log.Fatal(http.ListenAndServe(":"+port, nil))
}
