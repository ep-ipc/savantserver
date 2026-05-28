package main

import (
	"context"
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

func (b *Bridge) thermostatPollLoop(ctx context.Context) {
	if len(b.thermostats) == 0 {
		return
	}
	ticker := time.NewTicker(45 * time.Second)
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

func (b *Bridge) handleThermostatUpdate(update AVCStateUpdate) {
	addrStr := strings.TrimPrefix(update.State, "thermostat.")
	addr, err := strconv.ParseInt(addrStr, 0, 64)
	if err != nil {
		return
	}

	if _, ok := b.thermostatAddrIdx[int(addr)]; !ok {
		return
	}

	b.logger.Printf("thermostat update %s = %s (addr %d)", update.State, update.Value, addr)
	// avc thermostat push format is not documented; trigger a full REST refresh.
	go b.refreshThermostatStates(context.Background())
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
