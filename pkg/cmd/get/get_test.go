package get_test

import (
	"context"
	"testing"

	"github.com/jenkins-x-plugins/jx-pipeline/pkg/cmd/get"
	"github.com/jenkins-x-plugins/jx-pipeline/pkg/tektonlog"
	"github.com/stretchr/testify/assert"
	pipelinev1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	faketekton "github.com/tektoncd/pipeline/pkg/client/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testDevNameSpace = "jx-test"

func pipelineRun(ns, repo, branch, owner, context string, now metav1.Time) *pipelinev1.PipelineRun {
	return &pipelinev1.PipelineRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "PR1",
			Namespace: ns,
			Labels: map[string]string{
				tektonlog.LabelRepo:    repo,
				tektonlog.LabelBranch:  branch,
				tektonlog.LabelOwner:   owner,
				tektonlog.LabelContext: context,
			},
		},
		Spec: pipelinev1.PipelineRunSpec{
			Params: []pipelinev1.Param{
				{
					Name: "version",
					Value: pipelinev1.ParamValue{
						Type:      pipelinev1.ParamTypeString,
						StringVal: "v1",
					},
				},
				{
					Name: "build_id",
					Value: pipelinev1.ParamValue{
						Type:      pipelinev1.ParamTypeString,
						StringVal: "1",
					},
				},
			},
		},
		Status: pipelinev1.PipelineRunStatus{
			PipelineRunStatusFields: pipelinev1.PipelineRunStatusFields{
				CompletionTime: &now,
			},
		},
	}
}

var pipelineCases = []struct {
	desc      string
	namespace string
	repo      string
	branch    string
	owner     string
	context   string
}{
	{"", testDevNameSpace, "testRepo", "testBranch", "testOwner", "testContext"},
	{"", testDevNameSpace, "testRepo", "testBranch", "testOwner", ""},
}

func TestExecuteGetPipelines(t *testing.T) {
	for _, v := range pipelineCases {
		t.Run(v.desc, func(t *testing.T) {

			_, o := get.NewCmdPipelineGet()

			o.KubeClient = fake.NewSimpleClientset()
			o.TektonClient = faketekton.NewSimpleClientset(pipelineRun(v.namespace, v.repo, v.branch, v.owner, v.context, metav1.Now()))
			o.Namespace = v.namespace
			o.BatchMode = true
			o.Ctx = context.Background()

			err := o.Run()

			// Execution should not error out
			assert.NoError(t, err, "execute get pipelines")
		})
	}

}
