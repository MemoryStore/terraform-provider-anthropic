// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	provretry "github.com/ippontech/terraform-provider-anthropic/internal/retry"
)

// hashSkillFiles uses the same bundle root and relative paths as the uploader.
// Unknown inputs are deferred until Terraform knows their paths.
func hashSkillFiles(ctx context.Context, files types.List) (types.Map, diag.Diagnostics) {
	var diags diag.Diagnostics
	unknown := types.MapUnknown(types.StringType)
	if files.IsNull() || files.IsUnknown() {
		return unknown, diags
	}
	for _, file := range files.Elements() {
		if file.IsUnknown() {
			return unknown, diags
		}
	}
	var paths []string
	diags.Append(files.ElementsAs(ctx, &paths, false)...)
	if diags.HasError() {
		return unknown, diags
	}
	root, _, err := provretry.DeriveBundleRoot(paths)
	if err != nil {
		diags.AddAttributeError(path.Root("files"), "Invalid skill bundle", err.Error())
		return unknown, diags
	}
	hashes := make(map[string]string, len(paths))
	for _, filename := range paths {
		if err := ctx.Err(); err != nil {
			diags.AddError("Skill hashing cancelled", err.Error())
			return unknown, diags
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil || relative == "." || !filepath.IsLocal(relative) {
			diags.AddAttributeError(path.Root("files"), "Invalid skill bundle", fmt.Sprintf("File %q is outside the bundle root.", filename))
			return unknown, diags
		}
		hash, err := hashSkillFile(filename)
		if err != nil {
			diags.AddAttributeError(path.Root("files"), "Unable to hash skill file", fmt.Sprintf("Cannot read %q: %s", filename, err))
			return unknown, diags
		}
		hashes[filepath.ToSlash(relative)] = hash
	}
	value, d := types.MapValueFrom(ctx, types.StringType, hashes)
	diags.Append(d...)
	return value, diags
}

func hashSkillFile(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// verifySkillFiles prevents a saved plan from uploading different bytes than it
// described. Read never recomputes hashes: they describe the adopted/uploaded bundle.
func verifySkillFiles(ctx context.Context, files types.List, planned types.Map) (types.Map, diag.Diagnostics) {
	actual, diags := hashSkillFiles(ctx, files)
	if !diags.HasError() && !planned.IsUnknown() && !planned.IsNull() && !actual.Equal(planned) {
		diags.AddAttributeError(path.Root("files"), "Skill files changed after planning", "The skill bundle differs from file_hashes in the saved plan. Run terraform plan again before applying.")
	}
	return actual, diags
}

func (r *SkillResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan SkillResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	hashes, diags := hashSkillFiles(ctx, plan.Files)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("file_hashes"), hashes)...)
	if req.State.Raw.IsNull() {
		return
	}
	var state SkillResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Pure adoption and local path relocation do not write to the API. Keep the
	// API's computed values known so the state-only update can converge.
	if !hashes.IsUnknown() && !skillNeedsUpload(plan.Files, state.Files, hashes, state.FileHashes) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("latest_version"), state.LatestVersion)...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("updated_at"), state.UpdatedAt)...)
	} else {
		// Filesystem-only drift appears after the framework's automatic unknown
		// marking, so explicitly invalidate outputs changed by the version write.
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("latest_version"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("updated_at"), types.StringUnknown())...)
	}
}

func skillNeedsUpload(files, oldFiles types.List, hashes, oldHashes types.Map) bool {
	if oldHashes.IsNull() || oldHashes.IsUnknown() {
		return !oldFiles.IsNull() && !oldFiles.IsUnknown() && !files.Equal(oldFiles)
	}
	return !hashes.Equal(oldHashes)
}

func (r *SkillVersionResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan SkillVersionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	hashes, diags := hashSkillFiles(ctx, plan.Files)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("file_hashes"), hashes)...)
	if req.State.Raw.IsNull() {
		return
	}
	var state SkillVersionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !hashes.IsUnknown() && !state.FileHashes.IsNull() && !state.FileHashes.IsUnknown() && !hashes.Equal(state.FileHashes) {
		resp.RequiresReplace = append(resp.RequiresReplace, path.Root("file_hashes"))
		for _, name := range []string{"id", "version", "name", "description", "directory", "created_at"} {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), types.StringUnknown())...)
		}
	}
}
