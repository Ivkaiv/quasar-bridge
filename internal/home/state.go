package home

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"slices"
	"strings"
)

// Only climate metadata used for planning and confirmation is retained. The
// target temperature must never be presented as a measured room temperature.
type climateAttributes struct {
	Temperature        *float64 `json:"temperature,omitempty"`
	CurrentTemperature *float64 `json:"current_temperature,omitempty"`
	MinTemp            *float64 `json:"min_temp,omitempty"`
	MaxTemp            *float64 `json:"max_temp,omitempty"`
	TemperatureUnit    string   `json:"temperature_unit,omitempty"`
	HVACModes          []string `json:"hvac_modes,omitempty"`
}

type deviceState struct {
	State      string            `json:"state"`
	Attributes climateAttributes `json:"attributes"`
}

func (c *Controller) readState(ctx context.Context, id string) (deviceState, error) {
	raw, err := c.Tools.Call(ctx, "ha_get_state", map[string]any{"entity_id": id, "fields": []string{"entity_id", "state", "attributes"}})
	if err != nil {
		return deviceState{}, err
	}
	var result struct {
		Data struct {
			State      string         `json:"state"`
			Attributes jsontext.Value `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return deviceState{}, errors.New("invalid home device state")
	}
	state := deviceState{State: result.Data.State}
	if strings.HasPrefix(id, "climate.") && len(result.Data.Attributes) > 0 {
		if err := json.Unmarshal(result.Data.Attributes, &state.Attributes); err != nil {
			return deviceState{}, errors.New("invalid home climate attributes")
		}
	}

	return state, nil
}

func validEntityID(id string) bool {
	domain, name, ok := strings.Cut(id, ".")
	if !ok || domain == "" || name == "" {
		return false
	}
	for _, part := range []string{domain, name} {
		for _, r := range part {
			if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}

func validateTemperature(a Action, attrs climateAttributes) error {
	if a.Domain != "climate" || a.Service != "set_temperature" {
		return nil
	}
	target, ok := a.Data["temperature"].(float64)
	if !ok || math.IsNaN(target) || math.IsInf(target, 0) {
		return errors.New("invalid home temperature target")
	}
	if mode, ok := a.Data["hvac_mode"]; ok {
		if value, ok := mode.(string); !ok || !slices.Contains(attrs.HVACModes, value) {
			return errors.New("invalid home HVAC mode")
		}
	}
	// HA climate entities advertise their own limits. Guessing a universal
	// range would be unsafe for devices using different units or capabilities.
	if attrs.MinTemp == nil || attrs.MaxTemp == nil || math.IsNaN(*attrs.MinTemp) || math.IsNaN(*attrs.MaxTemp) || math.IsInf(*attrs.MinTemp, 0) || math.IsInf(*attrs.MaxTemp, 0) || *attrs.MinTemp > *attrs.MaxTemp {
		return errors.New("home temperature limits unavailable")
	}
	if target < *attrs.MinTemp || target > *attrs.MaxTemp {
		return errors.New("home temperature target outside device limits")
	}
	return nil
}

func temperatureMatches(value *float64, target float64) bool {
	return value != nil && math.Abs(*value-target) < 0.001
}

// HA's wait observes entity state, so attribute-only changes must use their own
// confirmation. Keep upstream brightness normalization, including zero -> off.
func expectedState(a Action) (string, bool) {
	if (a.Service != "turn_on" && a.Service != "turn_off") || (a.Domain != "light" && a.Domain != "switch" && a.Domain != "fan") {
		return "", false
	}
	want := strings.TrimPrefix(a.Service, "turn_")
	if a.Domain == "light" && a.Service == "turn_on" {
		brightness := a.Data["brightness"]
		if value, ok := brightness.(float64); ok {
			brightness = min(255, max(0, math.Trunc(value)))
		}
		if pct, ok := a.Data["brightness_pct"].(float64); ok {
			brightness = math.RoundToEven(255 * pct / 100)
		}
		if brightness == float64(0) {
			want = "off"
		}
	}
	return want, true
}
