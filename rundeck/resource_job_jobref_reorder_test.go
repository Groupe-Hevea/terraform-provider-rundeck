package rundeck

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

// TestAccJob_cmd_referred_job_reorder moves job references around a workflow
// and checks that each one keeps its own settings.
//
// A job reference's uuid, name, group_name, project_name, run_for_each_node
// and node_step are Optional + Computed. They used to carry UseStateForUnknown,
// which reads prior state at the same list index: once the commands moved, an
// attribute left out of the configuration was filled from whichever command
// used to sit at that position.
//
//   - The first reorder swaps a reference to a job of another project with a reference
//     that names no project. The latter used to inherit the former's
//     project_name and node_step, and Rundeck then looked for the job in the
//     wrong project at run time.
//   - The second reorder moves references to positions that held a plain
//     command or a reference to another job. Attributes were planned as null
//     where there was no prior reference, read back as false, and the apply
//     failed with "Provider produced inconsistent result after apply".
//
// The reference by uuid covers the other half: Rundeck resolves it, and what
// it returns must neither show up as a diff on the next plan nor follow the
// reference that takes its place.
func TestAccJob_cmd_referred_job_reorder(t *testing.T) {
	const (
		local  = "local-target"
		remote = "remote-target"
	)

	// Each element is one command of the caller job, in workflow order.
	exec := `
  command {
    shell_command = "echo between"
  }`
	localRef := fmt.Sprintf(`
  command {
    job {
      name = %q
    }
  }`, local)
	remoteRef := fmt.Sprintf(`
  command {
    job {
      name         = %q
      project_name = rundeck_project.remote.name
      node_step    = true
    }
  }`, remote)
	uuidRef := `
  command {
    job {
      uuid = rundeck_job.local.id
    }
  }`

	// checkRefs asserts what each job reference holds, given the index in the
	// workflow of the reference by name, of the one to another project, and of
	// the one by uuid.
	checkRefs := func(localIdx, remoteIdx, uuidIdx int) resource.TestCheckFunc {
		l := fmt.Sprintf("command.%d.job.0.", localIdx)
		r := fmt.Sprintf("command.%d.job.0.", remoteIdx)
		u := fmt.Sprintf("command.%d.job.0.", uuidIdx)
		return resource.ComposeTestCheckFunc(
			resource.TestCheckResourceAttr("rundeck_job.caller", l+"name", local),
			resource.TestCheckNoResourceAttr("rundeck_job.caller", l+"uuid"),
			resource.TestCheckNoResourceAttr("rundeck_job.caller", l+"project_name"),
			resource.TestCheckResourceAttr("rundeck_job.caller", l+"node_step", "false"),
			resource.TestCheckResourceAttr("rundeck_job.caller", l+"run_for_each_node", "false"),
			resource.TestCheckResourceAttr("rundeck_job.caller", r+"name", remote),
			resource.TestCheckResourceAttr("rundeck_job.caller", r+"project_name", "terraform-acc-test-jobref-reorder-remote"),
			resource.TestCheckResourceAttr("rundeck_job.caller", r+"node_step", "true"),
			resource.TestCheckResourceAttr("rundeck_job.caller", r+"run_for_each_node", "true"),
			resource.TestCheckResourceAttrPair("rundeck_job.caller", u+"uuid", "rundeck_job.local", "id"),
			resource.TestCheckResourceAttr("rundeck_job.caller", u+"node_step", "false"),
		)
	}

	initial := testAccJobConfig_cmd_referred_job_reorder(exec, localRef, remoteRef, uuidRef)
	swapped := testAccJobConfig_cmd_referred_job_reorder(exec, remoteRef, localRef, uuidRef)
	moved := testAccJobConfig_cmd_referred_job_reorder(uuidRef, localRef, exec, remoteRef)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             testAccJobCheckDestroy(),
		Steps: []resource.TestStep{
			{Config: initial, Check: checkRefs(1, 2, 3)},
			{Config: initial, PlanOnly: true},
			{Config: swapped, Check: checkRefs(2, 1, 3)},
			{Config: swapped, PlanOnly: true},
			{Config: moved, Check: checkRefs(1, 3, 0)},
			{Config: moved, PlanOnly: true},
		},
	})
}

func testAccJobConfig_cmd_referred_job_reorder(commands ...string) string {
	caller := ""
	for _, c := range commands {
		caller += c
	}

	return fmt.Sprintf(`
resource "rundeck_project" "test" {
  name        = "terraform-acc-test-jobref-reorder"
  description = "Project of the job whose references are reordered"
  resource_model_source {
    type = "file"
    config = {
      format = "resourceyaml"
      file   = "/tmp/terraform-acc-tests.yaml"
    }
  }
}

resource "rundeck_project" "remote" {
  name        = "terraform-acc-test-jobref-reorder-remote"
  description = "Project holding a job referenced from another project"
  resource_model_source {
    type = "file"
    config = {
      format = "resourceyaml"
      file   = "/tmp/terraform-acc-tests.yaml"
    }
  }
}

resource "rundeck_job" "local" {
  project_name      = rundeck_project.test.name
  name              = "local-target"
  description       = "Referenced without a project"
  execution_enabled = true
  command {
    shell_command = "echo local"
  }
}

resource "rundeck_job" "remote" {
  project_name      = rundeck_project.remote.name
  name              = "remote-target"
  description       = "Referenced from another project"
  execution_enabled = true
  command {
    shell_command = "echo remote"
  }
}

resource "rundeck_job" "caller" {
  project_name      = rundeck_project.test.name
  name              = "caller-job"
  description       = "Job whose references are reordered"
  execution_enabled = true
  depends_on        = [rundeck_job.local, rundeck_job.remote]
%s
}
`, caller)
}
