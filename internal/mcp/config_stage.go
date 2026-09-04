package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/seitzbg/heliograph/internal/config"
	"gopkg.in/yaml.v3"
)

type docRoot struct {
	Targets *config.Node `json:"targets"`
}

// mutateDoc parses the targets wrapper, applies fn to the root node, and remarshals.
func mutateDoc(doc json.RawMessage, fn func(root *config.Node) error) (json.RawMessage, error) {
	var dr docRoot
	if err := json.Unmarshal(doc, &dr); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfigInvalid, err)
	}
	if dr.Targets == nil {
		dr.Targets = &config.Node{}
	}
	if dr.Targets.Children == nil {
		dr.Targets.Children = map[string]*config.Node{}
	}
	if err := fn(dr.Targets); err != nil {
		return nil, err
	}
	return json.Marshal(dr)
}

// yamlScalar builds a scalar yaml.Node for config.Duration.UnmarshalYAML.
func yamlScalar(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: s} }

func ensureGroup(root *config.Node, groupPath string) *config.Node {
	n := root
	if strings.TrimSpace(groupPath) == "" {
		return n
	}
	for _, seg := range strings.Split(groupPath, "/") {
		if n.Children == nil {
			n.Children = map[string]*config.Node{}
		}
		if n.Children[seg] == nil {
			n.Children[seg] = &config.Node{}
		}
		n = n.Children[seg]
	}
	return n
}

// findNode locates a host node by id (matches Node.ID) or by "a/b/c" path.
func findNode(root *config.Node, ref string) (parent *config.Node, name string, node *config.Node, ok bool) {
	// by id
	var byID func(p *config.Node, key string, n *config.Node) bool
	byID = func(p *config.Node, key string, n *config.Node) bool {
		if n == nil {
			return false
		}
		if n.Host != "" && n.ID == ref {
			parent, name, node, ok = p, key, n, true
			return true
		}
		for k, ch := range n.Children {
			if byID(n, k, ch) {
				return true
			}
		}
		return false
	}
	for k, ch := range root.Children {
		if byID(root, k, ch) {
			return
		}
	}
	// by path
	segs := strings.Split(ref, "/")
	p, n := root, root
	for i, seg := range segs {
		if n.Children == nil || n.Children[seg] == nil {
			return nil, "", nil, false
		}
		p = n
		n = n.Children[seg]
		if i == len(segs)-1 {
			return p, seg, n, true
		}
	}
	return nil, "", nil, false
}

func pruneEmpty(root *config.Node) {
	var clean func(n *config.Node)
	clean = func(n *config.Node) {
		for k, ch := range n.Children {
			clean(ch)
			if ch.Host == "" && len(ch.Children) == 0 {
				delete(n.Children, k)
			}
		}
	}
	clean(root)
}

type addTargetIn struct {
	GroupPath string            `json:"group_path,omitempty" jsonschema:"slash-separated group path, e.g. Websites or Resolvers/dns1 (created if missing)"`
	Name      string            `json:"name" jsonschema:"leaf name for the target within the group"`
	Host      string            `json:"host" jsonschema:"host or address to probe"`
	Probe     string            `json:"probe" jsonschema:"probe kind, e.g. Ping, HTTP, DNS, NTP"`
	Params    map[string]string `json:"params,omitempty" jsonschema:"probe-specific params"`
	Title     string            `json:"title,omitempty"`
	Step      string            `json:"step,omitempty" jsonschema:"per-target polling interval as a Go duration, e.g. 60s"`
	Pings     int               `json:"pings,omitempty"`
	Measure   string            `json:"measure,omitempty" jsonschema:"NTP only: rtt or offset"`
	Vantages  []string          `json:"vantages,omitempty" jsonschema:"vantage names that should probe this target"`
}

func stageAddTarget(st *staging, in addTargetIn) error {
	if in.Name == "" || in.Host == "" || in.Probe == "" {
		return fmt.Errorf("%w: name, host and probe are required", ErrConfigInvalid)
	}
	return st.mutate(func(root *config.Node) error {
		grp := ensureGroup(root, in.GroupPath)
		if grp.Children == nil {
			grp.Children = map[string]*config.Node{}
		}
		if _, exists := grp.Children[in.Name]; exists {
			return fmt.Errorf("%w: %s already exists under %q", ErrConfigInvalid, in.Name, in.GroupPath)
		}
		node := &config.Node{Host: in.Host, Probe: in.Probe, Title: in.Title, Pings: in.Pings, Params: in.Params, Vantages: in.Vantages}
		if in.Measure != "" {
			if node.Params == nil {
				node.Params = map[string]string{}
			}
			node.Params["measure"] = in.Measure
		}
		if in.Step != "" {
			if err := node.Step.UnmarshalYAML(yamlScalar(in.Step)); err != nil {
				return fmt.Errorf("%w: bad step %q: %v", ErrConfigInvalid, in.Step, err)
			}
		}
		grp.Children[in.Name] = node
		return nil
	})
}

type editTargetIn struct {
	Target       string            `json:"target" jsonschema:"target id or path to edit"`
	Host         string            `json:"host,omitempty"`
	Probe        string            `json:"probe,omitempty"`
	Params       map[string]string `json:"params,omitempty" jsonschema:"replaces the params map when given"`
	Title        string            `json:"title,omitempty"`
	Step         string            `json:"step,omitempty" jsonschema:"per-target polling interval as a Go duration, e.g. 60s"`
	Pings        int               `json:"pings,omitempty"`
	Measure      string            `json:"measure,omitempty"`
	Vantages     []string          `json:"vantages,omitempty" jsonschema:"replaces the vantage list when given"`
	NewName      string            `json:"new_name,omitempty" jsonschema:"rename the leaf"`
	NewGroupPath string            `json:"new_group_path,omitempty" jsonschema:"move under a different group path"`
}

func stageEditTarget(st *staging, in editTargetIn) error {
	return st.mutate(func(root *config.Node) error {
		parent, name, node, ok := findNode(root, in.Target)
		if !ok || node.Host == "" {
			return fmt.Errorf("%w: target %q not found", ErrConfigInvalid, in.Target)
		}
		if in.Host != "" {
			node.Host = in.Host
		}
		if in.Probe != "" {
			node.Probe = in.Probe
		}
		if in.Params != nil {
			node.Params = in.Params
		}
		if in.Title != "" {
			node.Title = in.Title
		}
		if in.Step != "" {
			if err := node.Step.UnmarshalYAML(yamlScalar(in.Step)); err != nil {
				return fmt.Errorf("%w: bad step %q: %v", ErrConfigInvalid, in.Step, err)
			}
		}
		if in.Pings != 0 {
			node.Pings = in.Pings
		}
		if in.Vantages != nil {
			node.Vantages = in.Vantages
		}
		if in.Measure != "" {
			if node.Params == nil {
				node.Params = map[string]string{}
			}
			node.Params["measure"] = in.Measure
		}
		// move/rename: detach then reattach (keeps ID → stable identity)
		if in.NewName != "" || in.NewGroupPath != "" {
			dstName := name
			if in.NewName != "" {
				dstName = in.NewName
			}
			dst := parent
			if in.NewGroupPath != "" {
				dst = ensureGroup(root, in.NewGroupPath)
			}
			// Guard the destination slot: a no-op rename/move (the slot already holds
			// this same node) is fine, but landing on a DIFFERENT target's name would
			// silently overwrite it — its whole subtree and stable ID — with nothing
			// downstream catching it (validateDoc/Monitors only ever see the survivor).
			// Check before mutating anything so a rejected move leaves the working doc
			// untouched (the error propagates out of mutateDoc, so st.mutate never stores
			// the doc it built).
			if existing, ok := dst.Children[dstName]; ok && existing != node {
				return fmt.Errorf("%w: cannot move/rename %q: %q already exists in the destination group", ErrConfigInvalid, in.Target, dstName)
			}
			delete(parent.Children, name)
			if dst.Children == nil {
				dst.Children = map[string]*config.Node{}
			}
			dst.Children[dstName] = node
		}
		pruneEmpty(root)
		return nil
	})
}

func stageRemoveTarget(st *staging, ref string) error {
	return st.mutate(func(root *config.Node) error {
		parent, name, node, ok := findNode(root, ref)
		if !ok || node.Host == "" {
			return fmt.Errorf("%w: target %q not found", ErrConfigInvalid, ref)
		}
		delete(parent.Children, name)
		pruneEmpty(root)
		return nil
	})
}

// stageReplace parses a YAML or JSON doc (JSON is valid YAML) into a generic map,
// normalizes it to JSON, and stages it wholesale via st.setDoc, which mints ids for any
// new host nodes and validates locally. This replaces the ENTIRE working doc — it is the
// escape hatch for config shapes (alert routing, probe defaults, etc.) the typed
// config_stage_* tools don't cover.
func stageReplace(st *staging, raw string) error {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("%w: parse: %v", ErrConfigInvalid, err)
	}
	asJSON, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfigInvalid, err)
	}
	return st.setDoc(asJSON)
}

func registerConfigReplace(s *sdk.Server, c *Client, st *staging) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "config_stage_replace",
		Description: "Stage a wholesale replacement of the config DB fragment with a supplied YAML or JSON doc (use for alert routing, probe defaults, or anything the typed tools don't cover). Validated locally; LOCAL ONLY until config_apply. CAVEAT: to preserve each target's identity/history, base the replacement doc on heliograph_config_get source=db format=json (it includes each target's id) — a hand-authored doc that omits existing id fields mints fresh ones on apply, orphaning that target's history.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: false},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in struct {
		Doc string `json:"doc" jsonschema:"the full config DB fragment as YAML or JSON"`
	}) (*sdk.CallToolResult, stageResult, error) {
		if err := st.ensure(ctx, c); err != nil {
			return nil, stageResult{}, err
		}
		if err := stageReplace(st, in.Doc); err != nil {
			return nil, stageResult{}, err
		}
		return stageResultFor(st)
	})
}

func registerConfigStage(s *sdk.Server, c *Client, st *staging) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "config_stage_add_target",
		Description: "Stage adding a new target to the monitoring config. LOCAL ONLY — nothing is written until config_apply. Creates the group path if missing and mints a stable id.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: false},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in addTargetIn) (*sdk.CallToolResult, stageResult, error) {
		if err := st.ensure(ctx, c); err != nil {
			return nil, stageResult{}, err
		}
		if err := stageAddTarget(st, in); err != nil {
			return nil, stageResult{}, err
		}
		return stageResultFor(st)
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:        "config_stage_edit_target",
		Description: "Stage an edit to an existing target (host, params, probe, step, pings, measure, vantages, or move/rename). LOCAL ONLY until config_apply.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: false},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in editTargetIn) (*sdk.CallToolResult, stageResult, error) {
		if err := st.ensure(ctx, c); err != nil {
			return nil, stageResult{}, err
		}
		if err := stageEditTarget(st, in); err != nil {
			return nil, stageResult{}, err
		}
		return stageResultFor(st)
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:        "config_stage_remove_target",
		Description: "Stage removal of a target by id or path (empty groups are pruned). LOCAL ONLY until config_apply.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: false},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in struct {
		Target string `json:"target" jsonschema:"target id or path to remove"`
	}) (*sdk.CallToolResult, stageResult, error) {
		if err := st.ensure(ctx, c); err != nil {
			return nil, stageResult{}, err
		}
		if err := stageRemoveTarget(st, in.Target); err != nil {
			return nil, stageResult{}, err
		}
		return stageResultFor(st)
	})
}

type stageResult struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
}

func stageResultFor(st *staging) (*sdk.CallToolResult, stageResult, error) {
	added, removed, changed, err := st.diff()
	if err != nil {
		return nil, stageResult{}, err
	}
	res := stageResult{Added: added, Removed: removed, Changed: changed}
	msg := fmt.Sprintf("staged. added=%v removed=%v changed=%v — call config_review to inspect, config_apply to commit.", added, removed, changed)
	return textResult(msg), res, nil
}
