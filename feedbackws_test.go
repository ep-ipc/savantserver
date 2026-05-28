package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testFeedbackServer(t *testing.T) (*httptest.Server, chan feedbackMessage, func(FeedbackStateUpdate)) {
	t.Helper()
	received := make(chan feedbackMessage, 8)
	var pushMu sync.Mutex
	var conn *websocket.Conn

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		conn, err = upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg feedbackMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			received <- msg

			if msg.uri() == "feedback/state/register" {
				// Push initial/current values like the real server.
				pushMu.Lock()
				c := conn
				pushMu.Unlock()
				if c != nil {
					update := feedbackMessage{
						URIAlt: "feedback/state/update",
						Messages: []map[string]any{
							{"Kitchen Thermostat.HVAC_controller.ThermostatCurrentHeatPoint": "72"},
						},
					}
					_ = c.WriteJSON(update)
				}
			}
		}
	}))

	pushUpdate := func(update FeedbackStateUpdate) {
		pushMu.Lock()
		c := conn
		pushMu.Unlock()
		if c == nil {
			return
		}
		msg := feedbackMessage{
			URIAlt: "feedback/state/update",
			Messages: []map[string]any{
				{update.StateName: update.Value},
			},
		}
		_ = c.WriteJSON(msg)
	}

	return server, received, pushUpdate
}

func TestFeedbackClient_RegisterAndReceive(t *testing.T) {
	server, received, _ := testFeedbackServer(t)
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):]
	client := &FeedbackClient{url: wsURL}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	updates := make(chan FeedbackStateUpdate, 4)
	go client.ReadLoop(context.Background(), func(u FeedbackStateUpdate) {
		updates <- u
	})

	if err := client.RegisterStates([]string{"Kitchen Thermostat.HVAC_controller.ThermostatCurrentHeatPoint"}); err != nil {
		t.Fatalf("RegisterStates: %v", err)
	}

	select {
	case msg := <-received:
		if msg.uri() != "feedback/state/register" {
			t.Errorf("register URI = %q", msg.uri())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for register message")
	}

	select {
	case u := <-updates:
		if u.StateName != "Kitchen Thermostat.HVAC_controller.ThermostatCurrentHeatPoint" {
			t.Errorf("state name = %q", u.StateName)
		}
		if u.Value != "72" {
			t.Errorf("value = %q, want 72", u.Value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for state update")
	}
}
