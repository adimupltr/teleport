/*
Copyright 2023 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package awsoidc

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/gravitational/trace"
)

// DeployDBServiceRequest contains the required fields to deploy a Database Service.
type DeployDBServiceRequest struct {
	// Region is the AWS Region
	Region string

	// SubnetIDs are the subnets associated with the service.
	SubnetIDs []string
}

// CheckAndSetDefaults checks if the required fields are present.
func (req *DeployDBServiceRequest) CheckAndSetDefaults() error {
	if req.Region == "" {
		return trace.BadParameter("region is required")
	}

	if len(req.SubnetIDs) == 0 {
		return trace.BadParameter("at least one subnetid must be provided")
	}

	return nil
}

// DeployDBServiceResponse contains a page of AWS Instances.
type DeployDBServiceResponse struct {
	ClusterName        string
	ServiceName        string
	TaskDefinitionName string
}

// DeployDBServiceClient describes the required methods to List Databases (Instances and Clusters) using a 3rd Party API.
type DeployDBServiceClient interface {
}

type DefaultDeployDBServiceClient struct {
	ecsClient *ecs.Client
}

func NewDeployDBServiceClient(ctx context.Context, clientReq *AWSClientRequest) (*DefaultDeployDBServiceClient, error) {
	ecsClient, err := newECSClient(ctx, clientReq)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &DefaultDeployDBServiceClient{
		ecsClient: ecsClient,
	}, nil
}

// DeployDBService calls the following AWS API:
// https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_DescribeInstances.html
// It returns a list of Instances and an optional NextToken that can be used to fetch the next page
func DeployDBService(ctx context.Context, clt *DefaultDeployDBServiceClient, req DeployDBServiceRequest) (*DeployDBServiceResponse, error) {
	clusterName := "MarcoTestDefaultCluster"
	serviceName := "teleport-db-service"
	taskName := "teleport-database-service-13-iam-all"
	taskAgentContainerName := "teleport-database-service-13"

	// OR  "amazonlinux:2"
	// with `yum install -y curl hostname procps && bash -c "$(curl -fsSL https://<proxy>.teleportdemo.net/scripts/<token>/install-database.sh)"`,
	teleportDistrolessDebug13 := "public.ecr.aws/gravitational/teleport-distroless-debug:13"
	teleportDIstrolessDebug13StartAgentCommand := `
cat << EOF > /etc/teleport.yaml
version: v3
teleport:
  nodename: mynodename
  join_params:
    token_name: iam-token
    method: iam
  proxy_server: <proxy>.teleportdemo.net:443
  log:
    output: stderr
    severity: INFO
auth_service:
  enabled: no
ssh_service:
  enabled: no
proxy_service:
  enabled: no
db_service:
  enabled: "yes"
  resources:  
    - labels:
        "*": "*"
EOF
teleport start`

	taskAgentContainerImage := teleportDistrolessDebug13
	taskAgentContainerCommand := teleportDIstrolessDebug13StartAgentCommand

	// Pre-Req: a task definition
	// Task Definition:
	taskDefOut, err := clt.ecsClient.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family: &taskName,
		RequiresCompatibilities: []ecsTypes.Compatibility{
			ecsTypes.CompatibilityFargate,
		},
		// Ensure Cpu and Memory use one of the allowed combinations:
		// https://docs.aws.amazon.com/AmazonECS/latest/developerguide/task_definition_parameters.html
		Cpu:    sP("256"),
		Memory: sP("512"),

		NetworkMode: ecsTypes.NetworkModeAwsvpc,

		// Required to connect to the DB:
		// It needs:
		// - action=rds-db:connect, resource=<db resource id arn> (eg, arn:aws:rds-db:us-east-1:<accountid>:dbuser:db-<db-resource-id>/*)
		TaskRoleArn: sP("MarcoRoleDBAgentAccess"),

		// Only required if LogConfiguration is enabled
		// It needs:
		// - action=logs:*, resource=* (we can be more specific about actions and resources)
		ExecutionRoleArn: sP("MarcoRoleDBAgentAccess"),

		ContainerDefinitions: []ecsTypes.ContainerDefinition{{
			Command: []string{
				"-c",
				taskAgentContainerCommand,
			},
			EntryPoint: []string{"sh"},
			Image:      &taskAgentContainerImage,
			Name:       &taskAgentContainerName,
			LogConfiguration: &ecsTypes.LogConfiguration{
				LogDriver: ecsTypes.LogDriverAwslogs,
				Options: map[string]string{
					"awslogs-group":         "marcogroup-",
					"awslogs-region":        "us-east-1",
					"awslogs-create-group":  "true",
					"awslogs-stream-prefix": "marcologs-",
				},
			},
		}},
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	taskDefinitionARN := *taskDefOut.TaskDefinition.TaskDefinitionArn

	// Pre-Req: a cluster which is going to be used as a default cluster when Creating the Service
	createClusterOut, err := clt.ecsClient.CreateCluster(ctx, &ecs.CreateClusterInput{
		ClusterName:       &clusterName,
		CapacityProviders: []string{string(ecsTypes.LaunchTypeFargate)},
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	clusterARN := *createClusterOut.Cluster.ClusterArn

	// if task changes (eg, cmd changes), then CreateService fails
	oneAgent := int32(1)
	createServiceOut, err := clt.ecsClient.CreateService(ctx, &ecs.CreateServiceInput{
		ServiceName:    &serviceName,
		DesiredCount:   &oneAgent,
		LaunchType:     ecsTypes.LaunchTypeFargate,
		TaskDefinition: &taskDefinitionARN,
		Cluster:        &clusterARN,
		NetworkConfiguration: &ecsTypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecsTypes.AwsVpcConfiguration{
				AssignPublicIp: ecsTypes.AssignPublicIpEnabled, // no internet connection otherwise
				Subnets:        req.SubnetIDs,
			},
		},
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &DeployDBServiceResponse{
		ClusterName:        *createClusterOut.Cluster.ClusterName,
		ServiceName:        *createServiceOut.Service.ServiceName,
		TaskDefinitionName: *taskDefOut.TaskDefinition.TaskDefinitionArn,
	}, nil
}

func sP(s string) *string {
	return &s
}
