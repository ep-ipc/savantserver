package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandleClimateCommand_Mode(t *testing.T) {
	var gotBody map[string]any
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			json.NewDecoder(r.Body).Decode(&gotBody)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer apiServer.Close()

	b := newTestBridgeWithThermostat(t)
	b.api = &SavantAPI{baseURL: apiServer.URL, client: apiServer.Client()}

	mode := "heat"
	b.handleClimateCommand("environment/kitchen_thermostat", ClimateCommand{Mode: &mode})

	if gotBody["command"] != "SetHVACModeHeat" {
		t.Errorf("command = %v, want SetHVACModeHeat", gotBody["command"])
	}

	st := b.thermostatState[b.thermostats[0].UniqueID]
	if st.Mode != "heat" {
		t.Errorf("optimistic mode = %q, want heat", st.Mode)
	}
}

func TestHandleClimateCommand_Setpoints(t *testing.T) {
	var commands []string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			commands = append(commands, body["command"].(string))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer apiServer.Close()

	b := newTestBridgeWithThermostat(t)
	b.api = &SavantAPI{baseURL: apiServer.URL, client: apiServer.Client()}

	low, high := 68.0, 74.0
	b.handleClimateCommand("environment/kitchen_thermostat", ClimateCommand{
		TemperatureLow:  &low,
		TemperatureHigh: &high,
	})

	if len(commands) != 2 {
		t.Fatalf("expected 2 commands, got %v", commands)
	}
}

func newTestBridgeWithThermostat(t *testing.T) *Bridge {
	t.Helper()

	var components []HvacComponent
	var feedback HvacFeedbackResponse
	loadFixtureInto(t, "hvac_components.json", &components)
	loadFixtureInto(t, "hvac_feedback.json", &feedback)

	rooms := []Room{{
		RoomID: "45A014C2-AE2B-4B00-9F4B-55716C88AF1D",
		Name:   "Environment",
	}}

	thermostats, err := BuildThermostatEntities(rooms, components, &feedback)
	if err != nil {
		t.Fatalf("BuildThermostatEntities: %v", err)
	}

	// Use Kitchen Thermostat as first entity for predictable tests
	for i := range thermostats {
		if thermostats[i].Name == "Kitchen Thermostat" {
			thermostats[0], thermostats[i] = thermostats[i], thermostats[0]
			break
		}
	}

	b := newTestBridge()
	b.thermostats = thermostats
	b.thermostatState = make(map[string]*ThermostatState, len(thermostats))
	b.thermostatIdx = make(map[string]*ThermostatEntity, len(thermostats))
	b.thermostatAddrIdx = make(map[int]*ThermostatEntity, len(thermostats))

	for i := range b.thermostats {
		e := &b.thermostats[i]
		b.thermostatState[e.UniqueID] = &ThermostatState{Mode: "off", FanMode: "auto"}
		b.thermostatIdx[e.RoomSlug+"/"+e.EntitySlug] = e
	}
	// Kitchen is thermostats[0]; pin avc addr so handleThermostatUpdate tests are deterministic
	// (all SST-W300 components share address1 "1" in fixtures).
	if b.thermostats[0].ThermostatAddr >= 0 {
		b.thermostatAddrIdx[b.thermostats[0].ThermostatAddr] = &b.thermostats[0]
	}

	return b
}

func TestApplyAvcThermostatUpdate_JSON(t *testing.T) {
	b := newTestBridgeWithThermostat(t)

	value := `{"ThermostatCurrentHeatPoint":"72","ThermostatMode":"Heat"}`
	if !b.applyAvcThermostatUpdate(&b.thermostats[0], value) {
		t.Fatal("expected JSON avc update to apply")
	}

	st := b.thermostatState[b.thermostats[0].UniqueID]
	if st.TemperatureLow == nil || *st.TemperatureLow != 72 {
		t.Errorf("heat setpoint = %v, want 72", st.TemperatureLow)
	}
	if st.Mode != "heat" {
		t.Errorf("mode = %q, want heat", st.Mode)
	}
}

func TestHandleFeedbackStateUpdate(t *testing.T) {
	b := newTestBridgeWithThermostat(t)
	b.buildThermostatStateIndex()

	b.handleFeedbackStateUpdate(FeedbackStateUpdate{
		StateName: "Kitchen Thermostat.HVAC_controller.ThermostatCurrentHeatPoint",
		Value:     "72",
	})

	st := b.thermostatState[b.thermostats[0].UniqueID]
	if st.TemperatureLow == nil || *st.TemperatureLow != 72 {
		t.Errorf("expected heat setpoint 72, got %v", st.TemperatureLow)
	}
}

func TestHandleThermostatUpdate_FallsBackToRefresh(t *testing.T) {
	apiServer := httptest.NewServer(locationStateHandler(map[string]string{
		"Kitchen Thermostat.HVAC_controller.ThermostatCurrentHeatPoint": "70",
		"Kitchen Thermostat.HVAC_controller.ThermostatCurrentCoolPoint": "74",
		"Kitchen Thermostat.HVAC_controller.ThermostatMode":             "Heat",
	}, []string{"Kitchen Thermostat.HVAC_controller.ThermostatFanMode"}))
	defer apiServer.Close()

	b := newTestBridgeWithThermostat(t)
	b.api = &SavantAPI{baseURL: apiServer.URL, client: apiServer.Client()}

	// Opaque avc value triggers per-entity REST refresh (async in production).
	b.handleThermostatUpdate(AVCStateUpdate{State: "thermostat.1", Value: "opaque"})
	time.Sleep(200 * time.Millisecond)

	st := b.thermostatState[b.thermostats[0].UniqueID]
	if st.TemperatureLow == nil || *st.TemperatureLow != 70 {
		t.Errorf("expected heat setpoint 70 after refresh, got %v", st.TemperatureLow)
	}
}

func TestFetchHvacComponentsIntegration(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config/v1/hvac/components":
			http.ServeFile(w, r, "testdata/hvac_components.json")
		case "/feedback/v1/states/hvac":
			http.ServeFile(w, r, "testdata/hvac_feedback.json")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	api := &SavantAPI{baseURL: ts.URL, client: ts.Client()}
	components, err := api.FetchHvacComponents(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchHvacComponents: %v", err)
	}
	if len(components) != 8 {
		t.Errorf("expected 8 components, got %d", len(components))
	}

	feedback, err := api.FetchHvacFeedbackStates(context.Background())
	if err != nil {
		t.Fatalf("FetchHvacFeedbackStates: %v", err)
	}
	if len(feedback.Thermostats) != 8 {
		t.Errorf("expected 8 thermostat feedback entries, got %d", len(feedback.Thermostats))
	}
}
