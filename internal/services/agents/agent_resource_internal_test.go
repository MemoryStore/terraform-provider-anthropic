// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// TestMapAgentResponseToState_customToolInputSchema is a regression test for the
// anthropic-sdk-go upgrade (v1.37 -> v1.51) that changed how the
// BetaManagedAgentsCustomToolInputSchema struct marshals: field reordering plus a
// spurious "ExtraFields":null key. Re-marshaling the parsed schema with
// encoding/json produced a state value that no longer matched the user's planned
// input_schema string, triggering "Provider produced inconsistent result after
// apply". The mapping now preserves the raw API JSON via RawJSON() and stores it
// as a jsontypes.Normalized value, so semantic (key-order-insensitive) equality
// holds against the user's jsonencode() output.
func TestMapAgentResponseToState_customToolInputSchema(t *testing.T) {
	t.Parallel()

	// API response schema: "type" first, no ExtraFields key.
	const apiInputSchema = `{"type":"object","properties":{"email":{"description":"The user's email address","type":"string"}},"required":["email"]}`

	agentJSON := `{
		"id": "agent_123",
		"name": "test",
		"model": {"id": "claude-sonnet-4-6"},
		"version": 1,
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-01T00:00:00Z",
		"tools": [
			{
				"type": "custom",
				"name": "lookup_user",
				"description": "Look up a user",
				"input_schema": ` + apiInputSchema + `
			}
		]
	}`

	var agent anthropic.BetaManagedAgentsAgent
	if err := json.Unmarshal([]byte(agentJSON), &agent); err != nil {
		t.Fatalf("failed to unmarshal agent fixture: %s", err)
	}

	var data AgentResourceModel
	if diags := mapAgentResponseToState(context.Background(), &agent, &data); diags.HasError() {
		t.Fatalf("mapAgentResponseToState returned errors: %+v", diags)
	}

	var tools []agentCustomToolModel
	if diags := data.CustomTools.ElementsAs(context.Background(), &tools, false); diags.HasError() {
		t.Fatalf("failed to read custom tools: %+v", diags)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 custom tool, got %d", len(tools))
	}

	got := tools[0].InputSchema
	if got.IsNull() || got.IsUnknown() {
		t.Fatalf("input_schema should be set, got null/unknown")
	}

	// The state value must not contain the encoding/json artifact that broke
	// round-tripping under the SDK upgrade.
	if strings.Contains(got.ValueString(), "ExtraFields") {
		t.Errorf("input_schema must not contain ExtraFields; got %q", got.ValueString())
	}

	// Semantic equality must hold against the user's jsonencode() output, which
	// orders keys alphabetically (properties, required, type) — a different order
	// than the API response. Without jsontypes.Normalized this mismatch is what
	// surfaced as "inconsistent result after apply".
	const userInputSchema = `{"properties":{"email":{"description":"The user's email address","type":"string"}},"required":["email"],"type":"object"}`
	planned := jsontypes.NewNormalizedValue(userInputSchema)
	equal, diags := planned.StringSemanticEquals(context.Background(), got)
	if diags.HasError() {
		t.Fatalf("semantic equality check returned errors: %+v", diags)
	}
	if !equal {
		t.Errorf("planned and applied input_schema should be semantically equal\nplanned: %s\napplied: %s", userInputSchema, got.ValueString())
	}
}

// TestBuildAgentToolConfigParams is a regression test for the anthropic-sdk-go
// upgrade (v1.58 -> v1.67) that replaced the flat
// BetaManagedAgentsAgentToolConfigParams (a `name` enum plus shared `enabled` /
// `permission_policy` fields) with a discriminated union carrying one struct per
// built-in tool. The Terraform `name` attribute now selects the union branch, so
// this asserts every schema-accepted name lands on its own branch with the
// enabled flag and permission policy carried across.
func TestBuildAgentToolConfigParams(t *testing.T) {
	t.Parallel()

	// Each case checks the branch the name must select, and reads back the
	// enabled/permission-policy fields from that branch.
	tests := []struct {
		name    string
		extract func(anthropic.BetaManagedAgentsAgentToolConfigParamsUnion) (set bool, enabled param.Opt[bool], allow bool, ask bool)
	}{
		{"bash", func(u anthropic.BetaManagedAgentsAgentToolConfigParamsUnion) (bool, param.Opt[bool], bool, bool) {
			c := u.OfBash
			if c == nil {
				return false, param.Opt[bool]{}, false, false
			}
			return true, c.Enabled, c.PermissionPolicy.OfAlwaysAllow != nil, c.PermissionPolicy.OfAlwaysAsk != nil
		}},
		{"edit", func(u anthropic.BetaManagedAgentsAgentToolConfigParamsUnion) (bool, param.Opt[bool], bool, bool) {
			c := u.OfEdit
			if c == nil {
				return false, param.Opt[bool]{}, false, false
			}
			return true, c.Enabled, c.PermissionPolicy.OfAlwaysAllow != nil, c.PermissionPolicy.OfAlwaysAsk != nil
		}},
		{"read", func(u anthropic.BetaManagedAgentsAgentToolConfigParamsUnion) (bool, param.Opt[bool], bool, bool) {
			c := u.OfRead
			if c == nil {
				return false, param.Opt[bool]{}, false, false
			}
			return true, c.Enabled, c.PermissionPolicy.OfAlwaysAllow != nil, c.PermissionPolicy.OfAlwaysAsk != nil
		}},
		{"write", func(u anthropic.BetaManagedAgentsAgentToolConfigParamsUnion) (bool, param.Opt[bool], bool, bool) {
			c := u.OfWrite
			if c == nil {
				return false, param.Opt[bool]{}, false, false
			}
			return true, c.Enabled, c.PermissionPolicy.OfAlwaysAllow != nil, c.PermissionPolicy.OfAlwaysAsk != nil
		}},
		{"glob", func(u anthropic.BetaManagedAgentsAgentToolConfigParamsUnion) (bool, param.Opt[bool], bool, bool) {
			c := u.OfGlob
			if c == nil {
				return false, param.Opt[bool]{}, false, false
			}
			return true, c.Enabled, c.PermissionPolicy.OfAlwaysAllow != nil, c.PermissionPolicy.OfAlwaysAsk != nil
		}},
		{"grep", func(u anthropic.BetaManagedAgentsAgentToolConfigParamsUnion) (bool, param.Opt[bool], bool, bool) {
			c := u.OfGrep
			if c == nil {
				return false, param.Opt[bool]{}, false, false
			}
			return true, c.Enabled, c.PermissionPolicy.OfAlwaysAllow != nil, c.PermissionPolicy.OfAlwaysAsk != nil
		}},
		{"web_fetch", func(u anthropic.BetaManagedAgentsAgentToolConfigParamsUnion) (bool, param.Opt[bool], bool, bool) {
			c := u.OfWebFetch
			if c == nil {
				return false, param.Opt[bool]{}, false, false
			}
			return true, c.Enabled, c.PermissionPolicy.OfAlwaysAllow != nil, c.PermissionPolicy.OfAlwaysAsk != nil
		}},
		{"web_search", func(u anthropic.BetaManagedAgentsAgentToolConfigParamsUnion) (bool, param.Opt[bool], bool, bool) {
			c := u.OfWebSearch
			if c == nil {
				return false, param.Opt[bool]{}, false, false
			}
			return true, c.Enabled, c.PermissionPolicy.OfAlwaysAllow != nil, c.PermissionPolicy.OfAlwaysAsk != nil
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name+"/always_allow", func(t *testing.T) {
			t.Parallel()

			u, diags := buildAgentToolConfigParams(tc.name, param.NewOpt(true), "always_allow")
			if diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			set, enabled, allow, ask := tc.extract(u)
			if !set {
				t.Fatalf("name %q did not select its own union branch", tc.name)
			}
			if !enabled.Valid() || !enabled.Value {
				t.Errorf("enabled = %+v, want set to true", enabled)
			}
			if !allow || ask {
				t.Errorf("permission policy = (allow=%t, ask=%t), want (true, false)", allow, ask)
			}
		})

		t.Run(tc.name+"/always_ask", func(t *testing.T) {
			t.Parallel()

			u, diags := buildAgentToolConfigParams(tc.name, param.NewOpt(false), "always_ask")
			if diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			set, enabled, allow, ask := tc.extract(u)
			if !set {
				t.Fatalf("name %q did not select its own union branch", tc.name)
			}
			if !enabled.Valid() || enabled.Value {
				t.Errorf("enabled = %+v, want set to false", enabled)
			}
			if allow || !ask {
				t.Errorf("permission policy = (allow=%t, ask=%t), want (false, true)", allow, ask)
			}
		})

		// An omitted permission_policy must leave both variants unset so the
		// field is elided from the request and the toolset default applies.
		t.Run(tc.name+"/no_policy", func(t *testing.T) {
			t.Parallel()

			u, diags := buildAgentToolConfigParams(tc.name, param.Opt[bool]{}, "")
			if diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			set, enabled, allow, ask := tc.extract(u)
			if !set {
				t.Fatalf("name %q did not select its own union branch", tc.name)
			}
			if enabled.Valid() {
				t.Errorf("enabled = %+v, want unset", enabled)
			}
			if allow || ask {
				t.Errorf("permission policy = (allow=%t, ask=%t), want both unset", allow, ask)
			}
		})
	}
}

// TestBuildAgentToolConfigParams_unknownName guards the union's default branch:
// a name outside the eight built-ins must surface a diagnostic rather than
// silently produce an empty union that marshals without a discriminator.
func TestBuildAgentToolConfigParams_unknownName(t *testing.T) {
	t.Parallel()

	_, diags := buildAgentToolConfigParams("not_a_tool", param.NewOpt(true), "always_allow")
	if !diags.HasError() {
		t.Fatal("expected an error diagnostic for an unsupported tool name, got none")
	}
	if got := diags.Errors()[0].Detail(); !strings.Contains(got, "not_a_tool") {
		t.Errorf("diagnostic detail = %q, want it to name the offending value", got)
	}
}

func modelParamsJSON(t *testing.T, m anthropic.BetaManagedAgentsModelConfigParams) map[string]any {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal model params: %s", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal model params: %s", err)
	}
	return out
}

func TestBuildModelConfigParams(t *testing.T) {
	t.Parallel()

	base := func(effort, geo types.String) AgentResourceModel {
		return AgentResourceModel{
			Model:             types.StringValue("claude-sonnet-4-6"),
			ModelSpeed:        types.StringNull(),
			ModelEffort:       effort,
			ModelInferenceGeo: geo,
		}
	}

	tests := []struct {
		name string
		plan AgentResourceModel
		want map[string]any
	}{
		{
			name: "create with effort and geo",
			plan: base(types.StringValue("high"), types.StringValue("us")),
			want: map[string]any{"id": "claude-sonnet-4-6", "effort": "high", "inference_geo": "us"},
		},
		{
			name: "create with unknown effort omits it",
			plan: base(types.StringUnknown(), types.StringNull()),
			want: map[string]any{"id": "claude-sonnet-4-6"},
		},
		{
			name: "update sends planned effort",
			plan: base(types.StringValue("low"), types.StringNull()),
			want: map[string]any{"id": "claude-sonnet-4-6", "effort": "low"},
		},
		{
			name: "update with null geo omits it so the pin is cleared",
			plan: base(types.StringValue("high"), types.StringNull()),
			want: map[string]any{"id": "claude-sonnet-4-6", "effort": "high"},
		},
		{
			name: "update keeps unchanged geo",
			plan: base(types.StringValue("high"), types.StringValue("us")),
			want: map[string]any{"id": "claude-sonnet-4-6", "effort": "high", "inference_geo": "us"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := modelParamsJSON(t, buildModelConfigParams(tt.plan))
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("%s: got %v, want %v (full: %v)", k, got[k], v, got)
				}
			}
		})
	}
}

func TestMapAgentResponseToState_modelEffortAndGeo(t *testing.T) {
	t.Parallel()

	t.Run("present", func(t *testing.T) {
		t.Parallel()
		data := AgentResourceModel{ModelEffort: types.StringUnknown()}
		agent := parseAgentFixture(t, `{"id":"claude-sonnet-4-6","effort":{"type":"xhigh"},"inference_geo":"us"}`)
		if diags := mapAgentResponseToState(context.Background(), agent, &data); diags.HasError() {
			t.Fatalf("diags: %+v", diags)
		}
		if data.ModelEffort.ValueString() != "xhigh" || data.ModelInferenceGeo.ValueString() != "us" {
			t.Errorf("got effort=%v geo=%v", data.ModelEffort, data.ModelInferenceGeo)
		}
	})

	t.Run("absent resolves unknown to null", func(t *testing.T) {
		t.Parallel()
		data := AgentResourceModel{ModelEffort: types.StringUnknown()}
		agent := parseAgentFixture(t, `{"id":"claude-sonnet-4-6"}`)
		if diags := mapAgentResponseToState(context.Background(), agent, &data); diags.HasError() {
			t.Fatalf("diags: %+v", diags)
		}
		if !data.ModelEffort.IsNull() || !data.ModelInferenceGeo.IsNull() {
			t.Errorf("got effort=%v geo=%v, want null", data.ModelEffort, data.ModelInferenceGeo)
		}
	})

	t.Run("empty and null map to null", func(t *testing.T) {
		t.Parallel()
		for _, model := range []string{
			`{"id":"claude-sonnet-4-6","effort":null,"inference_geo":""}`,
			`{"id":"claude-sonnet-4-6","inference_geo":null}`,
		} {
			data := AgentResourceModel{}
			if diags := mapAgentResponseToState(context.Background(), parseAgentFixture(t, model), &data); diags.HasError() {
				t.Fatalf("diags: %+v", diags)
			}
			if !data.ModelEffort.IsNull() || !data.ModelInferenceGeo.IsNull() {
				t.Errorf("%s: got effort=%v geo=%v, want null", model, data.ModelEffort, data.ModelInferenceGeo)
			}
		}
	})
}

const configuredInputSchemaWithAdditionalProperties = `{"type":"object","properties":{"email":{"type":"string"}},"required":["email"],"additionalProperties":false}`

func TestCustomToolInputSchemaParam_preservesAdditionalProperties(t *testing.T) {
	t.Parallel()

	schema, err := customToolInputSchemaParam(configuredInputSchemaWithAdditionalProperties)
	if err != nil {
		t.Fatalf("customToolInputSchemaParam: %s", err)
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal input schema param: %s", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal marshaled schema: %s", err)
	}
	if got["additionalProperties"] != false {
		t.Fatalf("marshaled schema = %s, want additionalProperties=false", raw)
	}
}

func TestBuildToolsParams_preservesAdditionalProperties(t *testing.T) {
	t.Parallel()

	built, diags := buildToolsParams(context.Background(), AgentResourceModel{
		CustomTools: mustCustomToolsList(t, "lookup_user", "Look up a user", configuredInputSchemaWithAdditionalProperties),
	})
	if diags.HasError() {
		t.Fatalf("buildToolsParams: %+v", diags)
	}
	if len(built) != 1 || built[0].OfCustom == nil {
		t.Fatalf("expected one custom tool, got %+v", built)
	}
	raw, err := json.Marshal(built[0].OfCustom.InputSchema)
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}
	if !strings.Contains(string(raw), `"additionalProperties":false`) {
		t.Fatalf("request payload dropped additionalProperties: %s", raw)
	}
	union, err := json.Marshal(built[0].OfCustom)
	if err != nil {
		t.Fatalf("marshal custom tool: %s", err)
	}
	if !strings.Contains(string(union), `"additionalProperties":false`) {
		t.Fatalf("parent tool payload dropped additionalProperties: %s", union)
	}
}

func TestCustomToolInputSchemaParam_rejectsNonObject(t *testing.T) {
	t.Parallel()

	if _, err := customToolInputSchemaParam(`["not","an","object"]`); err == nil {
		t.Fatal("expected an error for a JSON array")
	}
	if _, err := customToolInputSchemaParam(`true`); err == nil {
		t.Fatal("expected an error for a JSON boolean")
	}
}

func TestPreserveConfiguredInputSchema(t *testing.T) {
	t.Parallel()

	configured := jsontypes.NewNormalizedValue(configuredInputSchemaWithAdditionalProperties)
	apiMatch := jsontypes.NewNormalizedValue(`{"type":"object","properties":{"email":{"type":"string"}},"required":["email"]}`)
	apiDiverge := jsontypes.NewNormalizedValue(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
	apiEmpty := jsontypes.NewNormalizedValue(`{}`)
	apiNull := jsontypes.NewNormalizedNull()

	tests := []struct {
		name        string
		configured  jsontypes.Normalized
		api         jsontypes.Normalized
		wantConfig  bool
		wantNullAPI bool
	}{
		{name: "keeps extras when API is a subset", configured: configured, api: apiMatch, wantConfig: true},
		{name: "uses API when properties diverge", configured: configured, api: apiDiverge},
		{name: "uses API when payload is empty object", configured: configured, api: apiEmpty},
		{name: "uses API when payload is null", configured: configured, api: apiNull, wantNullAPI: true},
		{name: "uses API when nothing was configured", configured: jsontypes.NewNormalizedNull(), api: apiMatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preserveConfiguredInputSchema(tt.configured, tt.api)
			if tt.wantConfig {
				equal, diags := tt.configured.StringSemanticEquals(context.Background(), got)
				if diags.HasError() || !equal {
					t.Fatalf("got %s, want configured schema", got.ValueString())
				}
				return
			}
			if tt.wantNullAPI {
				if !got.IsNull() {
					t.Fatalf("got %s, want API null", got.ValueString())
				}
				return
			}
			if tt.api.IsNull() {
				return
			}
			equal, diags := tt.api.StringSemanticEquals(context.Background(), got)
			if diags.HasError() || !equal {
				t.Fatalf("got %s, want API schema", got.ValueString())
			}
		})
	}
}

func TestMapAgentResponseToState_keepsConfiguredAdditionalProperties(t *testing.T) {
	t.Parallel()

	const apiInputSchema = `{"type":"object","properties":{"email":{"type":"string"}},"required":["email"]}`
	data := AgentResourceModel{CustomTools: mustCustomToolsList(t, "lookup_user", "Look up a user", configuredInputSchemaWithAdditionalProperties)}

	agentJSON := `{
		"id": "agent_123",
		"name": "test",
		"model": {"id": "claude-sonnet-4-6"},
		"version": 1,
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-01T00:00:00Z",
		"tools": [
			{
				"type": "custom",
				"name": "lookup_user",
				"description": "Look up a user",
				"input_schema": ` + apiInputSchema + `
			}
		]
	}`
	var agent anthropic.BetaManagedAgentsAgent
	if err := json.Unmarshal([]byte(agentJSON), &agent); err != nil {
		t.Fatalf("unmarshal agent: %s", err)
	}
	if diags := mapAgentResponseToState(context.Background(), &agent, &data); diags.HasError() {
		t.Fatalf("mapAgentResponseToState: %+v", diags)
	}

	got := firstCustomToolInputSchema(t, data)
	planned := jsontypes.NewNormalizedValue(configuredInputSchemaWithAdditionalProperties)
	equal, diags := planned.StringSemanticEquals(context.Background(), got)
	if diags.HasError() {
		t.Fatalf("semantic equals: %+v", diags)
	}
	if !equal {
		t.Fatalf("state input_schema = %s, want configured schema with additionalProperties", got.ValueString())
	}
}

func TestMapAgentResponseToState_usesAPIWhenPropertiesDiverge(t *testing.T) {
	t.Parallel()

	const apiInputSchema = `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`
	data := AgentResourceModel{CustomTools: mustCustomToolsList(t, "lookup_user", "Look up a user", configuredInputSchemaWithAdditionalProperties)}

	agentJSON := `{
		"id": "agent_123",
		"name": "test",
		"model": {"id": "claude-sonnet-4-6"},
		"version": 1,
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-01T00:00:00Z",
		"tools": [
			{
				"type": "custom",
				"name": "lookup_user",
				"description": "Look up a user",
				"input_schema": ` + apiInputSchema + `
			}
		]
	}`
	var agent anthropic.BetaManagedAgentsAgent
	if err := json.Unmarshal([]byte(agentJSON), &agent); err != nil {
		t.Fatalf("unmarshal agent: %s", err)
	}
	if diags := mapAgentResponseToState(context.Background(), &agent, &data); diags.HasError() {
		t.Fatalf("mapAgentResponseToState: %+v", diags)
	}

	got := firstCustomToolInputSchema(t, data)
	if strings.Contains(got.ValueString(), "additionalProperties") {
		t.Fatalf("expected API schema when properties diverge, got %s", got.ValueString())
	}
	if !strings.Contains(got.ValueString(), `"id"`) {
		t.Fatalf("expected API property id, got %s", got.ValueString())
	}
}

func mustCustomToolsList(t *testing.T, name, description, schema string) types.List {
	t.Helper()
	tool, diags := types.ObjectValue(agentCustomToolAttrTypes, map[string]attr.Value{
		"name":         types.StringValue(name),
		"description":  types.StringValue(description),
		"input_schema": jsontypes.NewNormalizedValue(schema),
	})
	if diags.HasError() {
		t.Fatalf("tool object: %+v", diags)
	}
	list, diags := types.ListValue(types.ObjectType{AttrTypes: agentCustomToolAttrTypes}, []attr.Value{tool})
	if diags.HasError() {
		t.Fatalf("tools list: %+v", diags)
	}
	return list
}

func firstCustomToolInputSchema(t *testing.T, data AgentResourceModel) jsontypes.Normalized {
	t.Helper()
	var tools []agentCustomToolModel
	if diags := data.CustomTools.ElementsAs(context.Background(), &tools, false); diags.HasError() {
		t.Fatalf("read custom tools: %+v", diags)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 custom tool, got %d", len(tools))
	}
	return tools[0].InputSchema
}
