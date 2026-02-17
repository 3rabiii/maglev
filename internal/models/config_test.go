package models

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGitPropertiesJSONTags(t *testing.T) {
	props := GitProperties{
		GitBranch:       "main",
		GitCommitId:     "abc12345",
		GitBuildVersion: "1.0.0",
		GitDirty:        "false",
	}

	data, err := json.Marshal(props)
	assert.Nil(t, err)
	jsonString := string(data)

	assert.Contains(t, jsonString, `"git.branch":"main"`)
	assert.Contains(t, jsonString, `"git.commit.id":"abc12345"`)
	assert.Contains(t, jsonString, `"git.build.version":"1.0.0"`)

	assert.NotContains(t, jsonString, "GitBranch")
}
