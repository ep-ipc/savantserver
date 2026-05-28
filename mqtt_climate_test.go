package main

import (
	"encoding/json"
	"testing"
)

func TestBuildClimateDiscoveryPayload(t *testing.T) {
	entity := &ThermostatEntity{
		UniqueID:         "savant_hvac_test",
		Name:             "Kitchen Thermostat",
		RoomName:         "Environment",
		RoomSlug:         "environment",
		EntitySlug:       "kitchen_thermostat",
		Modes:            []string{"off", "heat", "cool", "heat_cool"},
		FanModes:         []string{"auto", "on", "circulate"},
		SupportsHumidity: true,
		Manufacturer:     "Savant",
		DeviceModel:      "SST-W300",
	}

	data, err := buildClimateDiscoveryPayload(entity, "savant")
	if err != nil {
		t.Fatalf("buildClimateDiscoveryPayload: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	assertStr(t, got, "name", "Kitchen Thermostat")
	assertStr(t, got, "state_topic", "savant/environment/kitchen_thermostat/climate/state")
	assertStr(t, got, "mode_command_topic", "savant/environment/kitchen_thermostat/climate/set")
	if _, ok := got["target_humidity_command_topic"]; !ok {
		t.Error("expected target_humidity_command_topic for humidity-capable entity")
	}
}

func TestBuildClimateStatePayload(t *testing.T) {
	low, high, cur, temp := 68.0, 74.0, 70.0, 68.0
	st := ThermostatState{
		Mode:            "heat_cool",
		FanMode:         "auto",
		CurrentTemp:     &cur,
		Temperature:     &temp,
		TemperatureLow:  &low,
		TemperatureHigh: &high,
	}

	data, err := buildClimateStatePayload(st)
	if err != nil {
		t.Fatalf("buildClimateStatePayload: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertStr(t, got, "mode", "heat_cool")
	if got["temperature_low"].(float64) != 68 {
		t.Errorf("temperature_low = %v", got["temperature_low"])
	}
}

func TestParseClimateCommandTopic(t *testing.T) {
	entityID, err := parseClimateCommandTopic(
		"savant/environment/kitchen_thermostat/climate/set", "savant")
	if err != nil {
		t.Fatalf("parseClimateCommandTopic: %v", err)
	}
	if entityID != "environment/kitchen_thermostat" {
		t.Errorf("entityID = %q", entityID)
	}
}
