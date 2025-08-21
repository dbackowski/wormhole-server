package tunnel

import (
	"net/http"
)

type Message struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Method  string            `json:"method,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
	Status  int               `json:"status,omitempty"`
}

type Tunnel struct {
	ID       string
	Subdomain string
	LocalURL string
	RemoteURL string
}

type TunnelManager struct {
	tunnels map[string]*Tunnel
}

func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		tunnels: make(map[string]*Tunnel),
	}
}

func (tm *TunnelManager) AddTunnel(tunnel *Tunnel) {
	tm.tunnels[tunnel.Subdomain] = tunnel
}

func (tm *TunnelManager) GetTunnel(subdomain string) (*Tunnel, bool) {
	tunnel, exists := tm.tunnels[subdomain]
	return tunnel, exists
}

func (tm *TunnelManager) RemoveTunnel(subdomain string) {
	delete(tm.tunnels, subdomain)
}

func HTTPRequestToMessage(req *http.Request, id string) *Message {
	headers := make(map[string]string)
	for key, values := range req.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	// Use RequestURI() to preserve the original request path with query parameters
	// and fragments, which is crucial for asset loading
	url := req.URL.RequestURI()

	return &Message{
		Type:    "request",
		ID:      id,
		Method:  req.Method,
		URL:     url,
		Headers: headers,
	}
}

func MessageToHTTPResponse(msg *Message) *http.Response {
	header := make(http.Header)
	for key, value := range msg.Headers {
		header.Set(key, value)
	}

	return &http.Response{
		StatusCode: msg.Status,
		Header:     header,
	}
}