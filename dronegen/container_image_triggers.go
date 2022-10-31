// Copyright 2021 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"path"
)

// Describes a Drone trigger as it pertains to container image building.
type TriggerInfo struct {
	Trigger           trigger
	Name              string
	Flags             *TriggerFlags
	SupportedVersions []*ReleaseVersion
	SetupSteps        []step
}

// This type is mainly used to make passing these vars around cleaner
type TriggerFlags struct {
	ShouldAffectProductionImages bool
	ShouldBuildNewImages         bool
	UseUniqueStagingTag          bool
	ShouldOnlyPublishFullSemver  bool
}

func NewTagTrigger(branchMajorVersion string) *TriggerInfo {
	tagTrigger := triggerTag

	return &TriggerInfo{
		Trigger: tagTrigger,
		Name:    "tag",
		Flags: &TriggerFlags{
			ShouldAffectProductionImages: false,
			ShouldBuildNewImages:         true,
			UseUniqueStagingTag:          false,
			ShouldOnlyPublishFullSemver:  true,
		},
		SupportedVersions: []*ReleaseVersion{
			{
				MajorVersion:        branchMajorVersion,
				ShellVersion:        "$DRONE_TAG",
				RelativeVersionName: "branch",
			},
		},
	}
}

func NewPromoteTrigger(branchMajorVersion string) *TriggerInfo {
	promoteTrigger := triggerPromote
	promoteTrigger.Target.Include = append(promoteTrigger.Target.Include, "promote-docker")

	return &TriggerInfo{
		Trigger: promoteTrigger,
		Name:    "promote",
		Flags: &TriggerFlags{
			ShouldAffectProductionImages: true,
			ShouldBuildNewImages:         false,
			UseUniqueStagingTag:          false,
			ShouldOnlyPublishFullSemver:  false,
		},
		SupportedVersions: []*ReleaseVersion{
			{
				MajorVersion:        branchMajorVersion,
				ShellVersion:        "$DRONE_TAG",
				RelativeVersionName: "branch",
			},
		},
		SetupSteps: verifyValidPromoteRunSteps(),
	}
}

func NewCronTrigger(latestMajorVersions []string) *TriggerInfo {
	if len(latestMajorVersions) == 0 {
		return nil
	}

	majorVersionVarDirectory := "/go/vars/full-version"

	supportedVersions := make([]*ReleaseVersion, 0, len(latestMajorVersions))
	if len(latestMajorVersions) > 0 {
		latestMajorVersion := latestMajorVersions[0]
		supportedVersions = append(supportedVersions, &ReleaseVersion{
			MajorVersion:        latestMajorVersion,
			ShellVersion:        readCronShellVersionCommand(majorVersionVarDirectory, latestMajorVersion),
			RelativeVersionName: "current-version",
			SetupSteps:          []step{getLatestSemverStep(latestMajorVersion, majorVersionVarDirectory)},
		})

		if len(latestMajorVersions) > 1 {
			for i, majorVersion := range latestMajorVersions[1:] {
				supportedVersions = append(supportedVersions, &ReleaseVersion{
					MajorVersion:        majorVersion,
					ShellVersion:        readCronShellVersionCommand(majorVersionVarDirectory, majorVersion),
					RelativeVersionName: fmt.Sprintf("previous-version-%d", i+1),
					SetupSteps:          []step{getLatestSemverStep(majorVersion, majorVersionVarDirectory)},
				})
			}
		}
	}

	return &TriggerInfo{
		Trigger: cronTrigger([]string{"teleport-container-images-cron"}),
		Name:    "cron",
		Flags: &TriggerFlags{
			ShouldAffectProductionImages: true,
			ShouldBuildNewImages:         true,
			UseUniqueStagingTag:          true,
			ShouldOnlyPublishFullSemver:  false,
		},
		SupportedVersions: supportedVersions,
	}
}

func getLatestSemverStep(majorVersion string, majorVersionVarDirectory string) step {
	// We don't use "/go/src/github.com/gravitational/teleport" here as a later stage
	// may need to clone a different version, and "/go" persists between steps
	cloneDirectory := "/tmp/teleport"
	majorVersionVarPath := path.Join(majorVersionVarDirectory, majorVersion)
	return step{
		Name:  fmt.Sprintf("Find the latest available semver for %s", majorVersion),
		Image: fmt.Sprintf("golang:%s", GoVersion),
		Commands: append(
			cloneRepoCommands(cloneDirectory, fmt.Sprintf("branch/%s", majorVersion)),
			fmt.Sprintf("mkdir -pv %q", majorVersionVarDirectory),
			fmt.Sprintf("cd %q", path.Join(cloneDirectory, "build.assets", "tooling", "cmd", "query-latest")),
			fmt.Sprintf("go run . %q > %q", majorVersion, majorVersionVarPath),
			fmt.Sprintf("echo Found full semver \"$(cat %q)\" for major version %q", majorVersionVarPath, majorVersion),
		),
	}
}

func readCronShellVersionCommand(majorVersionDirectory, majorVersion string) string {
	return fmt.Sprintf("$(cat '%s')", path.Join(majorVersionDirectory, majorVersion))
}

// Drone triggers must all evaluate to "true" for a pipeline to be executed.
// As a result these pipelines are duplicated for each trigger.
// See https://docs.drone.io/pipeline/triggers/ for details.
func (ti *TriggerInfo) buildPipelines() []pipeline {
	pipelines := make([]pipeline, 0, len(ti.SupportedVersions))
	for _, teleportVersion := range ti.SupportedVersions {
		pipeline := teleportVersion.buildVersionPipeline(ti.SetupSteps, ti.Flags)
		pipeline.Name += "-" + ti.Name
		pipeline.Trigger = ti.Trigger

		pipelines = append(pipelines, pipeline)
	}

	return pipelines
}
