package main

import (
	"fmt"
	"strconv"
	"strings"
)

// --- REST API HVAC types ---

// HvacComponent matches GET /config/v1/hvac/components items.
type HvacComponent struct {
	DeviceID                    string `json:"deviceID"`
	RoomID                      string `json:"roomID"`
	Name                        string `json:"name"`
	Model                       string `json:"model"`
	Manufacturer                string `json:"manufacturer"`
	UID                         string `json:"uid"`
	Address1                    string `json:"address1"`
	Address2                    string `json:"address2,omitempty"`
	State1                      string `json:"state1,omitempty"`
	Enabled                     bool   `json:"enabled"`
	HumiditySupported           bool   `json:"humiditySupported"`
	TemperatureSupported        bool   `json:"temperatureSupported"`
	CoolControlSupported        bool   `json:"coolControlSupported"`
	HeatControlSupported        bool   `json:"heatControlSupported"`
	HumidifyControlSupported    bool   `json:"humidifyControlSupported"`
	DehumidifyControlSupported  bool   `json:"dehumidifyControlSupported"`
	AutoControlSupported        bool   `json:"autoControlSupported"`
	HistorySupported            bool   `json:"historySupported"`
	ExternalTemperatureSupported bool  `json:"externalTemperatureSupported"`
	TemperatureSetPoints        int64  `json:"temperatureSetPoints"`
	HumiditySetPoints           int64  `json:"humiditySetPoints"`
}

// HvacFeedbackResponse matches GET /feedback/v1/states/hvac.
type HvacFeedbackResponse struct {
	Thermostats map[string]HvacThermostatFeedback `json:"Thermostats"`
}

// HvacThermostatFeedback is per-thermostat state metadata from feedback API.
type HvacThermostatFeedback struct {
	ThermostatAddresses []ThermostatAddressPair `json:"ThermostatAddresses"`
	States              []HvacStateRef          `json:"States"`
}

// ThermostatAddressPair holds avc thermostat addresses from feedback.
type ThermostatAddressPair struct {
	ThermostatAddress  string `json:"ThermostatAddress"`
	ThermostatAddress2 string `json:"ThermostatAddress2"`
}

// HvacStateRef describes a single Savant state variable for an HVAC component.
type HvacStateRef struct {
	Type           string         `json:"Type"`
	StateString    string         `json:"StateString"`
	AdditionalInfo map[string]any `json:"AdditionalInfo"`
}

// Logical state keys used in ThermostatEntity.StateNames.
const (
	hvacStateMode             = "mode"
	hvacStateFanMode          = "fan_mode"
	hvacStateCurrentTemp      = "current_temperature"
	hvacStateHeatSetpoint     = "heat_setpoint"
	hvacStateCoolSetpoint     = "cool_setpoint"
	hvacStateHumidityMode     = "humidity_mode"
	hvacStateFanModeCycle     = "fan_mode_cycle"
)

// ThermostatEntity represents a single HA climate entity mapped from Savant HVAC.
type ThermostatEntity struct {
	UniqueID         string
	Name             string
	RoomName         string
	RoomSlug         string
	EntitySlug       string
	DeviceID         string
	DeviceModel      string
	Manufacturer     string
	ThermostatAddr   int // parsed from address1 / feedback, -1 if unknown
	ThermostatAddr2  int
	Modes            []string // HA modes: off, heat, cool, heat_cool
	FanModes         []string // auto, on, circulate
	SupportsHumidity bool
	StateNames       map[string]string // logical key -> full Savant state name
}

// StateTopic returns the MQTT state topic for this thermostat.
func (e *ThermostatEntity) StateTopic(prefix string) string {
	return fmt.Sprintf("%s/%s/%s/climate/state", prefix, e.RoomSlug, e.EntitySlug)
}

// CommandTopic returns the MQTT command topic for this thermostat.
func (e *ThermostatEntity) CommandTopic(prefix string) string {
	return fmt.Sprintf("%s/%s/%s/climate/set", prefix, e.RoomSlug, e.EntitySlug)
}

// DiscoveryTopic returns the HA MQTT Discovery config topic.
func (e *ThermostatEntity) DiscoveryTopic(haPrefix string) string {
	return fmt.Sprintf("%s/climate/%s/config", haPrefix, e.UniqueID)
}

// ThermostatState is the current state of a thermostat entity.
type ThermostatState struct {
	Mode            string   // HA mode: off, heat, cool, heat_cool
	FanMode         string   // auto, on, circulate
	CurrentTemp     *float64
	Temperature     *float64 // active target in heat/cool
	TemperatureLow  *float64 // heat setpoint in auto
	TemperatureHigh *float64 // cool setpoint in auto
	Humidity        *float64 // current relative humidity if known
	TargetHumidity  *float64
	HumidityMode    string // Savant humidity mode string
}

// ClimateCommand is a command received from HA via MQTT (partial updates allowed).
type ClimateCommand struct {
	Mode            *string  `json:"mode,omitempty"`
	FanMode         *string  `json:"fan_mode,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TemperatureLow  *float64 `json:"temperature_low,omitempty"`
	TemperatureHigh *float64 `json:"temperature_high,omitempty"`
	TargetHumidity  *float64 `json:"target_humidity,omitempty"`
	HumidityMode    *string  `json:"humidity_mode,omitempty"`
}

// climateStatePayload is the JSON published on the climate state topic.
type climateStatePayload struct {
	Mode            string   `json:"mode,omitempty"`
	FanMode         string   `json:"fan_mode,omitempty"`
	CurrentTemp     *float64 `json:"current_temperature,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TemperatureLow  *float64 `json:"temperature_low,omitempty"`
	TemperatureHigh *float64 `json:"temperature_high,omitempty"`
	Humidity        *float64 `json:"humidity,omitempty"`
	TargetHumidity  *float64 `json:"target_humidity,omitempty"`
}

func (st ThermostatState) toPayload() climateStatePayload {
	return climateStatePayload{
		Mode:            st.Mode,
		FanMode:         st.FanMode,
		CurrentTemp:     st.CurrentTemp,
		Temperature:     st.Temperature,
		TemperatureLow:  st.TemperatureLow,
		TemperatureHigh: st.TemperatureHigh,
		Humidity:        st.Humidity,
		TargetHumidity:  st.TargetHumidity,
	}
}

// applyHvacStateValue updates st based on a Savant state name suffix and raw value.
func applyHvacStateValue(_ *ThermostatEntity, logicalKey, value string, st *ThermostatState) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	switch logicalKey {
	case hvacStateMode:
		st.Mode = savantModeToHA(value)
	case hvacStateFanMode:
		st.FanMode = savantFanModeToHA(value)
	case hvacStateFanModeCycle:
		if value == "1" || strings.EqualFold(value, "true") {
			st.FanMode = "circulate"
		}
	case hvacStateCurrentTemp:
		if t, ok := parseTemperature(value); ok {
			st.CurrentTemp = &t
		}
	case hvacStateHeatSetpoint:
		if t, ok := parseTemperature(value); ok {
			st.TemperatureLow = &t
			if st.Mode == "heat" || st.Mode == "" {
				st.Temperature = &t
			}
		}
	case hvacStateCoolSetpoint:
		if t, ok := parseTemperature(value); ok {
			st.TemperatureHigh = &t
			if st.Mode == "cool" {
				st.Temperature = &t
			}
		}
	case hvacStateHumidityMode:
		st.HumidityMode = value
	}
}

func savantModeToHA(mode string) string {
	switch strings.TrimSpace(mode) {
	case "Off":
		return "off"
	case "Heat", "Emergency Heat":
		return "heat"
	case "Cool":
		return "cool"
	case "Auto", "Eco":
		return "heat_cool"
	default:
		return strings.ToLower(strings.ReplaceAll(mode, " ", "_"))
	}
}

func savantFanModeToHA(mode string) string {
	switch strings.TrimSpace(mode) {
	case "On":
		return "on"
	case "Off or Auto", "Auto", "Off":
		return "auto"
	default:
		return "auto"
	}
}

func haModeToSavantCommand(mode string) string {
	switch mode {
	case "off":
		return "SetHVACModeOff"
	case "heat":
		return "SetHVACModeHeat"
	case "cool":
		return "SetHVACModeCool"
	case "heat_cool":
		return "SetHVACModeAuto"
	default:
		return ""
	}
}

func haFanModeToSavantCommand(fan string) string {
	switch fan {
	case "auto":
		return "SetFanModeAuto"
	case "on":
		return "SetFanModeOn"
	case "circulate":
		return "SetFanModeCycle"
	default:
		return ""
	}
}

func parseTemperature(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func parseThermostatAddress(addr string) int {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return -1
	}
	n, err := strconv.ParseInt(addr, 0, 64)
	if err != nil {
		return -1
	}
	return int(n)
}

// buildHvacStateNames maps feedback state refs to logical keys for a component.
func buildHvacStateNames(feedback *HvacThermostatFeedback) map[string]string {
	if feedback == nil {
		return nil
	}
	names := make(map[string]string)
	for _, ref := range feedback.States {
		suffix := ref.StateString
		if idx := strings.LastIndex(suffix, "."); idx >= 0 {
			suffix = suffix[idx+1:]
		}
		switch suffix {
		case "ThermostatMode":
			names[hvacStateMode] = ref.StateString
		case "ThermostatFanMode":
			names[hvacStateFanMode] = ref.StateString
		case "IsThermostatCurrentFanModeCycle":
			names[hvacStateFanModeCycle] = ref.StateString
		case "ThermostatCurrentTemperature":
			names[hvacStateCurrentTemp] = ref.StateString
		case "ThermostatCurrentHeatPoint":
			names[hvacStateHeatSetpoint] = ref.StateString
		case "ThermostatCurrentCoolPoint":
			names[hvacStateCoolSetpoint] = ref.StateString
		case "ThermostatHumidityMode":
			names[hvacStateHumidityMode] = ref.StateString
		}
	}
	return names
}

// allHvacStateNames returns unique Savant state names to fetch for an entity.
func (e *ThermostatEntity) allHvacStateNames() []string {
	seen := make(map[string]bool)
	var names []string
	for _, n := range e.StateNames {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names
}
