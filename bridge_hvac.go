package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func (b *Bridge) discoverHvac(ctx context.Context, rooms []Room) error {
	components, err := b.api.FetchHvacComponents(ctx, "")
	if err != nil {
		return err
	}
	if len(components) == 0 {
		return nil
	}

	feedback, err := b.api.FetchHvacFeedbackStates(ctx)
	if err != nil {
		return fmt.Errorf("hvac feedback: %w", err)
	}

	thermostats, err := BuildThermostatEntities(rooms, components, feedback)
	if err != nil {
		return err
	}

	b.thermostats = thermostats
	b.thermostatState = make(map[string]*ThermostatState, len(thermostats))
	b.thermostatIdx = make(map[string]*ThermostatEntity, len(thermostats))
	b.thermostatAddrIdx = make(map[int]*ThermostatEntity, len(thermostats))
	b.buildThermostatStateIndex()

	for i := range b.thermostats {
		e := &b.thermostats[i]
		b.thermostatState[e.UniqueID] = &ThermostatState{Mode: "off", FanMode: "auto"}
		b.thermostatIdx[e.RoomSlug+"/"+e.EntitySlug] = e
		if e.ThermostatAddr >= 0 {
			b.thermostatAddrIdx[e.ThermostatAddr] = e
		}
	}

	b.logger.Printf("discovered %d hvac components → %d thermostats", len(components), len(thermostats))
	return nil
}

func (b *Bridge) buildThermostatStateIndex() {
	_, updates := b.thermostatStateQueries()
	b.thermostatStateIdx = make(map[string][]thermostatStateQuery, len(updates))
	for _, u := range updates {
		b.thermostatStateIdx[u.stateName] = append(b.thermostatStateIdx[u.stateName], u)
	}
}

func (b *Bridge) hydrateThermostats(ctx context.Context) error {
	if len(b.thermostats) == 0 {
		return nil
	}

	stateNames, updates := b.thermostatStateQueries()
	b.logger.Printf("hydrating state for %d thermostats (%d unique states)...", len(b.thermostats), len(stateNames))

	states, err := b.api.FetchStates(ctx, stateNames)
	if err != nil {
		return err
	}

	b.mu.Lock()
	for _, u := range updates {
		val, ok := states[u.stateName]
		if !ok {
			continue
		}
		st := b.thermostatState[u.entity.UniqueID]
		applyHvacStateValue(u.entity, u.logicalKey, val, st)
		finalizeThermostatState(st)
	}
	b.mu.Unlock()

	b.logger.Printf("hydrated thermostat state from %d values", len(states))
	return nil
}

type thermostatStateQuery struct {
	stateName  string
	logicalKey string
	entity     *ThermostatEntity
}

func (b *Bridge) thermostatStateQueries() ([]string, []thermostatStateQuery) {
	seen := make(map[string]bool)
	var stateNames []string
	var updates []thermostatStateQuery

	for i := range b.thermostats {
		e := &b.thermostats[i]
		for logicalKey, stateName := range e.StateNames {
			if stateName == "" {
				continue
			}
			updates = append(updates, thermostatStateQuery{
				stateName:  stateName,
				logicalKey: logicalKey,
				entity:     e,
			})
			if !seen[stateName] {
				seen[stateName] = true
				stateNames = append(stateNames, stateName)
			}
		}
	}
	return stateNames, updates
}

func (b *Bridge) refreshThermostatStates(ctx context.Context) {
	if len(b.thermostats) == 0 {
		return
	}

	stateNames, updates := b.thermostatStateQueries()
	states, err := b.api.FetchStates(ctx, stateNames)
	if err != nil {
		b.logger.Printf("thermostat state refresh failed: %v", err)
		return
	}

	changed := false
	b.mu.Lock()
	for _, u := range updates {
		val, ok := states[u.stateName]
		if !ok {
			continue
		}
		st := b.thermostatState[u.entity.UniqueID]
		before := *st
		applyHvacStateValue(u.entity, u.logicalKey, val, st)
		finalizeThermostatState(st)
		if *st != before {
			changed = true
		}
	}
	b.mu.Unlock()

	if changed && b.mqtt != nil {
		b.publishAllThermostatStates()
	}
}

const thermostatPollInterval = 8 * time.Minute
const feedbackReconnectInterval = 8 * time.Minute

func (b *Bridge) thermostatPollLoop(ctx context.Context) {
	if len(b.thermostats) == 0 {
		return
	}
	b.logger.Printf("thermostat REST backup poll every %s", thermostatPollInterval)
	ticker := time.NewTicker(thermostatPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.refreshThermostatStates(ctx)
		}
	}
}

func (b *Bridge) connectFeedback(ctx context.Context) error {
	if len(b.thermostats) == 0 {
		return nil
	}

	client := NewFeedbackClient(b.cfg.Savant.Host, b.cfg.Savant.RESTPort)
	if err := client.Connect(ctx); err != nil {
		return err
	}

	stateNames, _ := b.thermostatStateQueries()
	if err := client.RegisterStates(stateNames); err != nil {
		client.Close()
		return err
	}

	b.feedbackMu.Lock()
	b.feedback = client
	b.feedbackMu.Unlock()
	return nil
}

func (b *Bridge) feedbackReconnectLoop(ctx context.Context) {
	if len(b.thermostats) == 0 {
		return
	}
	ticker := time.NewTicker(feedbackReconnectInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.logger.Println("periodic feedback reconnect")

			b.feedbackMu.Lock()
			old := b.feedback
			b.feedbackMu.Unlock()
			if old != nil {
				old.Close()
			}

			if err := b.connectFeedback(ctx); err != nil {
				b.logger.Printf("feedback reconnect failed: %v (will retry in %s)", err, feedbackReconnectInterval)
				continue
			}

			b.feedbackMu.Lock()
			fb := b.feedback
			b.feedbackMu.Unlock()
			go fb.ReadLoop(ctx, b.handleFeedbackStateUpdate)
		}
	}
}

func (b *Bridge) handleFeedbackStateUpdate(update FeedbackStateUpdate) {
	queries, ok := b.thermostatStateIdx[update.StateName]
	if !ok || len(queries) == 0 {
		return
	}

	changedEntities := make(map[string]bool)
	b.mu.Lock()
	for _, q := range queries {
		st := b.thermostatState[q.entity.UniqueID]
		applyHvacStateValue(q.entity, q.logicalKey, update.Value, st)
		finalizeThermostatState(st)
		changedEntities[q.entity.UniqueID] = true
	}
	b.mu.Unlock()

	if b.mqtt == nil {
		return
	}
	for uid := range changedEntities {
		for i := range b.thermostats {
			e := &b.thermostats[i]
			if e.UniqueID != uid {
				continue
			}
			st := b.thermostatState[uid]
			if err := b.mqtt.PublishThermostatState(e, *st); err != nil {
				b.logger.Printf("publish climate state error for %s: %v", uid, err)
			}
			break
		}
	}
}

func (b *Bridge) handleThermostatUpdate(update AVCStateUpdate) {
	addrStr := strings.TrimPrefix(update.State, "thermostat.")
	addr, err := strconv.ParseInt(addrStr, 0, 64)
	if err != nil {
		return
	}

	entity, ok := b.thermostatAddrIdx[int(addr)]
	if !ok {
		return
	}

	b.logger.Printf("thermostat update %s = %q (addr %d)", update.State, update.Value, addr)
	if b.applyAvcThermostatUpdate(entity, update.Value) {
		return
	}
	go b.refreshThermostatForEntity(context.Background(), entity)
}

// applyAvcThermostatUpdate tries to parse an avc thermostat push value in-place.
// Returns true if the update was applied and published.
func (b *Bridge) applyAvcThermostatUpdate(entity *ThermostatEntity, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "X" || value == "-1" {
		return false
	}

	// Some firmware may send a JSON object; try that first.
	var fields map[string]any
	if strings.HasPrefix(value, "{") {
		if err := json.Unmarshal([]byte(value), &fields); err == nil {
			return b.applyAvcThermostatFields(entity, fields)
		}
	}

	// Undocumented binary/text formats: not handled here.
	return false
}

func (b *Bridge) applyAvcThermostatFields(entity *ThermostatEntity, fields map[string]any) bool {
	changed := false
	b.mu.Lock()
	st := b.thermostatState[entity.UniqueID]
	for key, raw := range fields {
		logicalKey, ok := mapAvcFieldToLogicalKey(key)
		if !ok {
			continue
		}
		applyHvacStateValue(entity, logicalKey, formatStateValue(raw), st)
		changed = true
	}
	if changed {
		finalizeThermostatState(st)
	}
	snapshot := *st
	b.mu.Unlock()

	if !changed || b.mqtt == nil {
		return changed
	}
	if err := b.mqtt.PublishThermostatState(entity, snapshot); err != nil {
		b.logger.Printf("publish climate state error for %s: %v", entity.UniqueID, err)
	}
	return changed
}

func mapAvcFieldToLogicalKey(field string) (string, bool) {
	switch strings.ToLower(field) {
	case "mode", "thermostatmode":
		return hvacStateMode, true
	case "fanmode", "thermostatfanmode":
		return hvacStateFanMode, true
	case "currenttemperature", "thermostatcurrenttemperature":
		return hvacStateCurrentTemp, true
	case "heatpoint", "thermostatcurrentheatpoint":
		return hvacStateHeatSetpoint, true
	case "coolpoint", "thermostatcurrentcoolpoint":
		return hvacStateCoolSetpoint, true
	default:
		return "", false
	}
}

func (b *Bridge) refreshThermostatForEntity(ctx context.Context, entity *ThermostatEntity) {
	var stateNames []string
	var updates []thermostatStateQuery
	for logicalKey, stateName := range entity.StateNames {
		if stateName == "" {
			continue
		}
		stateNames = append(stateNames, stateName)
		updates = append(updates, thermostatStateQuery{
			stateName:  stateName,
			logicalKey: logicalKey,
			entity:     entity,
		})
	}
	if len(stateNames) == 0 {
		return
	}

	states, err := b.api.FetchStates(ctx, stateNames)
	if err != nil {
		b.logger.Printf("thermostat refresh for %s failed: %v", entity.Name, err)
		return
	}

	changed := false
	b.mu.Lock()
	st := b.thermostatState[entity.UniqueID]
	before := *st
	for _, u := range updates {
		val, ok := states[u.stateName]
		if !ok {
			continue
		}
		applyHvacStateValue(u.entity, u.logicalKey, val, st)
	}
	finalizeThermostatState(st)
	if *st != before {
		changed = true
	}
	snapshot := *st
	b.mu.Unlock()

	if changed && b.mqtt != nil {
		if err := b.mqtt.PublishThermostatState(entity, snapshot); err != nil {
			b.logger.Printf("publish climate state error for %s: %v", entity.UniqueID, err)
		}
	}
}

func (b *Bridge) handleClimateCommand(entityID string, cmd ClimateCommand) {
	entity, ok := b.thermostatIdx[entityID]
	if !ok {
		b.logger.Printf("climate command for unknown entity: %s", entityID)
		return
	}

	b.logger.Printf("climate command for %s (%s): %+v", entityID, entity.Name, cmd)

	b.mu.Lock()
	st := b.thermostatState[entity.UniqueID]
	applyClimateCommandOptimistic(st, cmd)
	optimistic := *st
	b.mu.Unlock()

	if b.mqtt != nil {
		if err := b.mqtt.PublishThermostatState(entity, optimistic); err != nil {
			b.logger.Printf("optimistic climate publish error for %s: %v", entity.UniqueID, err)
		}
	}

	ctx := context.Background()
	if err := b.sendHvacCommands(ctx, entity, cmd, &optimistic); err != nil {
		b.logger.Printf("hvac command error for %s: %v", entity.UniqueID, err)
	}

	go b.refreshThermostatStates(context.Background())
}

func applyClimateCommandOptimistic(st *ThermostatState, cmd ClimateCommand) {
	if cmd.Mode != nil {
		st.Mode = *cmd.Mode
	}
	if cmd.FanMode != nil {
		st.FanMode = *cmd.FanMode
	}
	if cmd.Temperature != nil {
		t := *cmd.Temperature
		st.Temperature = &t
		switch st.Mode {
		case "heat":
			st.TemperatureLow = &t
		case "cool":
			st.TemperatureHigh = &t
		}
	}
	if cmd.TemperatureLow != nil {
		t := *cmd.TemperatureLow
		st.TemperatureLow = &t
	}
	if cmd.TemperatureHigh != nil {
		t := *cmd.TemperatureHigh
		st.TemperatureHigh = &t
	}
	if cmd.TargetHumidity != nil {
		t := *cmd.TargetHumidity
		st.TargetHumidity = &t
	}
	finalizeThermostatState(st)
}

func (b *Bridge) sendHvacCommands(ctx context.Context, entity *ThermostatEntity, cmd ClimateCommand, st *ThermostatState) error {
	if cmd.Mode != nil {
		savantCmd := haModeToSavantCommand(*cmd.Mode)
		if savantCmd == "" {
			return fmt.Errorf("unsupported mode %q", *cmd.Mode)
		}
		if err := b.api.SendHvacCommand(ctx, entity.DeviceID, savantCmd, nil); err != nil {
			return err
		}
	}

	if cmd.FanMode != nil {
		savantCmd := haFanModeToSavantCommand(*cmd.FanMode)
		if savantCmd == "" {
			return fmt.Errorf("unsupported fan_mode %q", *cmd.FanMode)
		}
		if err := b.api.SendHvacCommand(ctx, entity.DeviceID, savantCmd, nil); err != nil {
			return err
		}
	}

	if cmd.Temperature != nil {
		if err := b.sendTemperatureCommand(ctx, entity, st.Mode, cmd.Temperature); err != nil {
			return err
		}
	}

	if cmd.TemperatureLow != nil {
		if err := b.api.SendHvacCommand(ctx, entity.DeviceID, "SetHeatPointTemperature", map[string]any{
			"HeatPointTemperature": int(*cmd.TemperatureLow),
		}); err != nil {
			return err
		}
	}

	if cmd.TemperatureHigh != nil {
		if err := b.api.SendHvacCommand(ctx, entity.DeviceID, "SetCoolPointTemperature", map[string]any{
			"CoolPointTemperature": int(*cmd.TemperatureHigh),
		}); err != nil {
			return err
		}
	}

	if cmd.TargetHumidity != nil {
		if err := b.api.SendHvacCommand(ctx, entity.DeviceID, "SetHumiditySetPoint", map[string]any{
			"HumidityPoint": int(*cmd.TargetHumidity),
		}); err != nil {
			return err
		}
	}

	if cmd.HumidityMode != nil {
		var savantCmd string
		switch strings.ToLower(*cmd.HumidityMode) {
		case "on", "auto", "humidify", "dehumidify":
			savantCmd = "SetHumidityModeOn"
		case "off":
			savantCmd = "SetHumidityModeOff"
		default:
			return fmt.Errorf("unsupported humidity_mode %q", *cmd.HumidityMode)
		}
		if err := b.api.SendHvacCommand(ctx, entity.DeviceID, savantCmd, nil); err != nil {
			return err
		}
	}

	return nil
}

func (b *Bridge) sendTemperatureCommand(ctx context.Context, entity *ThermostatEntity, mode string, temp *float64) error {
	if temp == nil {
		return nil
	}
	t := int(*temp)

	switch mode {
	case "cool":
		return b.api.SendHvacCommand(ctx, entity.DeviceID, "SetCoolPointTemperature", map[string]any{
			"CoolPointTemperature": t,
		})
	default:
		return b.api.SendHvacCommand(ctx, entity.DeviceID, "SetHeatPointTemperature", map[string]any{
			"HeatPointTemperature": t,
		})
	}
}

func (b *Bridge) publishAllThermostatStates() {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for i := range b.thermostats {
		e := &b.thermostats[i]
		st := b.thermostatState[e.UniqueID]
		if err := b.mqtt.PublishThermostatState(e, *st); err != nil {
			b.logger.Printf("publish climate state error for %s: %v", e.UniqueID, err)
		}
	}
}
