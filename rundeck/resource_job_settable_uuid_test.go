package rundeck

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

// A configured uuid must reach the payload's "uuid" field: that is the one the
// import reads back, and the one uuidOption=preserve acts on.
func TestPlanToJobJSON_carriesConfiguredUUID(t *testing.T) {
	const want = "11111111-2222-3333-4444-555555555555"

	r := &jobResource{}
	job, err := r.planToJobJSON(context.Background(), &jobResourceModel{
		Name:        types.StringValue("test-job"),
		ProjectName: types.StringValue("test-project"),
		Command:     testSingleShellCommandList(t),
		UUID:        types.StringValue(want),
	})
	if err != nil {
		t.Fatalf("planToJobJSON: %v", err)
	}

	if job.UUID != want {
		t.Errorf("uuid = %q, want %q", job.UUID, want)
	}
}

// Unset, the uuid must stay out of the payload so Rundeck keeps assigning one.
func TestPlanToJobJSON_omitsUnsetUUID(t *testing.T) {
	r := &jobResource{}
	job, err := r.planToJobJSON(context.Background(), &jobResourceModel{
		Name:        types.StringValue("test-job"),
		ProjectName: types.StringValue("test-project"),
		Command:     testSingleShellCommandList(t),
	})
	if err != nil {
		t.Fatalf("planToJobJSON: %v", err)
	}

	if job.UUID != "" {
		t.Errorf("uuid = %q, want it empty so the field is omitted", job.UUID)
	}
}

func TestCanonicalUUIDPattern(t *testing.T) {
	cases := []struct {
		value string
		valid bool
	}{
		{"11111111-2222-3333-4444-555555555555", true},
		{"6bf08fc5-835e-4dea-a83f-56953879b497", true},
		{"6BF08FC5-835E-4DEA-A83F-56953879B497", false},
		{"6bf08fc5835e4dea a83f56953879b497", false},
		{"6bf08fc5-835e-4dea-a83f", false},
		// Rundeck's own constraint would accept this; the provider does not.
		{"deploy-prod-app1", false},
		{"", false},
	}

	for _, tc := range cases {
		if got := canonicalUUIDPattern.MatchString(tc.value); got != tc.valid {
			t.Errorf("MatchString(%q) = %v, want %v", tc.value, got, tc.valid)
		}
	}
}

// The behavioural test: a configured uuid must be the job's actual UUID in
// Rundeck, and stay so across a re-apply.
func TestAccJob_configuredUUIDIsPreserved(t *testing.T) {
	const want = "8c1d4e2a-7b93-4f61-95c8-2e0a6d3f7b14"

	requireJobID := func(want string) resource.TestCheckFunc {
		return func(s *terraform.State) error {
			rs, ok := s.RootModule().Resources["rundeck_job.test"]
			if !ok {
				return fmt.Errorf("rundeck_job.test not found in state")
			}
			if rs.Primary.ID != want {
				return fmt.Errorf("job id = %s, want %s (Rundeck did not preserve the configured uuid)",
					rs.Primary.ID, want)
			}
			return nil
		}
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccJobCheckDestroy(),
		Steps: []resource.TestStep{
			{
				Config: testAccJobConfig_settableUUID,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("rundeck_job.test", "uuid", want),
					requireJobID(want),
				),
			},
			{
				Config:   testAccJobConfig_settableUUID,
				PlanOnly: true,
			},
		},
	})
}

const testAccJobConfig_settableUUID = `
resource "rundeck_project" "test" {
  name        = "terraform-acc-test-job-uuid"
  description = "Test project for settable job uuid"
  resource_model_source {
    type = "file"
    config = {
      format = "resourceyaml"
      file   = "/tmp/terraform-acc-tests.yaml"
    }
  }
}

resource "rundeck_job" "test" {
  project_name      = rundeck_project.test.name
  uuid              = "8c1d4e2a-7b93-4f61-95c8-2e0a6d3f7b14"
  name              = "job-with-pinned-uuid"
  description       = "Job whose identity comes from the configuration"
  execution_enabled = true
  command {
    shell_command = "echo hello"
  }
}
`
