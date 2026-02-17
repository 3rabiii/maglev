package models

type GitProperties struct {
	GitBranch                string `json:"git.branch"`
	GitBuildHost             string `json:"git.build.host"`
	GitBuildTime             string `json:"git.build.time"`
	GitBuildUserEmail        string `json:"git.build.user.email"`
	GitBuildUserName         string `json:"git.build.user.name"`
	GitBuildVersion          string `json:"git.build.version"`
	GitClosestTagCommitCount string `json:"git.closest.tag.commit.count"`
	GitClosestTagName        string `json:"git.closest.tag.name"`
	GitCommitId              string `json:"git.commit.id"`
	GitCommitIdAbbrev        string `json:"git.commit.id.abbrev"`
	GitCommitIdDescribe      string `json:"git.commit.id.describe"`
	GitCommitIdDescribeShort string `json:"git.commit.id.describe-short"`
	GitCommitMessageFull     string `json:"git.commit.message.full"`
	GitCommitMessageShort    string `json:"git.commit.message.short"`
	GitCommitTime            string `json:"git.commit.time"`
	GitCommitUserEmail       string `json:"git.commit.user.email"`
	GitCommitUserName        string `json:"git.commit.user.name"`
	GitDirty                 string `json:"git.dirty"`
	GitRemoteOriginUrl       string `json:"git.remote.origin.url"`
	GitTags                  string `json:"git.tags"`
}

type ConfigModel struct {
	GitProperties   GitProperties `json:"gitProperties"`
	Id              string        `json:"id"`
	Name            string        `json:"name"`
	ServiceDateFrom string        `json:"serviceDateFrom"`
	ServiceDateTo   string        `json:"serviceDateTo"`
}
