package pipelines

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "github.com/jenkins-x/jx-api/v4/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/activities"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/naming"
	pipelinev1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	tektonversioned "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"
)

// ToPipelineActivityName creates an activity name from a pipeline run
func ToPipelineActivityName(pr *pipelinev1.PipelineRun, paList []v1.PipelineActivity) string {
	labels := pr.Labels
	if labels == nil {
		return ""
	}

	build := labels["build"]
	owner := naming.ToValidName(activities.GetLabel(labels, activities.OwnerLabels))
	repository := naming.ToValidName(activities.GetLabel(labels, activities.RepoLabels))
	branch := naming.ToValidName(activities.GetLabel(labels, activities.BranchLabels))

	if owner == "" || repository == "" || branch == "" {
		return ""
	}

	prefix := owner + "-" + repository + "-" + branch + "-"
	if build == "" {
		buildID := labels["lighthouse.jenkins-x.io/buildNum"]
		if buildID == "" {
			return ""
		}
		for i := range paList {
			pa := &paList[i]
			if pa.Labels == nil {
				continue
			}
			if pa.Labels["buildID"] == buildID || pa.Labels["lighthouse.jenkins-x.io/buildNum"] == buildID {
				if pa.Spec.Build != "" {
					pr.Labels["build"] = pa.Spec.Build
					return pa.Name
				}
			}
		}

		// no PA has the buildNum yet so lets try find the next PA build number...
		b := 1
		found := false
		var name string
		for i := range paList {
			pa := &paList[i]
			if strings.HasPrefix(pa.Name, prefix) {
				buildNum, _ := strconv.Atoi(strings.Split(pa.Name, prefix)[1])
				if buildNum >= b {
					b = buildNum
					found = true
				}
			}
		}
		if found {
			b++
		}
		build = strconv.Itoa(b)
		pr.Labels["build"] = build
		name = naming.ToValidName(prefix + build)
		return name
	}
	return naming.ToValidName(prefix + build)
}

func ToPipelineActivity(tektonclient tektonversioned.Interface, pr *pipelinev1.PipelineRun, pa *v1.PipelineActivity, overwriteSteps bool) {
	annotations := pr.Annotations
	labels := pr.Labels
	if pa.APIVersion == "" {
		pa.APIVersion = "jenkins.io/v1"
	}
	if pa.Kind == "" {
		pa.Kind = "PipelineActivity"
	}
	pa.Namespace = pr.Namespace

	if pa.Annotations == nil {
		pa.Annotations = map[string]string{}
	}
	if pa.Labels == nil {
		pa.Labels = map[string]string{}
	}
	for k, v := range annotations {
		switch k {
		case "lighthouse.jenkins-x.io/traceparent", "lighthouse.jenkins-x.io/tracestate":
			// the opentelemetry annotations holding trace context shouldn't be copied to other resources
		default:
			pa.Annotations[k] = v
		}
	}
	for k, v := range labels {
		pa.Labels[k] = v
	}

	ps := &pa.Spec
	if labels != nil {
		if ps.GitOwner == "" {
			ps.GitOwner = activities.GetLabel(labels, activities.OwnerLabels)
		}
		if ps.GitRepository == "" {
			ps.GitRepository = activities.GetLabel(labels, activities.RepoLabels)
		}
		if ps.GitBranch == "" {
			ps.GitBranch = activities.GetLabel(labels, activities.BranchLabels)
		}
		if ps.Build == "" {
			ps.Build = activities.GetLabel(labels, activities.BuildLabels)
		}
		if ps.Context == "" {
			ps.Context = activities.GetLabel(labels, activities.ContextLabels)
		}
		if ps.BaseSHA == "" {
			ps.BaseSHA = labels["lighthouse.jenkins-x.io/baseSHA"]
		}
		if ps.LastCommitSHA == "" {
			ps.LastCommitSHA = labels["lighthouse.jenkins-x.io/lastCommitSHA"]
		}
	}
	if ps.GitOwner != "" && ps.GitRepository != "" && ps.GitBranch != "" && ps.Pipeline == "" {
		ps.Pipeline = fmt.Sprintf("%s/%s/%s", ps.GitOwner, ps.GitRepository, ps.GitBranch)
	}
	if annotations != nil {
		if ps.GitURL == "" {
			ps.GitURL = annotations["lighthouse.jenkins-x.io/cloneURI"]
		}
	}

	podName := ""
	stageNames := map[string]bool{}
	var steps []v1.PipelineActivityStep
	var taskruns []pipelinev1.TaskRun
	for _, childReference := range pr.Status.ChildReferences {
		taskrun, err := tektonclient.TektonV1().TaskRuns(pa.Namespace).Get(context.TODO(), childReference.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		taskruns = append(taskruns, *taskrun)
		cleanedUpTaskName := strings.TrimPrefix(taskrun.Name[:len(taskrun.Name)-6], pr.Name+"-")
		stageName := strings.ReplaceAll(cleanedUpTaskName, "-", " ")
		stageNames[stageName] = true
		var stage *v1.PipelineActivityStep
		if podName == "" {
			podName = taskrun.Status.PodName
		}

		previousStepTerminated := false
		for i := range taskrun.Status.Steps {
			step := &taskrun.Status.Steps[i]
			name := step.Name
			var started *metav1.Time
			var completed *metav1.Time
			status := v1.ActivityStatusTypePending

			terminated := step.Terminated
			if terminated != nil {
				if terminated.ExitCode == 0 {
					status = v1.ActivityStatusTypeSucceeded
				} else if !terminated.FinishedAt.IsZero() {
					switch terminated.Reason {
					case pipelinev1.TaskRunReasonTimedOut.String():
						status = v1.ActivityStatusTypeTimedOut
					case pipelinev1.TaskRunReasonCancelled.String():
						status = v1.ActivityStatusTypeCancelled
					default:
						status = v1.ActivityStatusTypeFailed
					}
				}
				started = &terminated.StartedAt
				completed = &terminated.FinishedAt
				previousStepTerminated = true
			} else if step.Running != nil {
				if previousStepTerminated {
					started = &step.Running.StartedAt
					status = v1.ActivityStatusTypeRunning
				}
				previousStepTerminated = false
			}

			if status.IsTerminated() && completed == nil {
				completed = &metav1.Time{
					Time: time.Now(),
				}
			}

			coreActivityStep := v1.CoreActivityStep{
				Name:               Humanize(name),
				Description:        "",
				Status:             status,
				StartedTimestamp:   started,
				CompletedTimestamp: completed,
			}

			if stage == nil {
				stage = &v1.PipelineActivityStep{
					Kind: v1.ActivityStepKindTypeStage,
					Stage: &v1.StageActivityStep{
						CoreActivityStep: v1.CoreActivityStep{
							// Name:               Humanize(stageName),
							Name:             stageName,
							Description:      "",
							Status:           status,
							StartedTimestamp: started,
						},
					},
				}
			}
			stage.Stage.Steps = append(stage.Stage.Steps, coreActivityStep)
		}
		if len(taskrun.Status.Steps) == 0 {
			for _, m := range taskrun.Status.Conditions {
				// Only set the stage if the tekton pipeline run has succeeded and status is not unknown
				if m.Type == apis.ConditionSucceeded && m.Status != corev1.ConditionUnknown {
					// By default lets set the status to failed
					// This is ok as a pipeline that has succeeded will have steps and status set to true
					status := v1.ActivityStatusTypeFailed
					switch m.Reason {
					case pipelinev1.TaskRunReasonTimedOut.String():
						status = v1.ActivityStatusTypeTimedOut
					case pipelinev1.TaskRunReasonFailed.String():
						status = v1.ActivityStatusTypeFailed
					case pipelinev1.TaskRunReasonCancelled.String():
						status = v1.ActivityStatusTypeCancelled
					}
					stage = createStep(stageName, taskrun.Status.StartTime, status)
				}
			}
		}
		if stage != nil {
			// lets check we have a started time if we have at least 1 step
			if stage.Stage != nil && len(stage.Stage.Steps) > 0 {
				if stage.Stage.Steps[0].StartedTimestamp == nil {
					stage.Stage.Steps[0].StartedTimestamp = &metav1.Time{
						Time: time.Now(),
					}
				}
				if stage.Stage.StartedTimestamp == nil {
					stage.Stage.StartedTimestamp = stage.Stage.Steps[0].StartedTimestamp
				}
				// lets check the last step
				lastStep := stage.Stage.Steps[len(stage.Stage.Steps)-1]
				if stage.Stage.CompletedTimestamp == nil {
					stage.Stage.CompletedTimestamp = lastStep.CompletedTimestamp
				}
			}
			steps = append(steps, *stage)
		}
	}

	if overwriteSteps {
		for _, stage := range steps {
			if stage.Stage == nil {
				continue
			}
			idx := -1
			found := false
			for i := range ps.Steps {
				s := &ps.Steps[i]
				if s.Stage != nil && s.Stage.Name == stage.Stage.Name {
					s.Stage = stage.Stage
					found = true
					break
				}
				if s.Kind == v1.ActivityStepKindTypePreview || s.Kind == v1.ActivityStepKindTypePromote {
					if idx < 9 {
						idx = i
					}
				}
			}
			if !found {
				if idx < 0 {
					ps.Steps = append(ps.Steps, stage)
				} else {
					// lets add the new stage before the preview/promote stages
					var remaining []v1.PipelineActivityStep
					if idx < len(ps.Steps) {
						remaining = ps.Steps[idx:]
					}
					ps.Steps = append(ps.Steps[0:idx], stage)
					ps.Steps = append(ps.Steps, remaining...)
				}
			}
		}
	} else {
		// if the PipelineActivity has some real steps lets trust it; otherwise lets merge any preview/promote steps
		// with steps from the PipelineRun
		// lets add any missing steps from the PipelineActivity as they may have been created via a `jx promote` step
		hasStep := false
		for _, s := range ps.Steps {
			if s.Kind == v1.ActivityStepKindTypeStage && s.Stage != nil && s.Stage.Name != "Release" {
				hasStep = true
				break
			}
		}
		if !hasStep {
			for _, s := range ps.Steps {
				if s.Kind == v1.ActivityStepKindTypePreview || s.Kind == v1.ActivityStepKindTypePromote {
					steps = append(steps, s)
				}
			}
			ps.Steps = steps
		}
	}

	if len(ps.Steps) > 0 && ps.StartedTimestamp == nil {
		// lets default a start time
		if ps.Steps[0].Stage != nil {
			ps.StartedTimestamp = ps.Steps[0].Stage.StartedTimestamp
		}
	}
	if ps.StartedTimestamp == nil {
		ps.StartedTimestamp = &metav1.Time{
			Time: time.Now(),
		}
	}

	if len(ps.Steps) == 0 && !overwriteSteps {
		ps.Steps = append(ps.Steps, v1.PipelineActivityStep{
			Kind: v1.ActivityStepKindTypeStage,
			Stage: &v1.StageActivityStep{
				CoreActivityStep: v1.CoreActivityStep{
					Name:   "initialising",
					Status: v1.ActivityStatusTypeRunning,
				},
			},
		})
	}

	if podName != "" {
		pa.Labels["podName"] = podName
	}

	activities.UpdateStatus(pa, false, nil)

	addConditionsMessage(pr, pa)
	addTaskRunsMessage(taskruns, pa)
}

// addConditionsMessage reads the pr and gets the message for each condition then add it to the pa as Spec.Message
// It also edit the message so that it says PipelineActivity instead of PipelineRun
// It also replaces the pr.name with pa.name
func addConditionsMessage(pr *pipelinev1.PipelineRun, pa *v1.PipelineActivity) {
	for k := range pr.Status.Conditions {
		msg := pr.Status.Conditions[k].GetMessage()

		if strings.Contains(msg, "PipelineRun") {
			msg = strings.ReplaceAll(msg, "PipelineRun", "PipelineActivity")
		}

		if strings.Contains(msg, pa.Labels["tekton.dev/pipeline"]) {
			msg = strings.ReplaceAll(msg, pa.Labels["tekton.dev/pipeline"], pa.Name)
		}

		pa.Spec.Message = v1.ActivityMessageType(msg)
	}
}

// addConditionsMessage reads the pr and gets the message for each taskrun then add it to the pa as Spec.Steps[k].Stage.Message
// It replace TaskRun with Stage
func addTaskRunsMessage(taskruns []pipelinev1.TaskRun, pa *v1.PipelineActivity) {
	k1 := 0
	for i := range taskruns {
		taskrun := taskruns[i]
		msg := ""
		for k2 := range taskrun.Status.Conditions {
			msg = taskrun.Status.Conditions[k2].Message
		}
		msg = strings.ReplaceAll(msg, "TaskRun", "Stage")
		// Without this check, there is a panic in the codebase
		// ToDo(@maintainers): Test case for this
		if len(pa.Spec.Steps) > k1 {
			pa.Spec.Steps[k1].Stage.Message = v1.ActivityMessageType(msg)
		}
		k1++
	}
}

// Humanize splits into words and capitalises
func Humanize(text string) string {
	wordsText := strings.ReplaceAll(strings.ReplaceAll(text, "-", " "), "_", " ")
	words := strings.Split(wordsText, " ")
	for i := range words {
		words[i] = cases.Title(language.Und).String(words[i])
	}
	return strings.Join(words, " ")
}

func createStep(stageName string, startTime *metav1.Time, status v1.ActivityStatusType) *v1.PipelineActivityStep {
	return &v1.PipelineActivityStep{
		Kind: v1.ActivityStepKindTypeStage,
		Stage: &v1.StageActivityStep{
			CoreActivityStep: v1.CoreActivityStep{
				Name:             stageName,
				Description:      "",
				Status:           status,
				StartedTimestamp: startTime,
			},
		},
	}
}
