// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package skills_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	acctest "github.com/ippontech/terraform-provider-anthropic/internal/acctest"
	provretry "github.com/ippontech/terraform-provider-anthropic/internal/retry"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func testAccSkillFilePath(t *testing.T) string {
	t.Helper()
	// The API requires the multipart folder name to match the `name` field in
	// the SKILL.md frontmatter. Create an explicit subdirectory so dirName is
	// predictable rather than relying on t.TempDir()'s random suffix.
	dir := filepath.Join(t.TempDir(), "test-skill")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create test skill directory: %s", err)
	}
	p := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(p, []byte("---\nname: test-skill\ndescription: For testing.\n---\n\n# Test Skill\n\nFor testing.\n"), 0644); err != nil {
		t.Fatalf("failed to write test skill file: %s", err)
	}
	return p
}

func testAccCheckSkillDestroyed(s *terraform.State) error {
	client := acctest.NewAPIKeyClient()
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "anthropic_skill" {
			continue
		}
		_, err := client.Beta.Skills.Get(context.Background(), rs.Primary.ID, anthropic.BetaSkillGetParams{})
		if err != nil {
			// Resource not found — destroyed successfully.
			return nil
		}
		return fmt.Errorf("skill %s still exists", rs.Primary.ID)
	}
	return nil
}

func TestAccSkillResource_basic(t *testing.T) {
	skillFilePath := testAccSkillFilePath(t)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckSkillDestroyed,
		Steps: []resource.TestStep{
			// Create and Read
			{
				Config: fmt.Sprintf(testAccSkillResourceBasicConfig, skillFilePath),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("anthropic_skill.test", "id"),
					resource.TestCheckResourceAttr("anthropic_skill.test", "source", "custom"),
					resource.TestCheckResourceAttrSet("anthropic_skill.test", "created_at"),
					resource.TestCheckResourceAttrSet("anthropic_skill.test", "updated_at"),
					resource.TestCheckResourceAttrSet("anthropic_skill.test", "latest_version"),
				),
			},
			// ImportState — display_title is not set in config so the API may return a
			// server-derived value; force_destroy is not tracked server-side;
			// updated_at may differ between the create response and a subsequent GET.
			{
				ResourceName:            "anthropic_skill.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"file_hashes", "files", "display_title", "force_destroy", "updated_at"},
			},
		},
	})
}

func TestAccSkillResource_withDisplayTitle(t *testing.T) {
	skillFilePath := testAccSkillFilePath(t)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckSkillDestroyed,
		Steps: []resource.TestStep{
			// Create and Read
			{
				Config: fmt.Sprintf(testAccSkillResourceWithDisplayTitleConfig, skillFilePath),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("anthropic_skill.test", "id"),
					resource.TestCheckResourceAttr("anthropic_skill.test", "display_title", "tf-acc-test-skill"),
					resource.TestCheckResourceAttr("anthropic_skill.test", "source", "custom"),
				),
			},
			// ImportState — force_destroy is not tracked server-side;
			// updated_at may differ between create response and a subsequent GET.
			{
				ResourceName:            "anthropic_skill.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"file_hashes", "files", "force_destroy", "updated_at"},
			},
		},
	})
}

const testAccSkillResourceBasicConfig = `
resource "anthropic_skill" "test" {
  files         = [%q]
  force_destroy = true
}
`

const testAccSkillResourceWithDisplayTitleConfig = `
resource "anthropic_skill" "test" {
  display_title = "tf-acc-test-skill"
  files         = [%q]
  force_destroy = true
}
`

// Content edits at the same path must publish a new version without changing
// the skill ID. A bundle move and force_destroy changes are state-only updates.
func TestAccSkillResource_contentUpdate(t *testing.T) {
	acctest.PreCheck(t)
	file := testAccSkillFilePath(t)
	config := fmt.Sprintf(testAccSkillResourceBasicConfig, file)
	extra := filepath.Join(filepath.Dir(file), "nested", "helper.txt")
	expanded := fmt.Sprintf("resource \"anthropic_skill\" \"test\" {\n files = [%q, %q]\n force_destroy = true\n}\n", file, extra)

	var id, version string
	capture := func(s *terraform.State) error {
		r := s.RootModule().Resources["anthropic_skill.test"]
		id = r.Primary.ID
		version = r.Primary.Attributes["latest_version"]
		return nil
	}
	check := func(changed bool) resource.TestCheckFunc {
		return func(s *terraform.State) error {
			r := s.RootModule().Resources["anthropic_skill.test"]
			if r.Primary.ID != id {
				return fmt.Errorf("skill ID changed")
			}
			if (r.Primary.Attributes["latest_version"] != version) != changed {
				return fmt.Errorf("unexpected version change (wanted changed=%t)", changed)
			}
			version = r.Primary.Attributes["latest_version"]
			return nil
		}
	}
	moved := filepath.Join(t.TempDir(), "test-skill", "SKILL.md")
	resource.Test(t, resource.TestCase{
		PreCheck: func() { acctest.PreCheck(t) }, ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories, CheckDestroy: testAccCheckSkillDestroyed,
		Steps: []resource.TestStep{
			{Config: config, Check: capture},
			{PreConfig: func() {
				f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = f.Close() }()
				if _, err := f.WriteString("\nChanged content.\n"); err != nil {
					t.Fatal(err)
				}
			}, Config: config, Check: check(true)},
			{PreConfig: func() {
				if err := os.MkdirAll(filepath.Dir(extra), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(extra, []byte("helper"), 0600); err != nil {
					t.Fatal(err)
				}
			}, Config: expanded, Check: check(true)},
			{Config: config, Check: check(true)},

			{PreConfig: func() {
				if err := os.MkdirAll(filepath.Dir(moved), 0700); err != nil {
					t.Fatal(err)
				}
				content, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(moved, content, 0600); err != nil {
					t.Fatal(err)
				}
			}, Config: fmt.Sprintf(testAccSkillResourceBasicConfig, moved), Check: check(false)},
			{Config: fmt.Sprintf(testAccSkillResourceBasicConfig, moved), Check: check(false)},
			{Config: fmt.Sprintf("resource \"anthropic_skill\" \"test\" {\n files = [%q]\n force_destroy = false\n }", moved), Check: check(false)},
			{Config: fmt.Sprintf(testAccSkillResourceBasicConfig, moved), Check: check(false)},
		},
	})
}

func TestAccSkillResource_importAdoption(t *testing.T) {
	acctest.PreCheck(t)
	ctx := context.Background()
	file := testAccSkillFilePath(t)
	client := acctest.NewAPIKeyClient()
	root, name, err := provretry.DeriveBundleRoot([]string{file})
	if err != nil {
		t.Fatal(err)
	}
	skill, err := provretry.MultipartUpload(ctx, []string{file}, root, name, func(files []io.Reader) (*anthropic.BetaSkillNewResponse, error) {
		return client.Beta.Skills.New(ctx, anthropic.BetaSkillNewParams{Files: files, DisplayTitle: anthropic.String("tf-acc-test-skill")})
	})
	if err != nil {
		t.Fatal("unable to seed import fixture")
	}
	t.Cleanup(func() {
		// Terraform normally removes the imported fixture. Clean up independently
		// if import or an assertion fails before it can own the skill.
		versions := client.Beta.Skills.Versions.ListAutoPaging(ctx, skill.ID, anthropic.BetaSkillVersionListParams{})
		for versions.Next() {
			v := versions.Current()
			if _, err := client.Beta.Skills.Versions.Delete(ctx, v.Version, anthropic.BetaSkillVersionDeleteParams{SkillID: skill.ID}); err != nil {
				t.Error("unable to clean up import fixture version")
			}
		}
		if _, err := client.Beta.Skills.Delete(ctx, skill.ID, anthropic.BetaSkillDeleteParams{}); err != nil {
			var apierr *anthropic.Error
			if !errors.As(err, &apierr) || apierr.StatusCode != 404 {
				t.Error("unable to clean up import fixture skill")
			}
		}
	})
	config := fmt.Sprintf(testAccSkillResourceWithDisplayTitleConfig, file)
	withVersion := config + fmt.Sprintf("\nresource \"anthropic_skill_version\" \"test\" {\n skill_id = anthropic_skill.test.id\n files = [%q]\n}\n", file)

	resource.Test(t, resource.TestCase{
		PreCheck: func() { acctest.PreCheck(t) }, ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories, CheckDestroy: testAccCheckSkillDestroyed,
		Steps: []resource.TestStep{
			{Config: config, ResourceName: "anthropic_skill.test", ImportState: true, ImportStatePersist: true, ImportStateId: skill.ID},
			{Config: config, Check: func(s *terraform.State) error {
				r := s.RootModule().Resources["anthropic_skill.test"]
				if r.Primary.ID != skill.ID || r.Primary.Attributes["latest_version"] != skill.LatestVersion {
					return fmt.Errorf("import adoption published or replaced a skill")
				}
				return nil
			}},
			{Config: withVersion, ResourceName: "anthropic_skill_version.test", ImportState: true, ImportStatePersist: true, ImportStateId: skill.ID + "/" + skill.LatestVersion},
			{Config: withVersion, Check: func(s *terraform.State) error {
				v := s.RootModule().Resources["anthropic_skill_version.test"]
				if v.Primary.Attributes["version"] != skill.LatestVersion {
					return fmt.Errorf("version import adoption replaced the version")
				}
				return nil
			}},
		},
	})
}
