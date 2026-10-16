// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/ippontech/terraform-provider-anthropic/internal/oauthtest"
	"github.com/ippontech/terraform-provider-anthropic/internal/schematest"
)

func bundleFiles(t *testing.T, root string) types.List {
	t.Helper()
	var files []string
	for _, name := range []string{"SKILL.md", "nested/helper.txt"} {
		filename := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		files = append(files, filename)
	}
	value, diags := types.ListValueFrom(context.Background(), types.StringType, files)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return value
}

func TestHashSkillFiles(t *testing.T) {
	ctx := context.Background()
	files := bundleFiles(t, t.TempDir())
	hashes, diags := hashSkillFiles(ctx, files)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if len(hashes.Elements()) != 2 || hashes.Elements()["nested/helper.txt"] == nil {
		t.Fatalf("hashes: %v", hashes)
	}
	moved := bundleFiles(t, t.TempDir())
	movedHashes, diags := hashSkillFiles(ctx, moved)
	if diags.HasError() || !movedHashes.Equal(hashes) {
		t.Fatalf("root relocation changed hashes: %v", diags)
	}
	file := files.Elements()[0].(types.String).ValueString()
	if err := os.WriteFile(file, []byte("edited"), 0600); err != nil {
		t.Fatal(err)
	}
	updated, diags := hashSkillFiles(ctx, files)
	if diags.HasError() || updated.Equal(hashes) {
		t.Fatalf("content edit undetected: %v", diags)
	}
	if _, diags := verifySkillFiles(ctx, files, hashes); !diags.HasError() {
		t.Fatal("saved plan mismatch accepted")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, diags := hashSkillFiles(ctx, files); !diags.HasError() {
		t.Fatal("missing file accepted")
	}
	for _, value := range []types.List{types.ListUnknown(types.StringType), types.ListValueMust(types.StringType, []attr.Value{types.StringUnknown()})} {
		hashes, diags := hashSkillFiles(ctx, value)
		if diags.HasError() || !hashes.IsUnknown() {
			t.Fatalf("unknown paths not deferred: %v", diags)
		}
	}
	directory := types.ListValueMust(types.StringType, []attr.Value{types.StringValue(t.TempDir())})
	if _, diags := hashSkillFiles(ctx, directory); !diags.HasError() {
		t.Fatal("directory accepted as file")
	}
}

func skillState(t *testing.T, r resource.Resource, files types.List, hashes types.Map) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	state := schematest.NullState(t, r)
	for name, value := range map[string]attr.Value{"id": types.StringValue("skill_old"), "files": files, "file_hashes": hashes} {
		if diags := state.SetAttribute(ctx, path.Root(name), value); diags.HasError() {
			t.Fatal(diags)
		}
	}
	return state
}

func TestSkillHashPlanningAndAdoption(t *testing.T) {
	ctx := context.Background()
	for _, version := range []bool{false, true} {
		name := "skill"
		if version {
			name = "version"
		}
		t.Run(name, func(t *testing.T) {
			files := bundleFiles(t, t.TempDir())
			var r resource.ResourceWithModifyPlan = &SkillResource{}
			if version {
				r = &SkillVersionResource{}
			}
			old := skillState(t, r, files, types.MapNull(types.StringType))
			plan := tfsdk.Plan(old)
			response := resource.ModifyPlanResponse{Plan: plan}
			r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: old}, &response)
			if response.Diagnostics.HasError() || len(response.RequiresReplace) != 0 {
				t.Fatalf("adoption plan: %v", response.Diagnostics)
			}
			var hashes types.Map
			if d := response.Plan.GetAttribute(ctx, path.Root("file_hashes"), &hashes); d.HasError() || hashes.IsNull() {
				t.Fatalf("missing hashes: %v", d)
			}
			update := resource.UpdateResponse{State: old}
			// Neither resource has a client: adoption must not make an API call.
			r.Update(ctx, resource.UpdateRequest{Plan: response.Plan, State: old}, &update)
			if update.Diagnostics.HasError() {
				t.Fatal(update.Diagnostics)
			}
			if d := update.State.GetAttribute(ctx, path.Root("file_hashes"), &hashes); d.HasError() || hashes.IsNull() {
				t.Fatalf("adoption not persisted: %v", d)
			}
			adopted := update.State
			file := files.Elements()[0].(types.String).ValueString()
			if err := os.WriteFile(file, []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			plan = tfsdk.Plan(adopted)
			response = resource.ModifyPlanResponse{Plan: plan}
			r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: adopted}, &response)
			if response.Diagnostics.HasError() {
				t.Fatal(response.Diagnostics)
			}
			if version && len(response.RequiresReplace) != 1 {
				t.Fatal("hash edit did not replace immutable version")
			}
			if !version && len(response.RequiresReplace) != 0 {
				t.Fatal("hash edit replaces skill")
			}
			if !version {
				for _, name := range []string{"latest_version", "updated_at"} {
					var output types.String
					if d := response.Plan.GetAttribute(ctx, path.Root(name), &output); d.HasError() || !output.IsUnknown() {
						t.Fatalf("content edit left %s known: %v %v", name, output, d)
					}
				}
			}
			// Destroy must not require local files.
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			destroyed := tfsdk.Plan{Raw: tftypes.NewValue(schematest.ResourceObjectType(t, r), nil), Schema: old.Schema}
			response = resource.ModifyPlanResponse{Plan: destroyed}
			r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: destroyed, State: adopted}, &response)
			if response.Diagnostics.HasError() {
				t.Fatal(response.Diagnostics)
			}
		})
	}
}

func TestSkillUpdatePublishesOnlyContentChanges(t *testing.T) {
	ctx := context.Background()
	files := bundleFiles(t, t.TempDir())
	hashes, diags := hashSkillFiles(ctx, files)
	if diags.HasError() {
		t.Fatal(diags)
	}
	uploads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" || req.URL.Path != "/v1/skills/skill_old/versions" {
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
			w.WriteHeader(404)
			return
		}
		reader, err := req.MultipartReader()
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if !strings.Contains(part.Header.Get("Content-Disposition"), "SKILL.md") {
			t.Error("missing multipart bundle filename")
		}
		uploads++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"version_new","version":"2","created_at":"2026-10-15T00:00:00Z"}`))
	}))
	defer server.Close()
	r := &SkillResource{client: oauthtest.NewSDKClient(t, server, option.WithMaxRetries(0))}
	state := skillState(t, r, files, hashes)
	filename := files.Elements()[0].(types.String).ValueString()
	if err := os.WriteFile(filename, []byte("new content"), 0600); err != nil {
		t.Fatal(err)
	}
	newHashes, diags := hashSkillFiles(ctx, files)
	if diags.HasError() {
		t.Fatal(diags)
	}
	planned := skillState(t, r, files, newHashes)
	plan := tfsdk.Plan(planned)
	response := resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	var got SkillResourceModel
	if d := response.State.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	if uploads != 1 || got.ID.ValueString() != "skill_old" || got.LatestVersion.ValueString() != "2" {
		t.Fatalf("unexpected result: %d %+v", uploads, got)
	}
	// force_destroy is mutable but must not publish another version.
	state = response.State
	if d := state.SetAttribute(ctx, path.Root("force_destroy"), types.BoolValue(true)); d.HasError() {
		t.Fatal(d)
	}
	response = resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{Plan: tfsdk.Plan(state), State: state}, &response)
	if response.Diagnostics.HasError() || uploads != 1 {
		t.Fatalf("state-only update uploaded: %v", response.Diagnostics)
	}
	// Adoption cannot swallow an explicit file-list change.
	if !skillNeedsUpload(files, types.ListValueMust(types.StringType, []attr.Value{types.StringValue("old/path")}), hashes, types.MapNull(types.StringType)) {
		t.Fatal("file-list change swallowed by adoption")
	}
}

func TestSkillTitleReplaceUnknownsComputed(t *testing.T) {
	ctx := context.Background()
	files := bundleFiles(t, t.TempDir())
	hashes, diags := hashSkillFiles(ctx, files)
	if diags.HasError() {
		t.Fatal(diags)
	}
	r := &SkillResource{}
	state := skillState(t, r, files, hashes)
	for name, value := range map[string]attr.Value{
		"display_title":  types.StringValue("old"),
		"latest_version": types.StringValue("v1"),
		"updated_at":     types.StringValue("t1"),
		"created_at":     types.StringValue("c1"),
		"source":         types.StringValue("custom"),
	} {
		if d := state.SetAttribute(ctx, path.Root(name), value); d.HasError() {
			t.Fatal(d)
		}
	}
	plan := tfsdk.Plan(state)
	if d := plan.SetAttribute(ctx, path.Root("display_title"), types.StringValue("new")); d.HasError() {
		t.Fatal(d)
	}
	response := resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	for _, name := range []string{"id", "created_at", "source", "latest_version", "updated_at"} {
		var output types.String
		if d := response.Plan.GetAttribute(ctx, path.Root(name), &output); d.HasError() || !output.IsUnknown() {
			t.Fatalf("title replace left %s known: %v %v", name, output, d)
		}
	}
}

func TestSkillVersionReplaceUnknownsComputed(t *testing.T) {
	ctx := context.Background()
	files := bundleFiles(t, t.TempDir())
	hashes, diags := hashSkillFiles(ctx, files)
	if diags.HasError() {
		t.Fatal(diags)
	}
	elements := files.Elements()
	reordered, d := types.ListValue(types.StringType, []attr.Value{elements[1], elements[0]})
	if d.HasError() {
		t.Fatal(d)
	}
	r := &SkillVersionResource{}
	for _, tc := range []struct {
		name  string
		files types.List
		skill string
	}{
		{"reordered paths", reordered, "skill_a"},
		{"skill id", files, "skill_b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := skillState(t, r, files, hashes)
			for name, value := range map[string]attr.Value{
				"skill_id":    types.StringValue("skill_a"),
				"version":     types.StringValue("1"),
				"name":        types.StringValue("old"),
				"description": types.StringValue("old"),
				"directory":   types.StringValue("old"),
				"created_at":  types.StringValue("c1"),
			} {
				if d := state.SetAttribute(ctx, path.Root(name), value); d.HasError() {
					t.Fatal(d)
				}
			}
			plan := tfsdk.Plan(state)
			if d := plan.SetAttribute(ctx, path.Root("files"), tc.files); d.HasError() {
				t.Fatal(d)
			}
			if d := plan.SetAttribute(ctx, path.Root("skill_id"), types.StringValue(tc.skill)); d.HasError() {
				t.Fatal(d)
			}
			response := resource.ModifyPlanResponse{Plan: plan}
			r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state}, &response)
			if response.Diagnostics.HasError() || len(response.RequiresReplace) != 0 {
				t.Fatalf("hash-stable replace: %v %v", response.Diagnostics, response.RequiresReplace)
			}
			for _, name := range []string{"id", "version", "name", "description", "directory", "created_at"} {
				var output types.String
				if d := response.Plan.GetAttribute(ctx, path.Root(name), &output); d.HasError() || !output.IsUnknown() {
					t.Fatalf("%s left known: %v %v", name, output, d)
				}
			}
		})
	}
}

func TestSkillSavedPlanRejectsChangedFiles(t *testing.T) {
	ctx := context.Background()
	for _, r := range []resource.Resource{&SkillResource{}, &SkillVersionResource{}} {
		files := bundleFiles(t, t.TempDir())
		hashes, diags := hashSkillFiles(ctx, files)
		if diags.HasError() {
			t.Fatal(diags)
		}
		state := skillState(t, r, files, hashes)
		filename := files.Elements()[0].(types.String).ValueString()
		if err := os.WriteFile(filename, []byte("changed after planning"), 0600); err != nil {
			t.Fatal(err)
		}
		response := resource.CreateResponse{State: state}
		// A nil API client makes any upload a test failure; stale plans must fail
		// before sending a request for either resource.
		r.Create(ctx, resource.CreateRequest{Plan: tfsdk.Plan(state)}, &response)
		if !response.Diagnostics.HasError() || response.Diagnostics.Errors()[0].Summary() != "Skill files changed after planning" {
			t.Fatalf("saved plan accepted changed files: %v", response.Diagnostics)
		}
	}
}
