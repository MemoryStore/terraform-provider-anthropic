// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/ippontech/terraform-provider-anthropic/internal/providerdata"
)

type organizationIdentity struct {
	ID string `json:"id"`
}

// checkOrganization verifies each credential independently: one matching key
// says nothing about the organization targeted by another configured client.
func checkOrganization(ctx context.Context, expected string, pd *providerdata.ProviderData) diag.Diagnostics {
	var diags diag.Diagnostics
	if expected == "" {
		return diags
	}
	type identityLookup struct {
		kind string
		get  func(context.Context) (organizationIdentity, error)
	}
	var lookups []identityLookup
	if pd.Client != nil {
		lookups = append(lookups, identityLookup{"api_key", func(ctx context.Context) (organizationIdentity, error) {
			var org organizationIdentity
			err := pd.Client.Get(ctx, "/v1/organizations/me", nil, &org)
			return org, err
		}})
	}
	if pd.AdminClient != nil {
		lookups = append(lookups, identityLookup{"admin_api_key", func(ctx context.Context) (organizationIdentity, error) {
			var org organizationIdentity
			raw, err := pd.AdminClient.DoRequest(ctx, "GET", "/v1/organizations/me", nil)
			if err != nil {
				return org, err
			}
			err = json.Unmarshal(raw, &org)
			return org, err
		}})
	}
	if pd.OAuthClient != nil {
		lookups = append(lookups, identityLookup{"auth_token/federation", func(ctx context.Context) (organizationIdentity, error) {
			var org organizationIdentity
			err := pd.OAuthClient.Get(ctx, "/v1/organizations/me", nil, &org)
			return org, err
		}})
	}
	for _, lookup := range lookups {
		org, err := lookup.get(ctx)
		if err != nil || org.ID == "" {
			// Do not expose upstream bodies: an authentication service may echo
			// credentials or identity assertions in an error response.
			diags.AddError("Unable to verify organization", fmt.Sprintf("Could not resolve a valid organization identity for %s. Check its credentials, access to GET /v1/organizations/me, and API connectivity. No resources will be managed with this configuration.", lookup.kind))
			return diags
		}
		if org.ID != expected {
			diags.AddError("Organization mismatch", fmt.Sprintf("Configured organization_id is %q, but %s belongs to organization %q.", expected, lookup.kind, org.ID))
			return diags
		}
	}
	return diags
}
