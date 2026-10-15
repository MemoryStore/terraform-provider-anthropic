// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package metadata

import (
	"context"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestBuildPatch(t *testing.T) {
	ctx := context.Background()
	value := func(v map[string]string) types.Map {
		result, d := types.MapValueFrom(ctx, types.StringType, v)
		if d.HasError() {
			t.Fatal(d)
		}
		return result
	}
	state := value(map[string]string{"a": "1", "b": "2"})
	for _, tc := range []struct {
		name string
		plan types.Map
		want map[string]any
	}{
		{"upsert and remove", value(map[string]string{"a": "new", "c": "3"}), map[string]any{"a": "new", "b": nil, "c": "3"}},
		{"empty", value(map[string]string{}), map[string]any{"a": nil, "b": nil}},
		{"block removed", types.MapNull(types.StringType), map[string]any{"a": nil, "b": nil}},
		{"unknown defers", types.MapUnknown(types.StringType), map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, d := BuildPatch(ctx, tc.plan, state)
			if d.HasError() || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, %v; want %#v", got, d, tc.want)
			}
		})
	}
}
