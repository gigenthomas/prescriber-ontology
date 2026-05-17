package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// Types the lineage page knows about for the type dropdown. Sourced from
// the controlled vocabulary at boot would be cleaner; this is fine for v1.
var lineagePageTypes = []string{"Prescriber", "Drug", "GenericDrug", "Specialty", "Location"}

type lineagePageData struct {
	Title          string
	Types          []string
	Type           string
	ExternalID     string
	HasResult      bool
	NotFound       bool
	Identity       lineageIdentity
	Source         lineageSource
	PipelineRuns   []lineagePipelineRun
	Actions        []lineageActionRow
	Events         []lineageEventRow
	CurrentState   string
	StateUpdatedAt string
}

type lineageIdentity struct {
	UUID           string
	Type           string
	ExternalID     string
	CanonicalLabel string
	CreatedAt      string
	UpdatedAt      string
	Version        int
	AttrsJSON      string
}

type lineageSource struct {
	Name    string
	URI     string
	Version string
}

type lineagePipelineRun struct {
	StartedAt  string
	Name       string
	Status     string
	Actor      string
	EventCount int
	InputsJSON string
}

type lineageActionRow struct {
	InvokedAt  string
	Action     string
	Actor      string
	Status     string
	Error      string
	ParamsJSON string
}

type lineageEventRow struct {
	OccurredAt      string
	Topic           string
	Op              string
	PipelineRunName string
	ActionName      string
}

func lineageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	entityType := strings.TrimSpace(r.URL.Query().Get("type"))
	externalID := strings.TrimSpace(r.URL.Query().Get("external_id"))
	if entityType == "" {
		entityType = "Prescriber"
	}

	data := lineagePageData{
		Title:      "Entity",
		Types:      lineagePageTypes,
		Type:       entityType,
		ExternalID: externalID,
	}

	if externalID != "" {
		j, err := doEntityLineage(ctx, externalID, entityType, 50)
		if err != nil {
			data.NotFound = true
		} else {
			var raw struct {
				Identity struct {
					ID             string         `json:"id"`
					Type           string         `json:"type"`
					ExternalID     string         `json:"external_id"`
					CanonicalLabel string         `json:"canonical_label"`
					CreatedAt      string         `json:"created_at"`
					UpdatedAt      string         `json:"updated_at"`
					Version        int            `json:"version"`
					Attrs          any            `json:"attrs"`
				} `json:"identity"`
				Source struct {
					Name    string `json:"name"`
					URI     string `json:"uri"`
					Version string `json:"version"`
				} `json:"source"`
				PipelineRuns []struct {
					Name        string         `json:"name"`
					StartedAt   string         `json:"started_at"`
					Status      string         `json:"status"`
					Actor       string         `json:"actor"`
					Inputs      map[string]any `json:"inputs"`
					EventCount  int            `json:"event_count"`
				} `json:"pipeline_runs"`
				Actions []struct {
					InvokedAt string         `json:"invoked_at"`
					Action    string         `json:"action"`
					Actor     string         `json:"actor"`
					Status    string         `json:"status"`
					Error     string         `json:"error"`
					Params    map[string]any `json:"params"`
				} `json:"actions"`
				Events []struct {
					OccurredAt      string `json:"occurred_at"`
					Topic           string `json:"topic"`
					Op              string `json:"op"`
					PipelineRunName string `json:"pipeline_run_name"`
					ActionName      string `json:"action_name"`
				} `json:"events"`
				CurrentState   any    `json:"current_state"`
				StateUpdatedAt string `json:"state_updated_at"`
			}
			if err := json.Unmarshal([]byte(j), &raw); err != nil {
				log.Printf("lineage decode: %v", err)
				data.NotFound = true
			} else {
				data.HasResult = true
				data.Identity = lineageIdentity{
					UUID:           raw.Identity.ID,
					Type:           raw.Identity.Type,
					ExternalID:     raw.Identity.ExternalID,
					CanonicalLabel: raw.Identity.CanonicalLabel,
					CreatedAt:      formatTime(raw.Identity.CreatedAt),
					UpdatedAt:      formatTime(raw.Identity.UpdatedAt),
					Version:        raw.Identity.Version,
					AttrsJSON:      indentJSON(raw.Identity.Attrs),
				}
				data.Source = lineageSource{
					Name:    raw.Source.Name,
					URI:     raw.Source.URI,
					Version: raw.Source.Version,
				}
				for _, pr := range raw.PipelineRuns {
					data.PipelineRuns = append(data.PipelineRuns, lineagePipelineRun{
						StartedAt:  formatTime(pr.StartedAt),
						Name:       pr.Name,
						Status:     pr.Status,
						Actor:      pr.Actor,
						EventCount: pr.EventCount,
						InputsJSON: indentJSON(pr.Inputs),
					})
				}
				for _, a := range raw.Actions {
					data.Actions = append(data.Actions, lineageActionRow{
						InvokedAt:  formatTime(a.InvokedAt),
						Action:     a.Action,
						Actor:      a.Actor,
						Status:     a.Status,
						Error:      a.Error,
						ParamsJSON: indentJSON(a.Params),
					})
				}
				for _, e := range raw.Events {
					data.Events = append(data.Events, lineageEventRow{
						OccurredAt:      formatTime(e.OccurredAt),
						Topic:           e.Topic,
						Op:              e.Op,
						PipelineRunName: e.PipelineRunName,
						ActionName:      e.ActionName,
					})
				}
				if raw.CurrentState != nil {
					data.CurrentState = indentJSON(raw.CurrentState)
				}
				if raw.StateUpdatedAt != "" {
					data.StateUpdatedAt = formatTime(raw.StateUpdatedAt)
				}
				if raw.Identity.CanonicalLabel != "" {
					data.Title = raw.Identity.CanonicalLabel
				}
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, "lineage.html", data); err != nil {
		log.Printf("render lineage.html: %v", err)
	}
}

func formatTime(rfc string) string {
	if rfc == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return rfc
	}
	return t.Format("2006-01-02 15:04:05")
}

func indentJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
