package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/gorilla/websocket"
)

// FeedbackStateUpdate is a single state change from the openapi-go feedback WebSocket.
type FeedbackStateUpdate struct {
	StateName string
	Value     string
}

// feedbackMessage is the JSON envelope for feedback WebSocket messages.
// Outbound uses "URI" (uppercase); inbound may use "uri" (lowercase).
type feedbackMessage struct {
	URI      string           `json:"URI"`
	URIAlt   string           `json:"uri"`
	Messages []map[string]any `json:"messages"`
}

func (m *feedbackMessage) uri() string {
	if m.URI != "" {
		return m.URI
	}
	return m.URIAlt
}

// FeedbackClient connects to openapi-go feedback WebSocket for StateCenter subscriptions.
// See docs/03-openapi-websocket.md.
type FeedbackClient struct {
	url    string
	conn   *websocket.Conn
	mu     sync.Mutex
	logger *log.Logger
}

// NewFeedbackClient creates a client for ws://host:port/feedback/v1/register.
func NewFeedbackClient(host string, port int) *FeedbackClient {
	return &FeedbackClient{
		url:    fmt.Sprintf("ws://%s:%d/feedback/v1/register", host, port),
		logger: log.New(os.Stderr, "[feedback] ", log.LstdFlags),
	}
}

// Connect dials the feedback WebSocket endpoint.
func (f *FeedbackClient) Connect(ctx context.Context) error {
	if f.logger == nil {
		f.logger = log.New(os.Stderr, "[feedback] ", log.LstdFlags)
	}
	dialer := websocket.Dialer{}
	conn, _, err := dialer.DialContext(ctx, f.url, nil)
	if err != nil {
		return fmt.Errorf("feedback dial: %w", err)
	}
	f.conn = conn
	f.logger.Println("connected")
	return nil
}

// Close closes the WebSocket connection.
func (f *FeedbackClient) Close() error {
	if f.conn != nil {
		return f.conn.Close()
	}
	return nil
}

// RegisterStates subscribes to state name updates. Current values are pushed immediately.
func (f *FeedbackClient) RegisterStates(stateNames []string) error {
	if len(stateNames) == 0 {
		return nil
	}
	msg := feedbackMessage{
		URI: "feedback/state/register",
		Messages: []map[string]any{
			{"states": stateNames},
		},
	}
	if err := f.sendJSON(msg); err != nil {
		return fmt.Errorf("feedback register: %w", err)
	}
	f.logger.Printf("registered %d states", len(stateNames))
	return nil
}

// ReadLoop reads messages and dispatches feedback state updates to handler.
func (f *FeedbackClient) ReadLoop(ctx context.Context, handler func(FeedbackStateUpdate)) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, data, err := f.conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				f.logger.Println("connection closed")
				return
			}
			f.logger.Printf("read error: %v", err)
			return
		}

		var msg feedbackMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			f.logger.Printf("ignoring malformed message: %v", err)
			continue
		}

		if msg.uri() != "feedback/state/update" {
			continue
		}

		for _, m := range msg.Messages {
			for stateName, raw := range m {
				value := formatStateValue(raw)
				handler(FeedbackStateUpdate{
					StateName: stateName,
					Value:     value,
				})
			}
		}
	}
}

func (f *FeedbackClient) sendJSON(msg feedbackMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conn.WriteJSON(msg)
}
