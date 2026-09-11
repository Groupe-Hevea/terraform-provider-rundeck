package rundeck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

// A configured group must reach the payload, which is what moves the job.
func TestPlanToJobJSON_carriesGroupName(t *testing.T) {
	r := &jobResource{}
	job, err := r.planToJobJSON(context.Background(), &jobResourceModel{
		Name:        types.StringValue("test-job"),
		ProjectName: types.StringValue("test-project"),
		Command:     testSingleShellCommandList(t),
		GroupName:   types.StringValue("reorg/after"),
	})
	if err != nil {
		t.Fatalf("planToJobJSON: %v", err)
	}

	if job.Group != "reorg/after" {
		t.Errorf("group = %q, want %q", job.Group, "reorg/after")
	}
}

// A null group must leave "group" out of the payload entirely. That omission is
// what clears the group: Rundeck reads it back as
// se.groupPath = data['group'] ? data['group'] : null (ScheduledExecution.fromMap),
// so an absent key moves the job to the project root. Sending an empty string
// would be indistinguishable here, but only because omitempty drops it — this
// test pins that, since the module configures null rather than "".
func TestPlanToJobJSON_omitsNullGroupNameSoTheJobMovesToTheRoot(t *testing.T) {
	r := &jobResource{}
	job, err := r.planToJobJSON(context.Background(), &jobResourceModel{
		Name:        types.StringValue("test-job"),
		ProjectName: types.StringValue("test-project"),
		Command:     testSingleShellCommandList(t),
	})
	if err != nil {
		t.Fatalf("planToJobJSON: %v", err)
	}

	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if v, exists := decoded["group"]; exists {
		t.Errorf("group = %v, want it omitted so Rundeck moves the job to the project root", v)
	}
}

// TestAccJob_groupNameMovesInPlace is the behavioural test. Changing group_name
// must move the job rather than replace it: same UUID, same execution history,
// and the group actually changed server-side. The last step clears the group,
// which is how the job returns to the project root.
func TestAccJob_groupNameMovesInPlace(t *testing.T) {
	var (
		jobUUID          string
		executionsBefore []string
	)

	captureAndGiveItHistory := func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources["rundeck_job.test"]
		if !ok {
			return fmt.Errorf("rundeck_job.test not found in state")
		}
		jobUUID = rs.Primary.ID

		clients, err := getTestClients()
		if err != nil {
			return fmt.Errorf("error getting test client: %s", err)
		}
		if _, err := runJob(clients, jobUUID); err != nil {
			return fmt.Errorf("could not run the job to give it an execution history: %s", err)
		}
		executionsBefore, err = jobExecutionIDs(clients, jobUUID)
		if err != nil {
			return fmt.Errorf("could not list executions: %s", err)
		}
		if len(executionsBefore) == 0 {
			return fmt.Errorf("job has no execution after being run, so the history check below would prove nothing")
		}
		return nil
	}

	requireMovedInPlace := func(wantGroup string) resource.TestCheckFunc {
		return func(s *terraform.State) error {
			rs, ok := s.RootModule().Resources["rundeck_job.test"]
			if !ok {
				return fmt.Errorf("rundeck_job.test not found in state")
			}
			if rs.Primary.ID != jobUUID {
				return fmt.Errorf("job id changed across the move: %s -> %s (the job was replaced instead of moved)",
					jobUUID, rs.Primary.ID)
			}

			clients, err := getTestClients()
			if err != nil {
				return fmt.Errorf("error getting test client: %s", err)
			}

			job, err := GetJobJSON(clients.V1, jobUUID)
			if err != nil {
				return fmt.Errorf("could not read job back: %s", err)
			}
			if job.Group != wantGroup {
				return fmt.Errorf("group server-side = %q, want %q (the job did not move)", job.Group, wantGroup)
			}

			after, err := jobExecutionIDs(clients, jobUUID)
			if err != nil {
				return fmt.Errorf("could not list executions: %s", err)
			}
			kept := make(map[string]bool, len(after))
			for _, id := range after {
				kept[id] = true
			}
			for _, id := range executionsBefore {
				if !kept[id] {
					return fmt.Errorf("execution %s is gone after the move: the job was recreated and its history lost", id)
				}
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
				Config: testAccJobConfig_groupBefore,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("rundeck_job.test", "group_name", "reorg/before"),
					captureAndGiveItHistory,
				),
			},
			{
				Config: testAccJobConfig_groupAfter,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("rundeck_job.test", "group_name", "reorg/after"),
					requireMovedInPlace("reorg/after"),
				),
			},
			{
				// group_name absent from the configuration: the job goes back to
				// the project root, still without being replaced.
				Config: testAccJobConfig_groupCleared,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckNoResourceAttr("rundeck_job.test", "group_name"),
					requireMovedInPlace(""),
				),
			},
			{
				Config:   testAccJobConfig_groupCleared,
				PlanOnly: true,
			},
		},
	})
}

// runJob starts the job and returns the id of the execution Rundeck recorded.
func runJob(clients *RundeckClients, jobID string) (string, error) {
	url := fmt.Sprintf("%s/api/%s/job/%s/run", clients.BaseURL, clients.APIVersion, jobID)

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Rundeck-Auth-Token", clients.Token)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var execution struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&execution); err != nil {
		return "", fmt.Errorf("decoding run response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || execution.ID == 0 {
		return "", fmt.Errorf("run returned status %d with execution id %d", resp.StatusCode, execution.ID)
	}
	return strconv.Itoa(execution.ID), nil
}

// jobExecutionIDs lists every execution Rundeck holds for the job.
func jobExecutionIDs(clients *RundeckClients, jobID string) ([]string, error) {
	url := fmt.Sprintf("%s/api/%s/job/%s/executions", clients.BaseURL, clients.APIVersion, jobID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Rundeck-Auth-Token", clients.Token)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var body struct {
		Executions []struct {
			ID int `json:"id"`
		} `json:"executions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding executions response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing executions returned status %d", resp.StatusCode)
	}

	ids := make([]string, 0, len(body.Executions))
	for _, e := range body.Executions {
		ids = append(ids, strconv.Itoa(e.ID))
	}
	return ids, nil
}

const testAccJobConfig_groupProject = `
resource "rundeck_project" "test" {
  name        = "terraform-acc-test-job-group-move"
  description = "Test project for moving a job between groups"
  resource_model_source {
    type = "file"
    config = {
      format = "resourceyaml"
      file   = "/tmp/terraform-acc-tests.yaml"
    }
  }
}
`

const testAccJobConfig_groupBefore = testAccJobConfig_groupProject + `
resource "rundeck_job" "test" {
  project_name      = rundeck_project.test.name
  name              = "job-to-move"
  group_name        = "reorg/before"
  description       = "Job that will be moved between groups"
  execution_enabled = true
  command {
    shell_command = "echo hello"
  }
}
`

const testAccJobConfig_groupAfter = testAccJobConfig_groupProject + `
resource "rundeck_job" "test" {
  project_name      = rundeck_project.test.name
  name              = "job-to-move"
  group_name        = "reorg/after"
  description       = "Job that will be moved between groups"
  execution_enabled = true
  command {
    shell_command = "echo hello"
  }
}
`

const testAccJobConfig_groupCleared = testAccJobConfig_groupProject + `
resource "rundeck_job" "test" {
  project_name      = rundeck_project.test.name
  name              = "job-to-move"
  description       = "Job that will be moved between groups"
  execution_enabled = true
  command {
    shell_command = "echo hello"
  }
}
`
