package effective_test

import (
	"path/filepath"
	"testing"

	"github.com/jenkins-x/jx-helpers/v3/pkg/files"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient/giturl"
	"github.com/jenkins-x/jx-helpers/v3/pkg/testhelpers"
	"github.com/jenkins-x/lighthouse-client/pkg/filebrowser"
	"github.com/jenkins-x/lighthouse-client/pkg/filebrowser/fake"
	"github.com/jenkins-x/lighthouse-client/pkg/triggerconfig/inrepo"
	pipelinev1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"

	"github.com/jenkins-x-plugins/jx-pipeline/pkg/cmd/effective"
	"github.com/jenkins-x/jx-helpers/v3/pkg/yamls"
	"github.com/stretchr/testify/assert"

	"github.com/stretchr/testify/require"
)

var (
	testGitURL = "https://github.com/jstrachan/nodey560.git"

	// generateTestOutput enable to regenerate the expected output
	generateTestOutput = true
)

func TestPipelineEffective(t *testing.T) {
	tmpDir := t.TempDir()
	actual := filepath.Join(tmpDir, "pipeline.yaml")
	expectedFile := filepath.Join("test_data", ".lighthouse", "jenkins-x", "expected.yaml")

	_, o := effective.NewCmdPipelineEffective()

	o.Dir = "test_data"
	o.BatchMode = true
	o.OutFile = actual
	o.Resolver = CreateFakeResolver(t)
	err := o.Run()
	require.NoError(t, err, "Failed to run linter")

	assert.FileExists(t, actual, "should have generated file")

	pr := &pipelinev1.PipelineRun{}
	err = yamls.LoadFile(actual, pr)
	require.NoError(t, err, "failed to parse PipelineRun from %s", actual)

	t.Logf("generated valid YAML file %s\n", actual)

	if generateTestOutput {
		err = files.CopyFile(actual, expectedFile)
		require.NoError(t, err, "failed to copy %s to %s", actual, expectedFile)
		return
	}
	testhelpers.AssertTextFileContentsEqual(t, actual, expectedFile)
}

func CreateFakeResolver(t *testing.T) *inrepo.UsesResolver {
	filebrowsers, err := filebrowser.NewFileBrowsers(giturl.GitHubURL, fake.NewFakeFileBrowser(filepath.Join("test_data", "fake_file_browser"), true))
	require.NoError(t, err, "failed to create file browsers")

	return &inrepo.UsesResolver{
		FileBrowsers:     filebrowsers,
		OwnerName:        "myorg",
		LocalFileResolve: true,
		Cache:            inrepo.NewResolverCache(),
	}
}
