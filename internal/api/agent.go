package api

import (
	"net/http"
)

type assignmentTargetDTO struct {
	Name   string            `json:"name"`
	Probe  string            `json:"probe"`
	Host   string            `json:"host"`
	Params map[string]string `json:"params,omitempty"`
	StepMs int64             `json:"step_ms"`
	Pings  int               `json:"pings"`
}

type assignmentDTO struct {
	Vantage       string                `json:"vantage"`
	ConfigVersion string                `json:"config_version"`
	Targets       []assignmentTargetDTO `json:"targets"`
}

// agentAssignment serves the calling vantage its target list. The vantage identity
// comes from the authenticated key (requireAgent), never from a query param, so a
// key can only ever fetch its own assignment. config_version is served as an ETag;
// an agent that replays it in If-None-Match gets 304.
func (srv *Server) agentAssignment(w http.ResponseWriter, r *http.Request) {
	v := vantageFrom(r)
	monitors, cv := srv.Assignment(v)
	if match := r.Header.Get("If-None-Match"); match != "" && match == cv {
		w.Header().Set("ETag", cv)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	out := assignmentDTO{Vantage: v, ConfigVersion: cv, Targets: make([]assignmentTargetDTO, 0, len(monitors))}
	for _, m := range monitors {
		out.Targets = append(out.Targets, assignmentTargetDTO{
			Name: m.Name, Probe: m.ProbeKind, Host: m.Host,
			Params: m.Params, StepMs: m.Step.Milliseconds(), Pings: m.Pings,
		})
	}
	w.Header().Set("ETag", cv)
	writeJSON(w, out)
}
