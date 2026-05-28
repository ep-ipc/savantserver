package main

import (
	"encoding/json"
	"fmt"
	"strings"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// climateDiscoveryPayload is the HA MQTT Discovery config for a climate entity.
type climateDiscoveryPayload struct {
	Name              string          `json:"name"`
	UniqueID          string          `json:"unique_id"`
	ObjectID          string          `json:"object_id"`
	Modes             []string        `json:"modes"`
	FanModes          []string        `json:"fan_modes,omitempty"`
	TempUnit          string          `json:"temperature_unit"`
	MinTemp           float64         `json:"min_temp"`
	MaxTemp           float64         `json:"max_temp"`
	TempStep          float64         `json:"temp_step"`
	Precision         float64         `json:"precision"`
	StateTopic        string          `json:"state_topic"`
	ModeStateTemplate string          `json:"mode_state_template"`
	ModeCommandTopic  string          `json:"mode_command_topic"`
	ModeCommandTemplate string        `json:"mode_command_template"`
	CurrentTempTopic  string          `json:"current_temperature_topic"`
	CurrentTempTemplate string        `json:"current_temperature_template"`
	TempStateTemplate string          `json:"temperature_state_template"`
	TempCommandTopic  string          `json:"temperature_command_topic"`
	TempCommandTemplate string        `json:"temperature_command_template"`
	TempLowStateTemplate string       `json:"temperature_low_state_template,omitempty"`
	TempLowCommandTopic string        `json:"temperature_low_command_topic,omitempty"`
	TempLowCommandTemplate string     `json:"temperature_low_command_template,omitempty"`
	TempHighStateTemplate string      `json:"temperature_high_state_template,omitempty"`
	TempHighCommandTopic string       `json:"temperature_high_command_topic,omitempty"`
	TempHighCommandTemplate string    `json:"temperature_high_command_template,omitempty"`
	FanModeStateTemplate string       `json:"fan_mode_state_template,omitempty"`
	FanModeCommandTopic string        `json:"fan_mode_command_topic,omitempty"`
	FanModeCommandTemplate string     `json:"fan_mode_command_template,omitempty"`
	TargetHumidityStateTemplate string `json:"target_humidity_state_template,omitempty"`
	TargetHumidityCommandTopic string `json:"target_humidity_command_topic,omitempty"`
	TargetHumidityCommandTemplate string `json:"target_humidity_command_template,omitempty"`
	AvailabilityTopic string          `json:"availability_topic"`
	PayloadAvailable  string          `json:"payload_available"`
	PayloadNotAvail   string          `json:"payload_not_available"`
	Device            discoveryDevice `json:"device"`
}

// PublishThermostatDiscovery sends HA MQTT Discovery configs for thermostats.
func (m *MQTTClient) PublishThermostatDiscovery(entities []ThermostatEntity) error {
	for i := range entities {
		entity := &entities[i]
		payload, err := buildClimateDiscoveryPayload(entity, m.topicPrefix)
		if err != nil {
			return fmt.Errorf("building climate discovery for %s: %w", entity.UniqueID, err)
		}
		topic := entity.DiscoveryTopic(m.haPrefix)
		token := m.client.Publish(topic, 1, true, payload)
		token.Wait()
		if err := token.Error(); err != nil {
			return fmt.Errorf("publishing climate discovery for %s: %w", entity.UniqueID, err)
		}
	}
	if len(entities) > 0 {
		m.logger.Printf("Published discovery for %d thermostats", len(entities))
	}
	return nil
}

// PublishThermostatState publishes the current state of a thermostat entity.
func (m *MQTTClient) PublishThermostatState(entity *ThermostatEntity, state ThermostatState) error {
	payload, err := buildClimateStatePayload(state)
	if err != nil {
		return fmt.Errorf("building climate state for %s: %w", entity.UniqueID, err)
	}
	topic := entity.StateTopic(m.topicPrefix)
	token := m.client.Publish(topic, 0, true, payload)
	token.Wait()
	return token.Error()
}

// SubscribeClimateCommands subscribes to thermostat command topics.
func (m *MQTTClient) SubscribeClimateCommands(handler func(entityID string, cmd ClimateCommand)) error {
	topic := m.topicPrefix + "/+/+/climate/set"
	token := m.client.Subscribe(topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
		entityID, err := parseClimateCommandTopic(msg.Topic(), m.topicPrefix)
		if err != nil {
			m.logger.Printf("Ignoring message on %s: %v", msg.Topic(), err)
			return
		}

		var cmd ClimateCommand
		if err := json.Unmarshal(msg.Payload(), &cmd); err != nil {
			m.logger.Printf("Invalid climate command on %s: %v", msg.Topic(), err)
			return
		}

		handler(entityID, cmd)
	})
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("subscribing to %s: %w", topic, err)
	}
	m.logger.Printf("Subscribed to %s", topic)
	return nil
}

func buildClimateDiscoveryPayload(entity *ThermostatEntity, topicPrefix string) ([]byte, error) {
	cmdTopic := entity.CommandTopic(topicPrefix)
	stateTopic := entity.StateTopic(topicPrefix)

	p := climateDiscoveryPayload{
		Name:              entity.Name,
		UniqueID:          entity.UniqueID,
		ObjectID:          entity.UniqueID,
		Modes:             entity.Modes,
		FanModes:          entity.FanModes,
		TempUnit:          "F",
		MinTemp:           45,
		MaxTemp:           95,
		TempStep:          1,
		Precision:         1,
		StateTopic:        stateTopic,
		ModeStateTemplate: "{{ value_json.mode }}",
		ModeCommandTopic:  cmdTopic,
		ModeCommandTemplate: `{"mode":"{{ value }}"}`,
		CurrentTempTopic:  stateTopic,
		CurrentTempTemplate: "{{ value_json.current_temperature }}",
		TempStateTemplate: "{{ value_json.temperature }}",
		TempCommandTopic:  cmdTopic,
		TempCommandTemplate: `{"temperature":{{ value }}}`,
		TempLowStateTemplate: "{{ value_json.temperature_low }}",
		TempLowCommandTopic: cmdTopic,
		TempLowCommandTemplate: `{"temperature_low":{{ value }}}`,
		TempHighStateTemplate: "{{ value_json.temperature_high }}",
		TempHighCommandTopic: cmdTopic,
		TempHighCommandTemplate: `{"temperature_high":{{ value }}}`,
		FanModeStateTemplate: "{{ value_json.fan_mode }}",
		FanModeCommandTopic: cmdTopic,
		FanModeCommandTemplate: `{"fan_mode":"{{ value }}"}`,
		AvailabilityTopic: topicPrefix + "/status",
		PayloadAvailable:  "online",
		PayloadNotAvail:   "offline",
		Device: discoveryDevice{
			Identifiers:   []string{entity.UniqueID},
			Name:          entity.RoomName + " " + entity.Name,
			Manufacturer:  entity.Manufacturer,
			Model:         entity.DeviceModel,
			SuggestedArea: entity.RoomName,
		},
	}

	if entity.SupportsHumidity {
		p.TargetHumidityStateTemplate = "{{ value_json.target_humidity }}"
		p.TargetHumidityCommandTopic = cmdTopic
		p.TargetHumidityCommandTemplate = `{"target_humidity":{{ value }}}`
	}

	return json.Marshal(p)
}

func buildClimateStatePayload(state ThermostatState) ([]byte, error) {
	finalizeThermostatState(&state)
	return json.Marshal(state.toPayload())
}

func finalizeThermostatState(st *ThermostatState) {
	switch st.Mode {
	case "heat":
		if st.TemperatureLow != nil {
			t := *st.TemperatureLow
			st.Temperature = &t
		}
	case "cool":
		if st.TemperatureHigh != nil {
			t := *st.TemperatureHigh
			st.Temperature = &t
		}
	case "heat_cool":
		// temperature_low/high used directly
	default:
		st.Temperature = nil
	}
}

// parseClimateCommandTopic extracts room_slug/entity_slug from a climate command topic.
func parseClimateCommandTopic(topic string, prefix string) (string, error) {
	if !strings.HasPrefix(topic, prefix+"/") {
		return "", fmt.Errorf("topic %q does not start with prefix %q", topic, prefix)
	}
	rest := topic[len(prefix)+1:]
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[2] != "climate" || parts[3] != "set" {
		return "", fmt.Errorf("unexpected topic format: %q", topic)
	}
	return parts[0] + "/" + parts[1], nil
}
