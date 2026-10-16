// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/ippontech/terraform-provider-anthropic/internal/admintest"
	"github.com/ippontech/terraform-provider-anthropic/internal/oauthtest"
	"github.com/ippontech/terraform-provider-anthropic/internal/providerdata"
)

func TestConfigureOrganizationPin(t *testing.T) {
	for _, tc := range []struct {
		name, expected, env, body, credential string
		skip                                  bool
		status                                int
		wantCalls                             int
		wantError                             string
	}{
		{name: "standard match", expected: "org_expected", body: `{"id":"org_expected"}`, credential: "api_key", wantCalls: 1},
		{name: "oauth match", expected: "org_expected", body: `{"id":"org_expected"}`, credential: "auth_token", wantCalls: 1},
		{name: "mismatch", expected: "org_expected", body: `{"id":"org_actual"}`, credential: "api_key", wantCalls: 1, wantError: "Organization mismatch"},
		{name: "unset", body: `{}`, credential: "api_key"},
		{name: "skip", expected: "org_expected", skip: true, credential: "api_key"},
		{name: "environment", env: "org_expected", body: `{"id":"org_expected"}`, credential: "api_key", wantCalls: 1},
		{name: "config precedence", expected: "org_expected", env: "org_other", body: `{"id":"org_expected"}`, credential: "api_key", wantCalls: 1},
		{name: "invalid response", expected: "org_expected", body: `{}`, credential: "api_key", wantCalls: 1, wantError: "Unable to verify organization"},
		{name: "invalid JSON", expected: "org_expected", body: `not json`, credential: "api_key", wantCalls: 1, wantError: "Unable to verify organization"},
		{name: "unauthorized", expected: "org_expected", status: 401, body: `{"error":{"type":"authentication_error","message":"secret-sentinel"}}`, credential: "api_key", wantCalls: 1, wantError: "Unable to verify organization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearCredentialEnv(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/organizations/me" || r.Method != "GET" {
					t.Errorf("unexpected identity request: %s %s", r.Method, r.URL.Path)
				}
				if tc.credential == "api_key" && (r.Header.Get("x-api-key") == "" || r.Header.Get("Authorization") != "") {
					t.Error("mixed or missing standard credential")
				}
				if tc.credential == "auth_token" && (r.Header.Get("Authorization") == "" || r.Header.Get("x-api-key") != "") {
					t.Error("mixed or missing OAuth credential")
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			t.Setenv("ANTHROPIC_BASE_URL", server.URL)
			if tc.env != "" {
				t.Setenv("ANTHROPIC_ORGANIZATION_ID", tc.env)
			}
			attrs := map[string]tftypes.Value{tc.credential: tftypes.NewValue(tftypes.String, "fake-key")}
			if tc.expected != "" {
				attrs["organization_id"] = tftypes.NewValue(tftypes.String, tc.expected)
			}
			if tc.skip {
				attrs["skip_organization_check"] = tftypes.NewValue(tftypes.Bool, true)
			}
			response := configureProvider(t, attrs)
			if calls != tc.wantCalls {
				t.Errorf("calls=%d;want %d", calls, tc.wantCalls)
			}
			if tc.wantError == "" {
				providerDataFrom(t, response)
			} else {
				if !response.Diagnostics.HasError() || response.Diagnostics.Errors()[0].Summary() != tc.wantError {
					t.Fatalf("diagnostics: %v", response.Diagnostics)
				}
				if response.ResourceData != nil || response.DataSourceData != nil {
					t.Error("provider data exposed after failed check")
				}
				if strings.Contains(fmt.Sprint(response.Diagnostics), "secret-sentinel") {
					t.Error("upstream secret leaked")
				}
				if tc.wantError == "Organization mismatch" {
					for _, id := range []string{"org_expected", "org_actual"} {
						if !strings.Contains(response.Diagnostics.Errors()[0].Detail(), id) {
							t.Errorf("missing %s", id)
						}
					}
				}
			}
		})
	}
}

func TestCheckOrganizationEveryClient(t *testing.T) {
	for _, mismatch := range []string{"", "standard", "admin", "oauth"} {
		t.Run("mismatch_"+mismatch, func(t *testing.T) {
			clearCredentialEnv(t)
			calls := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				kind := r.Header.Get("x-api-key")
				if r.Header.Get("Authorization") != "" {
					kind = "oauth"
					if r.Header.Get("x-api-key") != "" {
						t.Error("credentials mixed")
					}
				}
				calls[kind]++
				id := "org_expected"
				if kind == mismatch {
					id = "org_other"
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
			}))
			defer server.Close()
			t.Setenv("ANTHROPIC_BASE_URL", server.URL)
			pd := &providerdata.ProviderData{
				Client:      newSDKClient(option.WithAPIKey("standard"), server.Client()),
				AdminClient: admintest.NewClient(t, server),
				OAuthClient: oauthtest.NewClient(t, server),
			}
			pd.AdminClient.ApiKey = "admin"
			diags := checkOrganization(context.Background(), "org_expected", pd)
			if diags.HasError() != (mismatch != "") {
				t.Fatalf("diagnostics: %v", diags)
			}
			if mismatch == "" && (calls["standard"] != 1 || calls["admin"] != 1 || calls["oauth"] != 1) {
				t.Fatalf("did not check every client: %v", calls)
			}
		})
	}
}

func TestConfigureFederatedOrganizationPin(t *testing.T) {
	clearCredentialEnv(t)
	server := newFederationServer(t, 3600)
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	expected := "00000000-0000-0000-0000-000000000000"
	response := configureProvider(t, map[string]tftypes.Value{
		"organization_id": tftypes.NewValue(tftypes.String, expected),
		"federation": federationValue(t, map[string]tftypes.Value{
			"organization_id":    tftypes.NewValue(tftypes.String, expected),
			"federation_rule_id": tftypes.NewValue(tftypes.String, "fdrl_rule"),
			"identity_token":     tftypes.NewValue(tftypes.String, "fake-jwt"),
		}),
	})
	providerDataFrom(t, response)
	exchanged, authorized := server.snapshot()
	if len(exchanged) != 1 || len(authorized) != 1 || authorized[0] != "Bearer sk-ant-oat01-minted-1" {
		t.Fatalf("exchange/check counts: %d/%d", len(exchanged), len(authorized))
	}
}
