package processor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jenkins-x/jx-helpers/v3/pkg/yamls"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	pipelinev1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	LighthouseTaskParams = []pipelinev1.ParamSpec{
		{
			Name:        "BUILD_ID",
			Type:        "string",
			Description: "The unique build number",
			Default:     nil,
		},
		{
			Name:        "JOB_NAME",
			Type:        "string",
			Description: "The fileName of the job which is the trigger context fileName",
			Default:     nil,
		},
		{
			Name:        "JOB_SPEC",
			Type:        "string",
			Description: "The specification of the job",
			Default:     nil,
		},
		{
			Name:        "JOB_TYPE",
			Type:        "string",
			Description: "The kind of the job: postsubmit or presubmit",
			Default:     nil,
		},
		{
			Name:        "PULL_BASE_REF",
			Type:        "string",
			Description: "The base git reference of the pull request",
			Default:     nil,
		},
		{
			Name:        "PULL_BASE_SHA",
			Type:        "string",
			Description: "The git sha of the base of the pull request",
			Default:     nil,
		},
		{
			Name:        "PULL_NUMBER",
			Type:        "string",
			Description: "The git pull request number",
			Default:     &pipelinev1.ParamValue{Type: "string", StringVal: ""},
		},
		{
			Name:        "PULL_PULL_REF",
			Type:        "string",
			Description: "The git pull request ref in the form 'refs/pull/$PULL_NUMBER/head'",
			Default:     &pipelinev1.ParamValue{Type: "string", StringVal: ""},
		},
		{
			Name:        "PULL_PULL_SHA",
			Type:        "string",
			Description: "The git pull reference strings of base and latest in the form 'master:$PULL_BASE_SHA,$PULL_NUMBER:$PULL_PULL_SHA:refs/pull/$PULL_NUMBER/head'",
			Default:     nil,
		},
		{
			Name:        "PULL_REFS",
			Type:        "string",
			Description: "The git pull reference strings of base and latest in the form 'master:$PULL_BASE_SHA,$PULL_NUMBER:$PULL_PULL_SHA:refs/pull/$PULL_NUMBER/head'",
			Default:     nil,
		},
		{
			Name:        "REPO_NAME",
			Type:        "string",
			Description: "The git repository fileName",
			Default:     nil,
		},
		{
			Name:        "REPO_OWNER",
			Type:        "string",
			Description: "The git repository owner (user or organisation)",
			Default:     nil,
		},
		{
			Name:        "REPO_URL",
			Type:        "string",
			Description: "The URL of the git repo to clone",
			Default:     nil,
		},
	}

	LighthouseEnvs = ParamsToEnvVars(LighthouseTaskParams)

	HomeEnv = v1.EnvVar{
		Name:  "HOME",
		Value: "/workspace",
	}

	trueRef = true

	DefaultEnvFroms = []v1.EnvFromSource{
		{
			SecretRef: &v1.SecretEnvSource{
				LocalObjectReference: v1.LocalObjectReference{
					Name: "jx-boot-job-env-vars",
				},
				Optional: &trueRef,
			},
		},
	}

	serviceAccountName = "tekton-bot"
)

type RemoteTasksMigrator struct {
	workspaceVolumeQuantity resource.Quantity
	gitResolver             *GitRefResolver
}

// NewRemoteTasksMigrator creates a new uses migrator
func NewRemoteTasksMigrator(overrideSHA string, workspaceVolumeQuantity resource.Quantity) *RemoteTasksMigrator {
	return &RemoteTasksMigrator{workspaceVolumeQuantity: workspaceVolumeQuantity,
		gitResolver: NewGitRefResolver(overrideSHA),
	}
}

func (p *RemoteTasksMigrator) ProcessPipeline(pipeline *pipelinev1.Pipeline, path string) (bool, error) { //nolint:revive
	return false, nil
}

// ProcessPipelineRun processes a PipelineRun and migrates it to either a new PipelineRun or to individual Tasks
// based on whether it is a parent or child PipelineRun
func (p *RemoteTasksMigrator) ProcessPipelineRun(prs *pipelinev1.PipelineRun, path string) (bool, error) {
	log.Logger().Infof("Processing pipeline run %s", path)
	if taskCount := len(prs.Spec.PipelineSpec.Tasks); taskCount != 1 {
		// All jx pipelines should only have one task
		return false, fmt.Errorf("pipeline run %s has %d tasks. Expecting 1", path, taskCount)
	}

	stepTemplate := prs.Spec.PipelineSpec.Tasks[0].TaskSpec.StepTemplate
	if stepTemplate == nil {
		// If there is no step template then we can assume that the pipeline run is a parent pipeline run and
		// so we should migrate it to individual tasks
		log.Logger().Infof("No step template found. Migrating it to individual tasks.")
		return p.migrateToTasks(prs, path)
	}

	prsParent, err := p.gitResolver.NewRefFromUsesImage(stepTemplate.Image, "")
	if err != nil {
		return false, fmt.Errorf("failed to create new ref from uses image %s: %w", stepTemplate.Image, err)
	}
	if prsParent != nil {
		// If the step template image is a uses image then we can assume that the pipeline run is a child pipeline run,
		// and so we should migrate it to a new pipeline run
		log.Logger().Infof("StepTemplate has image \"%s\". Migrating to new pipelineRun", stepTemplate.Image)
		return p.migrateToNewPipelineRun(prs)
	}

	// Otherwise we can assume that the pipeline run is a parent pipeline run, and so we should migrate it to individual tasks
	log.Logger().Infof("StepTemplate has image \"%s\". Migrating to individual tasks", stepTemplate.Image)
	return p.migrateToTasks(prs, path)
}

// migrateToNewPipelineRun takes a PipelineRun and migrates it to a native Tekton PipelineRun converting the steps
// of the original to individual Tasks within the new PipelineRun. This overwrites the original PipelineRun found at path.
func (p *RemoteTasksMigrator) migrateToNewPipelineRun(prs *pipelinev1.PipelineRun) (bool, error) {
	steps := prs.Spec.PipelineSpec.Tasks[0].TaskSpec.Steps
	newPrs := pipelinev1.PipelineRun{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "tekton.dev/v1",
			Kind:       "PipelineRun",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: prs.Name,
		},
		Spec: pipelinev1.PipelineRunSpec{
			PipelineSpec: &pipelinev1.PipelineSpec{
				Workspaces: []pipelinev1.PipelineWorkspaceDeclaration{
					{
						Name: "pipeline-ws",
					},
				},
			},
			TaskRunTemplate: pipelinev1.PipelineTaskRunTemplate{
				ServiceAccountName: serviceAccountName,
			},
			Timeouts: &pipelinev1.TimeoutFields{
				Pipeline: prs.Spec.Timeouts.Pipeline,
			},
			Workspaces: []pipelinev1.WorkspaceBinding{
				{
					Name: "pipeline-ws",
					VolumeClaimTemplate: &v1.PersistentVolumeClaim{
						Spec: v1.PersistentVolumeClaimSpec{
							AccessModes: []v1.PersistentVolumeAccessMode{
								v1.ReadWriteOnce,
							},
							Resources: v1.VolumeResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceStorage: p.workspaceVolumeQuantity,
								},
							},
						},
					},
				},
			},
		},
	}

	pipelineTasks := make([]pipelinev1.PipelineTask, len(steps))
	for idx := range steps {
		newPipelineTask, err := p.NewPipelineTaskFromStepAndPipelineRun(&steps[idx], prs)
		if err != nil {
			return false, err
		}
		if idx > 0 {
			// If this is not the first step then we need to set the run after to the fileName of the previous task
			newPipelineTask.RunAfter = []string{pipelineTasks[idx-1].Name}
		}
		pipelineTasks[idx] = newPipelineTask
	}
	newPrs.Spec.PipelineSpec.Tasks = pipelineTasks

	if err := newPrs.DeepCopy().Validate(context.Background()); err != nil {
		return false, err
	}
	*prs = newPrs
	return true, nil
}

// migrateToTasks takes a PipelineRun and migrates the steps of the original to individual Tasks. This writes the
// new Tasks to a subdirectory in path named the same as the original PipelineRun. The original PipelineRun is not
// modified.
func (p *RemoteTasksMigrator) migrateToTasks(prs *pipelinev1.PipelineRun, path string) (bool, error) {
	steps := prs.Spec.PipelineSpec.Tasks[0].TaskSpec.Steps

	subDir := filepath.Join(filepath.Dir(path), prs.Name)
	err := os.MkdirAll(subDir, 0700)
	if err != nil {
		return false, err
	}

	tasks := make([]pipelinev1.Task, len(steps))
	for idx := range steps {
		newTask, err := p.NewTaskFromStepAndPipelineRun(&steps[idx], prs, false)
		if err != nil {
			return false, fmt.Errorf("failed to create task from step[%d] \"%s\": %w", idx, steps[idx].Name, err)
		}

		if err := yamls.SaveFile(newTask, filepath.Join(subDir, newTask.Name+".yaml")); err != nil {
			return false, err
		}
		tasks[idx] = newTask
	}
	return false, nil
}

func (p *RemoteTasksMigrator) populateTaskValuesFromPipelineRun(task *pipelinev1.Task, pr *pipelinev1.PipelineRun, isEmbeddedTask bool) {
	if task.Spec.StepTemplate == nil {
		task.Spec.StepTemplate = &pipelinev1.StepTemplate{}
	}

	task.Spec.StepTemplate.WorkingDir = pr.Spec.PipelineSpec.Tasks[0].TaskSpec.TaskSpec.StepTemplate.WorkingDir

	if !isEmbeddedTask {
		// Embedded tasks don't need to have the default params added as they get populated by lighthouse
		p.appendDefaultValues(task)
	}
}

func (p *RemoteTasksMigrator) appendDefaultValues(task *pipelinev1.Task) {
	if task.Spec.StepTemplate == nil {
		task.Spec.StepTemplate = &pipelinev1.StepTemplate{}
	}

	task.Spec.Params = AppendParamsIfNotPresent(task.Spec.Params, LighthouseTaskParams)
	task.Spec.StepTemplate.EnvFrom = AppendEnvsFromIfNotPresent(task.Spec.StepTemplate.EnvFrom, DefaultEnvFroms)

	task.Spec.StepTemplate.Env = AppendEnvsIfNotPresent(task.Spec.StepTemplate.Env, LighthouseEnvs)
	// We need to replace the HOME env if it's already present in the task as we're moving it from
	// /tekton/home to /workspace
	task.Spec.StepTemplate.Env = ReplaceOrAppendEnv(task.Spec.StepTemplate.Env, HomeEnv)

}

func (p *RemoteTasksMigrator) NewPipelineTaskFromStepAndPipelineRun(step *pipelinev1.Step, prs *pipelinev1.PipelineRun) (pipelinev1.PipelineTask, error) {
	stepParentRef, err := p.gitResolver.NewRefFromUsesImage(step.Image, step.Name)
	if err != nil {
		return pipelinev1.PipelineTask{}, err
	}

	if stepParentRef != nil {
		// If the step has a parent then we can just convert it to a pipeline task
		return p.pipelineTaskFromParentRef(stepParentRef.GetParentFileName(), stepParentRef), nil
	}

	if step.Image != "" {
		// If the step has no parent and also has an image then it's a root step and can just be converted to an embedded task
		return p.pipelineTaskFromStep(step, prs)
	}

	// Otherwise it's a child step and is inherited from the parent pipelineRun in the stepTemplate
	stepTemplateParentRef, err := p.gitResolver.NewRefFromUsesImage(prs.Spec.PipelineSpec.Tasks[0].TaskSpec.StepTemplate.Image, step.Name)
	if err != nil {
		return pipelinev1.PipelineTask{}, err
	}

	return p.pipelineTaskFromParentRef(step.Name, stepTemplateParentRef), nil
}

func (p *RemoteTasksMigrator) pipelineTaskFromStep(step *pipelinev1.Step, prs *pipelinev1.PipelineRun) (pipelinev1.PipelineTask, error) {
	task, err := p.NewTaskFromStepAndPipelineRun(step, prs, true)
	if err != nil {
		return pipelinev1.PipelineTask{}, err
	}
	return pipelinev1.PipelineTask{
		Name: step.Name,
		TaskSpec: &pipelinev1.EmbeddedTask{
			TaskSpec: task.Spec,
		},
	}, nil
}

func (p *RemoteTasksMigrator) pipelineTaskFromParentRef(name string, ref *GitRef) pipelinev1.PipelineTask {
	return pipelinev1.PipelineTask{
		Name: name,
		TaskRef: &pipelinev1.TaskRef{
			ResolverRef: ref.ToResolverRef(),
		},
		Workspaces: []pipelinev1.WorkspacePipelineTaskBinding{
			{Name: "output", Workspace: "pipeline-ws"},
		},
	}
}

// NewTaskFromStepAndPipelineRun takes a step and a PipelineRun and creates a Task from them.
func (p *RemoteTasksMigrator) NewTaskFromStepAndPipelineRun(step *pipelinev1.Step, prs *pipelinev1.PipelineRun, isEmbeddedTask bool) (pipelinev1.Task, error) {
	taskStep := step

	workingDir := step.WorkingDir
	if workingDir == "" {
		// if the step does not have a working dir then we need to use the working dir from the parent pipeline run
		workingDir = prs.Spec.PipelineSpec.Tasks[0].TaskSpec.TaskSpec.StepTemplate.WorkingDir
	}

	newTask := pipelinev1.Task{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Task",
			APIVersion: "tekton.dev/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: step.Name,
		},
		Spec: pipelinev1.TaskSpec{
			Steps: []pipelinev1.Step{
				*taskStep,
			},
			StepTemplate: &pipelinev1.StepTemplate{
				WorkingDir: workingDir,
			},
			Workspaces: []pipelinev1.WorkspaceDeclaration{
				{
					Name:        "output",
					MountPath:   "/workspace",
					Description: "The workspace used to store the cloned git repository and the generated files",
				},
			},
		},
	}

	p.populateTaskValuesFromPipelineRun(&newTask, prs, isEmbeddedTask)
	if err := newTask.DeepCopy().Validate(context.Background()); err != nil {
		return pipelinev1.Task{}, fmt.Errorf("failed to validate new task: %w", err)
	}
	return newTask, nil
}

// ProcessTask processes a task and appends the default params and environment variables to it
func (p *RemoteTasksMigrator) ProcessTask(task *pipelinev1.Task, path string) (bool, error) {
	log.Logger().Infof("Processing task %s", path)
	// We need to add all the default params to the task
	p.appendDefaultValues(task)
	err := task.DeepCopy().Validate(context.Background())
	if err != nil {
		return false, fmt.Errorf("failed to validate task: %w", err)
	}
	return true, nil
}

func (p *RemoteTasksMigrator) ProcessTaskRun(tr *pipelinev1.TaskRun, path string) (bool, error) { //nolint:revive
	return false, nil
}
