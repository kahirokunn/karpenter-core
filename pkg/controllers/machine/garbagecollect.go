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

package machine

import (
	"context"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/prometheus/client_golang/prometheus"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/metrics"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
)

type GarbageCollect struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	lastChecked   *cache.Cache
}

func (g *GarbageCollect) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if !machine.StatusConditions().GetCondition(v1alpha5.MachineCreated).IsTrue() {
		return reconcile.Result{}, nil
	}
	// If there is no node representation for the machine, then check if there is a representation at the cloudprovider
	if _, err := machineutil.NodeForMachine(ctx, g.kubeClient, machine); err == nil || !machineutil.IsNodeNotFoundError(err) {
		return reconcile.Result{}, nil
	}
	if _, expireTime, ok := g.lastChecked.GetWithExpiration(client.ObjectKeyFromObject(machine).String()); ok {
		return reconcile.Result{RequeueAfter: time.Until(expireTime)}, nil
	}
	// Check if there is cloudprovider machine that is backing this in-cluster machine. If there isn't, then delete the Machine
	if _, err := g.cloudProvider.Get(ctx, machine.Status.ProviderID); cloudprovider.IsMachineNotFoundError(err) {
		if err = g.kubeClient.Delete(ctx, machine); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
		logging.FromContext(ctx).Debugf("garbage collecting machine with no cloudprovider representation")
		metrics.MachinesTerminatedCounter.With(prometheus.Labels{
			metrics.ReasonLabel:      "garbage_collected",
			metrics.ProvisionerLabel: machine.Labels[v1alpha5.ProvisionerNameLabelKey],
		})
		return reconcile.Result{}, nil
	}
	g.lastChecked.SetDefault(client.ObjectKeyFromObject(machine).String(), nil)
	return reconcile.Result{}, nil
}
