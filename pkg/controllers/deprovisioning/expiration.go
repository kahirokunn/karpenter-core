/*
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

package deprovisioning

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"k8s.io/utils/clock"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
)

// Expiration is a subreconciler that deletes empty nodes.
// Expiration will respect TTLSecondsAfterEmpty
type Expiration struct {
	clock       clock.Clock
	kubeClient  client.Client
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
	recorder    events.Recorder
}

func NewExpiration(clk clock.Clock, kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner, recorder events.Recorder) *Expiration {
	return &Expiration{
		clock:       clk,
		kubeClient:  kubeClient,
		cluster:     cluster,
		provisioner: provisioner,
		recorder:    recorder,
	}
}

// ShouldDeprovision is a predicate used to filter deprovisionable nodes
func (e *Expiration) ShouldDeprovision(ctx context.Context, c *Candidate) bool {
	return e.clock.Now().After(getExpirationTime(c.Node, c.provisioner))
}

// SortCandidates orders expired nodes by when they've expired
func (e *Expiration) filterAndSortCandidates(ctx context.Context, nodes []*Candidate) ([]*Candidate, error) {
	candidates, err := filterCandidates(ctx, e.kubeClient, e.recorder, nodes)
	if err != nil {
		return nil, fmt.Errorf("filtering candidates, %w", err)
	}
	sort.Slice(candidates, func(i int, j int) bool {
		return getExpirationTime(candidates[i].Node, candidates[i].provisioner).Before(getExpirationTime(candidates[j].Node, candidates[j].provisioner))
	})
	return candidates, nil
}

// ComputeCommand generates a deprovisioning command given deprovisionable nodes
func (e *Expiration) ComputeCommand(ctx context.Context, nodes ...*Candidate) (Command, error) {
	candidates, err := e.filterAndSortCandidates(ctx, nodes)
	if err != nil {
		return Command{}, fmt.Errorf("filtering candidates, %w", err)
	}

	for _, candidate := range candidates {
		// Check if we need to create any nodes.
		newMachines, allPodsScheduled, err := simulateScheduling(ctx, e.kubeClient, e.cluster, e.provisioner, candidate)
		if err != nil {
			// if a candidate node is now deleting, just retry
			if errors.Is(err, errCandidateDeleting) {
				continue
			}
			return Command{}, err
		}
		// Log when all pods can't schedule, as the command will get executed immediately.
		if !allPodsScheduled {
			logging.FromContext(ctx).With("node", candidate.Name).Debugf("continuing to expire node after scheduling simulation failed to schedule all pods")
		}

		logging.FromContext(ctx).With("ttl", time.Duration(ptr.Int64Value(candidates[0].provisioner.Spec.TTLSecondsUntilExpired))*time.Second).
			With("delay", time.Since(getExpirationTime(candidates[0].Node, candidates[0].provisioner))).Infof("triggering termination for expired node after TTL")
		return Command{
			candidates:   []*Candidate{candidate},
			action:       actionReplace,
			replacements: newMachines,
		}, nil
	}
	return Command{action: actionDoNothing}, nil
}

// String is the string representation of the deprovisioner
func (e *Expiration) String() string {
	return metrics.ExpirationReason
}

func getExpirationTime(node *v1.Node, provisioner *v1alpha5.Provisioner) time.Time {
	if provisioner == nil || provisioner.Spec.TTLSecondsUntilExpired == nil {
		// If not defined, return some much larger time.
		return time.Date(5000, 0, 0, 0, 0, 0, 0, time.UTC)
	}
	expirationTTL := time.Duration(ptr.Int64Value(provisioner.Spec.TTLSecondsUntilExpired)) * time.Second
	return node.CreationTimestamp.Add(expirationTTL)
}
