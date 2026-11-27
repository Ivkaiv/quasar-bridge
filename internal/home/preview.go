package home

import (
	"context"
	"strings"
)

// ScopeEntity omits state values: this diagnostic inspects selection, not home activity.
type ScopeEntity struct {
	ID   string `json:"entity_id"`
	Name string `json:"name"`
	Area string `json:"area"`
}

type ScopePreview struct {
	DefaultArea   string        `json:"default_area"`
	SelectedAreas []string      `json:"selected_areas"`
	Source        string        `json:"source"`
	Answer        string        `json:"answer,omitempty"`
	DirectHandled bool          `json:"direct_handled"`
	Actions       []Action      `json:"proposed_actions"`
	Entities      []ScopeEntity `json:"entities"`
}

// PreviewScope only calls ha_search. It never invokes the planner or a service.
func (c *Controller) PreviewScope(ctx context.Context, text, request string) (ScopePreview, error) {
	result := ScopePreview{DefaultArea: c.DefaultArea, Entities: []ScopeEntity{}, Actions: []Action{}}
	inventory := Controller{Tools: c.Tools, Domains: c.Domains}
	if len(inventory.Domains) == 0 {
		inventory.Domains = controlDomains(strings.TrimSpace(request + " " + text))
	}
	entities, err := inventory.Entities(ctx)
	if err != nil {
		return result, err
	}
	selection := areaSelection{entities: scopeEntities(request+" "+text, entities)}
	if c.DefaultArea != "" {
		selection = c.selectArea(text, request, entities)
	}
	result.SelectedAreas, result.Source, result.Answer = selection.areas, selection.source, selection.answer
	for _, e := range selection.entities {
		result.Entities = append(result.Entities, ScopeEntity{e.ID, e.Name, e.Area})
	}
	if result.Answer == "" && c.DefaultArea != "" {
		plan, handled := c.scopedPlan(text, request, selection)
		result.DirectHandled = handled && (c.Direct || len(plan.Actions) == 0)
		if result.DirectHandled {
			result.Actions, result.Answer = plan.Actions, plan.Answer
			known := make(map[string]Entity, len(selection.entities))
			for _, entity := range selection.entities {
				known[entity.ID] = entity
			}
			if len(selection.areas) > 0 {
				if answer := scopedGroupAnswer(plan, known); answer != "" {
					result.Actions, result.Answer = []Action{}, answer
				}
			}
		}
	}
	return result, nil
}
