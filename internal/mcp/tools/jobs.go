package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// ---------- listJobs ----------

const listJobsInputSchema = `{
  "type": "object",
  "properties": {
    "stateFilter": {
      "type": "string",
      "enum": ["running", "succeeded", "failed", "canceled"],
      "description": "Optional filter; absent returns every job."
    }
  },
  "additionalProperties": false
}`

const listJobsOutputSchema = `{
  "type": "object",
  "properties": {
    "jobs": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name":          {"type": "string"},
          "kind":          {"type": "string"},
          "title":         {"type": "string"},
          "state":         {"type": "string"},
          "progress":      {"type": "number", "minimum": 0, "maximum": 1},
          "detail":        {"type": "string"},
          "startTime":     {"type": "string"},
          "endTime":       {"type": "string"},
          "durationMs":    {"type": "integer", "minimum": 0},
          "errorMessage":  {"type": "string"}
        },
        "required": ["name", "state"],
        "additionalProperties": false
      }
    }
  },
  "required": ["jobs"],
  "additionalProperties": false
}`

type listJobsInput struct {
	StateFilter string `json:"stateFilter,omitempty"`
}

type listJobsOutput struct {
	Jobs []jobOut `json:"jobs"`
}

type jobOut struct {
	Name         string  `json:"name"`
	Kind         string  `json:"kind,omitempty"`
	Title        string  `json:"title,omitempty"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress,omitempty"`
	Detail       string  `json:"detail,omitempty"`
	StartTime    string  `json:"startTime,omitempty"`
	EndTime      string  `json:"endTime,omitempty"`
	DurationMs   int64   `json:"durationMs,omitempty"`
	ErrorMessage string  `json:"errorMessage,omitempty"`
}

// ListJobs is the MCP tool returning the current job registry contents.
type ListJobs struct{ reg *jobs.Registry }

// NewListJobs constructs the tool. A nil registry returns an empty list.
func NewListJobs(r *jobs.Registry) *ListJobs { return &ListJobs{reg: r} }

// Name implements mcp.Tool.
func (*ListJobs) Name() string { return "listJobs" }

// Description implements mcp.Tool.
func (*ListJobs) Description() string {
	return "Return the currently-tracked background jobs (running and recent history), optionally filtered by state."
}

// InputSchema implements mcp.Tool.
func (*ListJobs) InputSchema() json.RawMessage { return json.RawMessage(listJobsInputSchema) }

// OutputSchema implements mcp.Tool.
func (*ListJobs) OutputSchema() json.RawMessage { return json.RawMessage(listJobsOutputSchema) }

// Invoke implements mcp.Tool.
func (l *ListJobs) Invoke(_ context.Context, req json.RawMessage) (json.RawMessage, error) {
	if l.reg == nil {
		body, err := json.Marshal(listJobsOutput{Jobs: []jobOut{}})
		if err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("marshal output: %w", err))
		}
		return body, nil
	}
	var in listJobsInput
	if len(req) > 0 {
		if err := json.Unmarshal(req, &in); err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("decode arguments: %w", err))
		}
	}
	out := listJobsOutput{Jobs: []jobOut{}}
	for _, j := range l.reg.List() {
		if in.StateFilter != "" && string(j.State) != in.StateFilter {
			continue
		}
		end := ""
		if !j.EndTime.IsZero() {
			end = j.EndTime.UTC().Format(time.RFC3339)
		}
		dur := int64(0)
		if !j.EndTime.IsZero() {
			dur = j.EndTime.Sub(j.StartTime).Milliseconds()
		} else {
			dur = time.Since(j.StartTime).Milliseconds()
		}
		out.Jobs = append(out.Jobs, jobOut{
			Name:         "jobs/" + j.UID,
			Kind:         j.Kind,
			Title:        j.Title,
			State:        string(j.State),
			Progress:     j.Progress,
			Detail:       j.Detail,
			StartTime:    j.StartTime.UTC().Format(time.RFC3339),
			EndTime:      end,
			DurationMs:   dur,
			ErrorMessage: j.ErrMsg,
		})
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal output: %w", err))
	}
	return body, nil
}

// ---------- cancelJob ----------

const cancelJobInputSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "pattern": "^jobs/[A-Z0-9]{26}$", "description": "Resource name of the job to cancel: \"jobs/{uid}\"."}
  },
  "required": ["name"],
  "additionalProperties": false
}`

const cancelJobOutputSchema = `{
  "type": "object",
  "properties": {
    "name":  {"type": "string"},
    "state": {"type": "string"}
  },
  "required": ["name", "state"],
  "additionalProperties": false
}`

type cancelJobInput struct {
	Name string `json:"name"`
}

type cancelJobOutput struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// CancelJob is the MCP tool that cancels a tracked job by name.
type CancelJob struct{ reg *jobs.Registry }

// NewCancelJob constructs the tool.
func NewCancelJob(r *jobs.Registry) *CancelJob { return &CancelJob{reg: r} }

// Name implements mcp.Tool.
func (*CancelJob) Name() string { return "cancelJob" }

// Description implements mcp.Tool.
func (*CancelJob) Description() string {
	return "Cancel a tracked background job by its AIP resource name (\"jobs/{uid}\")."
}

// InputSchema implements mcp.Tool.
func (*CancelJob) InputSchema() json.RawMessage { return json.RawMessage(cancelJobInputSchema) }

// OutputSchema implements mcp.Tool.
func (*CancelJob) OutputSchema() json.RawMessage { return json.RawMessage(cancelJobOutputSchema) }

// Invoke implements mcp.Tool.
func (c *CancelJob) Invoke(_ context.Context, req json.RawMessage) (json.RawMessage, error) {
	if c.reg == nil {
		return nil, mcp.NewInternalError(errors.New("jobs registry not wired"))
	}
	var in cancelJobInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("decode arguments: %w", err))
	}
	uid := strings.TrimPrefix(in.Name, "jobs/")
	if uid == in.Name || uid == "" {
		return nil, mcp.NewValidationError(
			"name must be of the form \"jobs/{uid}\"", nil)
	}
	if !c.reg.Cancel(uid) {
		return nil, mcp.NewValidationError("no running job with that name", nil)
	}
	j, _ := c.reg.Get(uid)
	body, err := json.Marshal(cancelJobOutput{
		Name:  in.Name,
		State: string(j.State),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal output: %w", err))
	}
	return body, nil
}
