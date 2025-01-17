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

package allocation

import (
	"context"
	"fmt"

	"github.com/awslabs/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/awslabs/karpenter/pkg/utils/pod"
	"github.com/awslabs/karpenter/pkg/utils/ptr"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Filter struct {
	KubeClient client.Client
}

func (f *Filter) GetProvisionablePods(ctx context.Context, provisioner *v1alpha5.Provisioner) ([]*v1.Pod, error) {
	// 1. List Pods that aren't scheduled
	pods := &v1.PodList{}
	if err := f.KubeClient.List(ctx, pods, client.MatchingFields{"spec.nodeName": ""}); err != nil {
		return nil, fmt.Errorf("listing unscheduled pods, %w", err)
	}
	// 2. Filter pods that aren't provisionable
	provisionable := []*v1.Pod{}
	for i := range pods.Items {
		p := pods.Items[i]
		if err := f.isProvisionable(&p, provisioner); err != nil {
			logging.FromContext(ctx).Debugf("Ignored pod %s/%s when allocating for provisioner %s, %s",
				p.Name, p.Namespace, provisioner.Name, err.Error(),
			)
			continue
		}
		provisionable = append(provisionable, ptr.Pod(p))
	}
	return provisionable, nil
}

func (f *Filter) isProvisionable(pod *v1.Pod, provisioner *v1alpha5.Provisioner) error {
	return multierr.Combine(
		f.isUnschedulable(pod),
		f.validateAffinity(pod),
		f.validateTopology(pod),
		f.matchesProvisioner(pod, provisioner),
	)
}

func (f *Filter) isUnschedulable(p *v1.Pod) error {
	if !pod.FailedToSchedule(p) {
		return fmt.Errorf("awaiting scheduling")
	}
	if pod.IsOwnedByDaemonSet(p) {
		return fmt.Errorf("owned by daemonset")
	}
	if pod.IsOwnedByNode(p) {
		return fmt.Errorf("owned by node")
	}
	return nil
}

func (f *Filter) matchesProvisioner(pod *v1.Pod, provisioner *v1alpha5.Provisioner) error {
	name, ok := pod.Spec.NodeSelector[v1alpha5.ProvisionerNameLabelKey]
	if ok && provisioner.Name == name {
		return nil
	}
	if !ok && provisioner.Name == v1alpha5.DefaultProvisioner.Name {
		return nil
	}
	return fmt.Errorf("matched another provisioner, %s", name)
}

func (f *Filter) validateTopology(pod *v1.Pod) (errs error) {
	for _, constraint := range pod.Spec.TopologySpreadConstraints {
		if supported := sets.NewString(v1.LabelHostname, v1.LabelTopologyZone); !supported.Has(constraint.TopologyKey) {
			errs = multierr.Append(errs, fmt.Errorf("unsupported topology key, %s not in %s", constraint.TopologyKey, supported))
		}
	}
	return errs
}

func (f *Filter) validateAffinity(pod *v1.Pod) (errs error) {
	if pod.Spec.Affinity == nil {
		return nil
	}
	if pod.Spec.Affinity.PodAffinity != nil {
		errs = multierr.Append(errs, fmt.Errorf("pod affinity is not supported"))
	}
	if pod.Spec.Affinity.PodAntiAffinity != nil {
		errs = multierr.Append(errs, fmt.Errorf("pod anti-affinity is not supported"))
	}
	if pod.Spec.Affinity.NodeAffinity != nil {
		for _, term := range pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			errs = multierr.Append(errs, validateNodeSelectorTerm(term.Preference))
		}
		if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			for _, term := range pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
				errs = multierr.Append(errs, validateNodeSelectorTerm(term))
			}
		}
	}
	return errs
}

func validateNodeSelectorTerm(term v1.NodeSelectorTerm) (errs error) {
	if term.MatchFields != nil {
		errs = multierr.Append(errs, fmt.Errorf("matchFields is not supported"))
	}
	if term.MatchExpressions != nil {
		for _, requirement := range term.MatchExpressions {
			if !sets.NewString(string(v1.NodeSelectorOpIn), string(v1.NodeSelectorOpNotIn)).Has(string(requirement.Operator)) {
				errs = multierr.Append(errs, fmt.Errorf("unsupported operator, %s", requirement.Operator))
			}
		}
	}
	return errs
}
