package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SavantAPI is an HTTP client for Savant's openapi-go REST API (default port 3062).
// openapi-go provides read-only access to configuration (rooms, loads, devices,
// scenes) and state values. It does NOT support hardware control — use AVCClient
// for that. See docs/02-rest-api.md for endpoint details.
type SavantAPI struct {
	baseURL string
	client  *http.Client
}

// NewSavantAPI creates a new SavantAPI client pointing at the given host and port.
func NewSavantAPI(host string, port int) *SavantAPI {
	return &SavantAPI{
		baseURL: fmt.Sprintf("http://%s:%d", host, port),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// FetchRooms retrieves all rooms from the Savant REST API.
func (s *SavantAPI) FetchRooms(ctx context.Context) ([]Room, error) {
	var rooms []Room
	if err := s.getJSON(ctx, "/config/v1/rooms", &rooms); err != nil {
		return nil, fmt.Errorf("fetching rooms: %w", err)
	}
	return rooms, nil
}

// FetchLoads retrieves all lighting loads from the Savant REST API.
func (s *SavantAPI) FetchLoads(ctx context.Context) ([]Load, error) {
	var loads []Load
	if err := s.getJSON(ctx, "/config/v1/lighting/loads", &loads); err != nil {
		return nil, fmt.Errorf("fetching loads: %w", err)
	}
	return loads, nil
}

// FetchDevices retrieves all lighting devices from the Savant REST API.
// Note: the list endpoint does not include loads or buttons arrays.
func (s *SavantAPI) FetchDevices(ctx context.Context) ([]Device, error) {
	var devices []Device
	if err := s.getJSON(ctx, "/config/v1/lighting/devices", &devices); err != nil {
		return nil, fmt.Errorf("fetching devices: %w", err)
	}
	return devices, nil
}

// FetchDevice retrieves a single device by ID, including its loads and buttons.
func (s *SavantAPI) FetchDevice(ctx context.Context, deviceID string) (*Device, error) {
	var device Device
	path := fmt.Sprintf("/config/v1/lighting/devices/%s", deviceID)
	if err := s.getJSON(ctx, path, &device); err != nil {
		return nil, fmt.Errorf("fetching device %s: %w", deviceID, err)
	}
	return &device, nil
}

// FetchHvacComponents retrieves all HVAC components from the Savant REST API.
func (s *SavantAPI) FetchHvacComponents(ctx context.Context, roomID string) ([]HvacComponent, error) {
	path := "/config/v1/hvac/components"
	if roomID != "" {
		path += "?RoomID=" + url.QueryEscape(roomID)
	}
	var components []HvacComponent
	if err := s.getJSON(ctx, path, &components); err != nil {
		return nil, fmt.Errorf("fetching hvac components: %w", err)
	}
	return components, nil
}

// FetchHvacFeedbackStates retrieves HVAC state name metadata from the feedback API.
func (s *SavantAPI) FetchHvacFeedbackStates(ctx context.Context) (*HvacFeedbackResponse, error) {
	var resp HvacFeedbackResponse
	if err := s.getJSON(ctx, "/feedback/v1/states/hvac", &resp); err != nil {
		return nil, fmt.Errorf("fetching hvac feedback states: %w", err)
	}
	return &resp, nil
}

// SendHvacCommand issues an HVAC control command via REST PUT.
func (s *SavantAPI) SendHvacCommand(ctx context.Context, deviceID, command string, args map[string]any) error {
	body := map[string]any{
		"command": command,
	}
	if len(args) > 0 {
		body["arguments"] = args
	}
	path := fmt.Sprintf("/config/v1/hvac/components/%s/command", url.PathEscape(deviceID))
	if err := s.putJSON(ctx, path, body); err != nil {
		return fmt.Errorf("hvac command %s for %s: %w", command, deviceID, err)
	}
	return nil
}

// stateValueResponse is the GET /config/v1/location/state response body.
type stateValueResponse struct {
	State string `json:"state"`
	Value any    `json:"value"`
}

// FetchState reads a single state value by name from StateCenter via the v1 REST API.
func (s *SavantAPI) FetchState(ctx context.Context, stateName string) (string, error) {
	path := "/config/v1/location/state?state=" + url.QueryEscape(stateName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode == http.StatusInternalServerError {
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &errResp) == nil && strings.Contains(errResp.Error, "no value") {
			return "", nil
		}
		return "", fmt.Errorf("fetching state %s: status %d: %s", stateName, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching state %s: status %d: %s", stateName, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var stateResp stateValueResponse
	if err := json.Unmarshal(body, &stateResp); err != nil {
		return "", fmt.Errorf("decoding state %s: %w", stateName, err)
	}
	return formatStateValue(stateResp.Value), nil
}

func formatStateValue(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(v)
	}
}

// FetchStates reads multiple state values concurrently.
// Returns a map from state name to value.
func (s *SavantAPI) FetchStates(ctx context.Context, stateNames []string) (map[string]string, error) {
	type result struct {
		name  string
		value string
		err   error
	}

	results := make(chan result, len(stateNames))
	var wg sync.WaitGroup

	for _, name := range stateNames {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			val, err := s.FetchState(ctx, n)
			results <- result{name: n, value: val, err: err}
		}(name)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	states := make(map[string]string, len(stateNames))
	var errs []string
	for r := range results {
		if r.err != nil {
			errs = append(errs, r.err.Error())
			continue
		}
		states[r.name] = r.value
	}

	if len(errs) > 0 {
		return states, fmt.Errorf("errors fetching states: %s", strings.Join(errs, "; "))
	}
	return states, nil
}

// getJSON performs a GET request and decodes the JSON response into dest.
func (s *SavantAPI) getJSON(ctx context.Context, path string, dest interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d for %s", resp.StatusCode, path)
	}

	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// putJSON performs a PUT request with a JSON body.
func (s *SavantAPI) putJSON(ctx context.Context, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d for %s", resp.StatusCode, path)
	}
	return nil
}

// BuildThermostatEntities joins rooms, HVAC components, and feedback into ThermostatEntity values.
func BuildThermostatEntities(rooms []Room, components []HvacComponent, feedback *HvacFeedbackResponse) ([]ThermostatEntity, error) {
	roomMap := make(map[string]Room, len(rooms))
	for _, r := range rooms {
		roomMap[r.RoomID] = r
	}

	entities := make([]ThermostatEntity, 0, len(components))
	for _, comp := range components {
		room, ok := roomMap[comp.RoomID]
		if !ok {
			return nil, fmt.Errorf("room %s not found for hvac component %s", comp.RoomID, comp.Name)
		}

		var fb *HvacThermostatFeedback
		if feedback != nil {
			if entry, ok := feedback.Thermostats[comp.Name]; ok {
				fb = &entry
			}
		}

		stateNames := buildHvacStateNames(fb)
		if len(stateNames) == 0 {
			continue
		}

		modes := []string{"off"}
		if comp.HeatControlSupported {
			modes = append(modes, "heat")
		}
		if comp.CoolControlSupported {
			modes = append(modes, "cool")
		}
		if comp.AutoControlSupported {
			modes = append(modes, "heat_cool")
		}

		fanModes := []string{"auto", "on", "circulate"}

		addr := parseThermostatAddress(comp.Address1)
		addr2 := parseThermostatAddress(comp.Address2)
		if fb != nil && len(fb.ThermostatAddresses) > 0 {
			if a := parseThermostatAddress(fb.ThermostatAddresses[0].ThermostatAddress); a >= 0 {
				addr = a
			}
			if a := parseThermostatAddress(fb.ThermostatAddresses[0].ThermostatAddress2); a >= 0 {
				addr2 = a
			}
		}

		uniqueID := "savant_hvac_" + strings.ToLower(strings.ReplaceAll(comp.DeviceID, "-", "_"))

		entity := ThermostatEntity{
			UniqueID:         uniqueID,
			Name:             comp.Name,
			RoomName:         room.Name,
			RoomSlug:         Slugify(room.Name),
			EntitySlug:       Slugify(comp.Name),
			DeviceID:         comp.DeviceID,
			DeviceModel:      comp.Model,
			Manufacturer:     comp.Manufacturer,
			ThermostatAddr:   addr,
			ThermostatAddr2:  addr2,
			Modes:            modes,
			FanModes:         fanModes,
			SupportsHumidity: comp.HumiditySupported,
			StateNames:       stateNames,
		}
		entities = append(entities, entity)
	}

	return entities, nil
}

// BuildEntities transforms rooms, loads, and devices into LightEntity values
// suitable for Home Assistant MQTT discovery.
//
// The devices slice may include devices with populated Buttons (from per-device
// FetchDevice calls). natural light toggle mapping is derived from buttons with
// "naturallighttogglewithoff" or "naturallighttoggle" functions.
func BuildEntities(rooms []Room, loads []Load, devices []Device) ([]LightEntity, error) {
	// 1. roomID → Room
	roomMap := make(map[string]Room, len(rooms))
	for _, r := range rooms {
		roomMap[r.RoomID] = r
	}

	// 2. deviceID → Device
	deviceMap := make(map[string]Device, len(devices))
	for _, d := range devices {
		deviceMap[d.DeviceID] = d
	}

	// 3. Build natural light toggle address map: roomID → switch address
	// Iterate devices with buttons to find natural light toggle buttons.
	daylightToggleMap := make(map[string]int)
	for _, d := range devices {
		if len(d.Buttons) == 0 {
			continue
		}
		devAddr, err := ParseDeviceAddress(d.Address)
		if err != nil {
			continue
		}
		for _, btn := range d.Buttons {
			fn := btn.Function
			if fn != "naturallighttogglewithoff" && fn != "naturallighttoggle" {
				continue
			}
			targetRoomID := btn.RoomID
			if targetRoomID == "" {
				continue
			}
			switchAddr := ComputeSwitchAddr(devAddr, btn.Index)
			daylightToggleMap[targetRoomID] = switchAddr
		}
	}

	// 4. Build entities from loads
	entities := make([]LightEntity, 0, len(loads))
	for _, load := range loads {
		dev, ok := deviceMap[load.DeviceID]
		if !ok {
			return nil, fmt.Errorf("device %s not found for load %s", load.DeviceID, load.LoadID)
		}

		room, ok := roomMap[load.RoomID]
		if !ok {
			return nil, fmt.Errorf("room %s not found for load %s", load.RoomID, load.LoadID)
		}

		devAddr, err := ParseDeviceAddress(dev.Address)
		if err != nil {
			return nil, fmt.Errorf("parsing device address for load %s: %w", load.LoadID, err)
		}

		hexAddr := ComputeHexAddr(devAddr, load.Offset)

		daylightToggleAddr := -1
		if load.FollowDaylight {
			if addr, ok := daylightToggleMap[load.RoomID]; ok {
				daylightToggleAddr = addr
			}
		}

		entity := LightEntity{
			UniqueID:       fmt.Sprintf("savant_load_%s_%d", dev.Address, load.Offset),
			Name:           load.Name,
			RoomName:       room.Name,
			RoomSlug:       Slugify(room.Name),
			LoadSlug:       Slugify(load.Name),
			DeviceAddress:  dev.Address,
			LoadOffset:     load.Offset,
			HexAddr:        hexAddr,
			IsDimmable:     load.Type == 0,
			FollowDaylight: load.FollowDaylight,
			Min:            load.Min,
			Max:            load.Max,
			DaylightToggleAddr:   daylightToggleAddr,
			DeviceModel:    dev.RPMLightingDeviceName,
		}
		entities = append(entities, entity)
	}

	return entities, nil
}
