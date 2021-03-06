// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package stream

import (
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/copilot-cli/internal/pkg/aws/ecs"
)

const (
	ecsPrimaryDeploymentStatus = "PRIMARY"
)

var ecsEventFailureKeywords = []string{"fail", "unhealthy", "error", "throttle", "unable", "missing"}

// ECSServiceDescriber is the interface to describe an ECS service.
type ECSServiceDescriber interface {
	Service(clusterName, serviceName string) (*ecs.Service, error)
}

// ECSDeployment represent an ECS rolling update deployment.
type ECSDeployment struct {
	Status          string
	TaskDefRevision string
	DesiredCount    int
	RunningCount    int
	FailedCount     int
	PendingCount    int
	RolloutState    string
}

// ECSService is a description of an ECS service.
type ECSService struct {
	Deployments         []ECSDeployment
	LatestFailureEvents []string
}

// ECSDeploymentStreamer is a Streamer for ECSService descriptions until the deployment is completed.
type ECSDeploymentStreamer struct {
	client                 ECSServiceDescriber
	cluster                string
	service                string
	deploymentCreationTime time.Time

	subscribers   []chan ECSService
	done          chan struct{}
	pastEventIDs  map[string]bool
	eventsToFlush []ECSService
}

// NewECSDeploymentStreamer creates a new ECSDeploymentStreamer that streams service descriptions
// since the deployment creation time and until the primary deployment is completed.
func NewECSDeploymentStreamer(ecs ECSServiceDescriber, cluster, service string, deploymentCreationTime time.Time) *ECSDeploymentStreamer {
	return &ECSDeploymentStreamer{
		client:                 ecs,
		cluster:                cluster,
		service:                service,
		deploymentCreationTime: deploymentCreationTime,
		done:                   make(chan struct{}),
		pastEventIDs:           make(map[string]bool),
	}
}

// Subscribe returns a read-only channel that will receive service descriptions from the ECSDeploymentStreamer.
func (s *ECSDeploymentStreamer) Subscribe() <-chan ECSService {
	c := make(chan ECSService)
	s.subscribers = append(s.subscribers, c)
	return c
}

// Fetch retrieves and stores ECSService descriptions since the deployment's creation time
// until the primary deployment's running count is equal to its desired count.
// If an error occurs from describe service, returns a wrapped err.
// Otherwise, returns the time the next Fetch should be attempted.
func (s *ECSDeploymentStreamer) Fetch() (next time.Time, err error) {
	out, err := s.client.Service(s.cluster, s.service)
	if err != nil {
		return next, fmt.Errorf("fetch service description: %w", err)
	}
	var deployments []ECSDeployment
	for _, deployment := range out.Deployments {
		status := aws.StringValue(deployment.Status)
		desiredCount, runningCount := aws.Int64Value(deployment.DesiredCount), aws.Int64Value(deployment.RunningCount)
		deployments = append(deployments, ECSDeployment{
			Status:          status,
			TaskDefRevision: parseRevisionFromTaskDefARN(aws.StringValue(deployment.TaskDefinition)),
			DesiredCount:    int(desiredCount),
			RunningCount:    int(runningCount),
			FailedCount:     int(aws.Int64Value(deployment.FailedTasks)),
			PendingCount:    int(aws.Int64Value(deployment.PendingCount)),
			RolloutState:    aws.StringValue(deployment.RolloutState),
		})
		if status == ecsPrimaryDeploymentStatus && desiredCount == runningCount {
			// The deployment is done, notify that there is no need for another Fetch call beyond this point.
			close(s.done)
		}
	}
	var failureMsgs []string
	for _, event := range out.Events {
		if createdAt := aws.TimeValue(event.CreatedAt); createdAt.Before(s.deploymentCreationTime) {
			break
		}
		id := aws.StringValue(event.Id)
		if _, ok := s.pastEventIDs[id]; ok {
			break
		}
		if msg := aws.StringValue(event.Message); isFailureServiceEvent(msg) {
			failureMsgs = append(failureMsgs, msg)
		}
		s.pastEventIDs[id] = true
	}
	s.eventsToFlush = append(s.eventsToFlush, ECSService{
		Deployments:         deployments,
		LatestFailureEvents: failureMsgs,
	})
	return time.Now().Add(streamerFetchIntervalDuration), nil
}

// Notify flushes all new events to the streamer's subscribers.
func (s *ECSDeploymentStreamer) Notify() {
	for _, event := range s.eventsToFlush {
		for _, sub := range s.subscribers {
			sub <- event
		}
	}
	s.eventsToFlush = nil // reset after flushing all events.
}

// Close closes all subscribed channels notifying them that no more events will be sent.
func (s *ECSDeploymentStreamer) Close() {
	for _, sub := range s.subscribers {
		close(sub)
	}
}

// Done returns a channel that's closed when there are no more events that can be fetched.
func (s *ECSDeploymentStreamer) Done() <-chan struct{} {
	return s.done
}

// parseRevisionFromTaskDefARN returns the revision number as string given the ARN of a task definition.
// For example, given the input "arn:aws:ecs:us-west-2:1111:task-definition/webapp-test-frontend:3"
// the output is "3".
func parseRevisionFromTaskDefARN(arn string) string {
	familyName := strings.Split(arn, "/")[1]
	return strings.Split(familyName, ":")[1]
}

func isFailureServiceEvent(msg string) bool {
	for _, kw := range ecsEventFailureKeywords {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
