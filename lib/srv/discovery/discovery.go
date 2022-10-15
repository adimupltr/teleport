/*
Copyright 2022 Gravitational, Inc.

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

package discovery

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/cloud"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/srv/server"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
)

// Config provides configuration for the discovery server.
type Config struct {
	// Clients is an interface for retrieving cloud clients.
	Clients cloud.Clients
	// AWSMatchers is a list of AWS EC2 matchers.
	AWSMatchers []services.AWSMatcher
	// AzureMatchers is a list of Azure VM matchers.
	AzureMatchers []services.AzureMatcher
	// NodeWatcher is a node watcher.
	NodeWatcher *services.NodeWatcher
	// Emitter is events emitter, used to submit discrete events
	Emitter apievents.Emitter
	// AccessPoint is a discovery access point
	AccessPoint auth.DiscoveryAccessPoint
}

// Server is a discovery server, used to discover cloud resources for
// inclusion in Teleport
type Server struct {
	*Config

	ctx context.Context
	// cancelfn is used with ctx when stopping the discovery server
	cancelfn context.CancelFunc

	// log is used for logging.
	log *logrus.Entry

	// ec2Watcher periodically retrieves cloud resources, currently
	// only EC2
	ec2Watcher *server.EC2Watcher
	// ec2Installer is used to start the installation process on discovered EC2 nodes
	ec2Installer *server.SSMInstaller

	azureWatcher *server.AzureWatcher
}

// New initializes a discovery Server
func New(ctx context.Context, cfg *Config) (*Server, error) {
	if len(cfg.AWSMatchers) == 0 && len(cfg.AzureMatchers) == 0 {
		return nil, trace.NotFound("no matchers present")
	}

	localCtx, cancelfn := context.WithCancel(ctx)
	s := &Server{
		Config: cfg,
		log: logrus.WithFields(logrus.Fields{
			trace.Component: teleport.ComponentDiscovery,
		}),
		ctx:      localCtx,
		cancelfn: cancelfn,
	}

	var err error
	if len(s.AWSMatchers) > 0 {
		s.ec2Watcher, err = server.NewEC2Watcher(s.ctx, s.AWSMatchers, s.Clients)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		s.ec2Installer = server.NewSSMInstaller(server.SSMInstallerConfig{
			Emitter: cfg.Emitter,
		})
	}

	if len(s.AzureMatchers) > 0 {
		s.azureWatcher, err = server.NewAzureWatcher(s.ctx, s.AzureMatchers, s.Clients)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	return s, nil
}

func (s *Server) filterExistingEC2Nodes(instances *server.EC2Instances) {
	nodes := s.NodeWatcher.GetNodes(func(n services.Node) bool {
		labels := n.GetAllLabels()
		_, accountOK := labels[types.AWSAccountIDLabel]
		_, instanceOK := labels[types.AWSInstanceIDLabel]
		return accountOK && instanceOK
	})

	var filtered []*ec2.Instance
outer:
	for _, inst := range instances.Instances {
		for _, node := range nodes {
			match := types.MatchLabels(node, map[string]string{
				types.AWSAccountIDLabel:  instances.AccountID,
				types.AWSInstanceIDLabel: aws.StringValue(inst.InstanceId),
			})
			if match {
				continue outer
			}
		}
		filtered = append(filtered, inst)
	}
	instances.Instances = filtered
}

func genEC2InstancesLogStr(instances []*ec2.Instance) string {
	return genInstancesLogStr(instances, func(i *ec2.Instance) string {
		return aws.StringValue(i.InstanceId)
	})
}

func genAzureInstancesLogStr(instances []*armcompute.VirtualMachine) string {
	return genInstancesLogStr(instances, func(i *armcompute.VirtualMachine) string {
		return *i.ID
	})
}

func genInstancesLogStr[T any](instances []T, getID func(T) string) string {
	var logInstances strings.Builder
	for idx, inst := range instances {
		if idx == 10 || idx == (len(instances)-1) {
			logInstances.WriteString(getID(inst))
			break
		}
		logInstances.WriteString(getID(inst) + ", ")
	}
	if len(instances) > 10 {
		logInstances.WriteString(fmt.Sprintf("... + %d instance IDs truncated", len(instances)-10))
	}

	return fmt.Sprintf("[%s]", logInstances.String())
}

func (s *Server) handleEC2Instances(instances *server.EC2Instances) error {
	client, err := s.Clients.GetAWSSSMClient(instances.Region)
	if err != nil {
		return trace.Wrap(err)
	}
	s.filterExistingEC2Nodes(instances)
	if len(instances.Instances) == 0 {
		return trace.NotFound("all fetched nodes already enrolled")
	}

	s.log.Debugf("Running Teleport installation on these instances: AccountID: %s, Instances: %s",
		instances.AccountID, genEC2InstancesLogStr(instances.Instances))
	req := server.SSMRunRequest{
		DocumentName: instances.DocumentName,
		SSM:          client,
		Instances:    instances.Instances,
		Params:       instances.Parameters,
		Region:       instances.Region,
		AccountID:    instances.AccountID,
	}
	return trace.Wrap(s.ec2Installer.Run(s.ctx, req))
}

func (s *Server) handleEC2Discovery() {
	go s.ec2Watcher.Run()
	for {
		select {
		case instances := <-s.ec2Watcher.InstancesC:
			s.log.Debugf("EC2 instances discovered (AccountID: %s, Instances: %v), starting installation",
				instances.AccountID, genEC2InstancesLogStr(instances.Instances))
			if err := s.handleEC2Instances(&instances); err != nil {
				if trace.IsNotFound(err) {
					s.log.Debug("All discovered EC2 instances are already part of the cluster.")
				} else {
					s.log.WithError(err).Error("Failed to enroll discovered EC2 instances.")
				}
			}
		case <-s.ctx.Done():
			s.ec2Watcher.Stop()
			return
		}
	}
}

func (s *Server) handleAzureInstances(instances *server.AzureInstances) error {
	return trace.NotImplemented("Automatic Azure node joining not implemented")
}

func (s *Server) handleAzureDiscovery() {
	go s.azureWatcher.Run()
	for {
		select {
		case instances := <-s.azureWatcher.InstancesC:
			s.log.Debugf("Azure instances discovered (SubscriptionID: %s, Instances: %v), starting installation",
				instances.SubscriptionID, genAzureInstancesLogStr(instances.Instances),
			)
			if err := s.handleAzureInstances(&instances); err != nil {
				if trace.IsNotFound(err) {
					s.log.Debug("All discovered Azure VMs are already part of the cluster.")
				} else {
					s.log.WithError(err).Error("Failed to enroll discovered Azure VMs.")
				}
			}
		case <-s.ctx.Done():
			s.azureWatcher.Stop()
			return
		}
	}
}

// Start starts the discovery service.
func (s *Server) Start() error {
	if s.ec2Watcher != nil {
		go s.handleEC2Discovery()
	}
	if s.azureWatcher != nil {
		go s.handleAzureDiscovery()
	}
	return nil
}

// Stop stops the discovery service.
func (s *Server) Stop() {
	s.cancelfn()
	if s.ec2Watcher != nil {
		s.ec2Watcher.Stop()
	}
	if s.azureWatcher != nil {
		s.azureWatcher.Stop()
	}
}

// Wait will block while the server is running.
func (s *Server) Wait() error {
	<-s.ctx.Done()
	if err := s.ctx.Err(); err != nil && err != context.Canceled {
		return trace.Wrap(err)
	}
	return nil
}
