// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/ippontech/terraform-provider-anthropic/internal/oauthtest"
	"github.com/ippontech/terraform-provider-anthropic/internal/schematest"
)

func TestAgentMetadataUpdate(t *testing.T) {
	ctx := context.Background()
	value := func(v map[string]string) types.Map {
		m, d := types.MapValueFrom(ctx, types.StringType, v)
		if d.HasError() {
			t.Fatal(d)
		}
		return m
	}
	for _, tc := range []struct {
		name    string
		planned types.Map
		want    map[string]any
	}{
		{"key added", value(map[string]string{"a": "1", "b": "2", "c": "3"}), map[string]any{"a": "1", "b": "2", "c": "3"}},
		{"key removed", value(map[string]string{"a": "1"}), map[string]any{"a": "1", "b": nil}},
		{"all keys removed", value(map[string]string{}), map[string]any{"a": nil, "b": nil}},
		{"block removed", types.MapNull(types.StringType), map[string]any{"a": nil, "b": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				called = true
				if req.Method != "POST" || req.URL.Path != "/v1/agents/object_test" {
					t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(body["metadata"], tc.want) {
					t.Errorf("metadata=%#v;want %#v", body["metadata"], tc.want)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"object_test","name":"test","model":{"id":"claude-sonnet-4-6"},"version":2,"metadata":{},"created_at":"2026-10-15T00:00:00Z","updated_at":"2026-10-15T00:00:00Z"}`))
			}))
			defer server.Close()
			r := &AgentResource{client: oauthtest.NewSDKClient(t, server, option.WithMaxRetries(0))}
			state := schematest.NullState(t, r)
			for name, v := range map[string]attr.Value{"id": types.StringValue("object_test"), "name": types.StringValue("test"), "metadata": value(map[string]string{"a": "1", "b": "2"})} {
				if d := state.SetAttribute(ctx, path.Root(name), v); d.HasError() {
					t.Fatal(d)
				}
			}
			plan := tfsdk.Plan(state)
			if d := plan.SetAttribute(ctx, path.Root("metadata"), tc.planned); d.HasError() {
				t.Fatal(d)
			}
			response := resource.UpdateResponse{State: state}
			r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &response)
			if response.Diagnostics.HasError() || !called {
				t.Fatalf("update failed: %v", response.Diagnostics)
			}
			if len(tc.planned.Elements()) == 0 {
				var got types.Map
				if d := response.State.GetAttribute(ctx, path.Root("metadata"), &got); d.HasError() || !got.Equal(tc.planned) {
					t.Fatalf("empty/null map not preserved: %v %v", got, d)
				}
			}
		})
	}
}
