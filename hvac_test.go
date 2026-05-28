package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildThermostatEntities(t *testing.T) {
	var components []HvacComponent
	var feedback HvacFeedbackResponse
	loadFixtureInto(t, "hvac_components.json", &components)
	loadFixtureInto(t, "hvac_feedback.json", &feedback)

	rooms := []Room{{
		RoomID: "45A014C2-AE2B-4B00-9F4B-55716C88AF1D",
		Name:   "Environment",
	}}

	entities, err := BuildThermostatEntities(rooms, components, &feedback)
	if err != nil {
		t.Fatalf("BuildThermostatEntities: %v", err)
	}
	if len(entities) != 8 {
		t.Fatalf("expected 8 thermostats, got %d", len(entities))
	}

	var kitchen *ThermostatEntity
	for i := range entities {
		if entities[i].Name == "Kitchen Thermostat" {
			kitchen = &entities[i]
			break
		}
	}
	if kitchen == nil {
		t.Fatal("Kitchen Thermostat not found")
	}

	if kitchen.RoomSlug != "environment" {
		t.Errorf("RoomSlug = %q, want environment", kitchen.RoomSlug)
	}
	if kitchen.EntitySlug != "kitchen_thermostat" {
		t.Errorf("EntitySlug = %q, want kitchen_thermostat", kitchen.EntitySlug)
	}
	if kitchen.ThermostatAddr != 1 {
		t.Errorf("ThermostatAddr = %d, want 1", kitchen.ThermostatAddr)
	}
	if !contains(kitchen.Modes, "heat_cool") {
		t.Errorf("Modes = %v, want heat_cool", kitchen.Modes)
	}
	if kitchen.StateNames[hvacStateMode] == "" {
		t.Error("expected mode state name")
	}
}

func TestApplyHvacStateValue(t *testing.T) {
	st := &ThermostatState{}
	applyHvacStateValue(nil, hvacStateMode, "Heat", st)
	if st.Mode != "heat" {
		t.Errorf("mode = %q, want heat", st.Mode)
	}
	applyHvacStateValue(nil, hvacStateHeatSetpoint, "72", st)
	applyHvacStateValue(nil, hvacStateCoolSetpoint, "74", st)
	applyHvacStateValue(nil, hvacStateCurrentTemp, "70.5", st)
	finalizeThermostatState(st)
	if st.TemperatureLow == nil || *st.TemperatureLow != 72 {
		t.Errorf("temperature_low = %v", st.TemperatureLow)
	}
	if st.CurrentTemp == nil || *st.CurrentTemp != 70.5 {
		t.Errorf("current_temperature = %v", st.CurrentTemp)
	}
}

func TestSendHvacCommand(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	api := &SavantAPI{baseURL: ts.URL, client: ts.Client()}
	err := api.SendHvacCommand(context.Background(), "device-id", "SetHVACModeHeat", nil)
	if err != nil {
		t.Fatalf("SendHvacCommand: %v", err)
	}
	if gotBody["command"] != "SetHVACModeHeat" {
		t.Errorf("command = %v", gotBody["command"])
	}
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
