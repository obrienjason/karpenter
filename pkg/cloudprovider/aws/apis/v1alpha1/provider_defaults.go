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

package v1alpha1

import (
	"context"
	"fmt"

	"github.com/awslabs/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/awslabs/karpenter/pkg/utils/functional"
	"github.com/awslabs/karpenter/pkg/utils/options"
	v1 "k8s.io/api/core/v1"
)

var ClusterDiscoveryTagKeyFormat = "kubernetes.io/cluster/%s"

// Default the constraints.
func (c *Constraints) Default(ctx context.Context) {
	c.defaultArchitecture()
	c.defaultCapacityTypes()
	c.defaultSubnets(options.Get(ctx).ClusterName)
	c.defaultSecurityGroups(options.Get(ctx).ClusterName)
}

func (c *Constraints) defaultCapacityTypes() {
	if _, ok := c.Labels[v1alpha5.LabelCapacityType]; ok {
		return
	}
	if functional.ContainsString(c.Requirements.Keys(), v1alpha5.LabelCapacityType) {
		return
	}
	c.Requirements = append(c.Requirements, v1.NodeSelectorRequirement{
		Key:      v1alpha5.LabelCapacityType,
		Operator: v1.NodeSelectorOpIn,
		Values:   []string{CapacityTypeOnDemand},
	})
}

func (c *Constraints) defaultArchitecture() {
	if _, ok := c.Labels[v1.LabelArchStable]; ok {
		return
	}
	if functional.ContainsString(c.Requirements.Keys(), v1.LabelArchStable) {
		return
	}
	c.Requirements = append(c.Requirements, v1.NodeSelectorRequirement{
		Key:      v1.LabelArchStable,
		Operator: v1.NodeSelectorOpIn,
		Values:   []string{v1alpha5.ArchitectureAmd64},
	})
}

func (c *Constraints) defaultSubnets(clusterName string) {
	if c.SubnetSelector != nil {
		return
	}
	c.SubnetSelector = map[string]string{fmt.Sprintf(ClusterDiscoveryTagKeyFormat, clusterName): "*"}
}

func (c *Constraints) defaultSecurityGroups(clusterName string) {
	if c.SecurityGroupSelector != nil {
		return
	}
	c.SecurityGroupSelector = map[string]string{fmt.Sprintf(ClusterDiscoveryTagKeyFormat, clusterName): "*"}
}
