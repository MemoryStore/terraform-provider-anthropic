// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

// Package metadata builds declarative patches for API metadata bags.
package metadata

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// BuildPatch computes a PATCH body for the metadata field shared by
// agents, environments, vaults and vault credentials. The API preserves omitted keys and deletes keys
// whose value is null, so to converge declarative config with server state the
// patch upserts every planned key and explicitly sets keys removed since the
// prior state to null. Returns a map[string]any (values are string for upserts,
// nil for deletes) suitable for SetExtraFields; an empty map means "no change".
func BuildPatch(ctx context.Context, plan, state types.Map) (map[string]any, diag.Diagnostics) {
	var diags diag.Diagnostics
	patch := map[string]any{}
	if plan.IsUnknown() {
		return patch, diags
	}

	if !plan.IsNull() && !plan.IsUnknown() {
		var pm map[string]string
		diags.Append(plan.ElementsAs(ctx, &pm, false)...)
		if diags.HasError() {
			return nil, diags
		}
		for k, v := range pm {
			patch[k] = v
		}
	}

	if !state.IsNull() && !state.IsUnknown() {
		var sm map[string]string
		diags.Append(state.ElementsAs(ctx, &sm, false)...)
		if diags.HasError() {
			return nil, diags
		}
		for k := range sm {
			if _, ok := patch[k]; !ok {
				patch[k] = nil
			}
		}
	}

	return patch, diags
}
