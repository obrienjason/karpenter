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
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/awslabs/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/awslabs/karpenter/pkg/cloudprovider"
	"github.com/awslabs/karpenter/pkg/controllers/allocation/binpacking"
	"github.com/awslabs/karpenter/pkg/controllers/allocation/scheduling"
)

const (
	maxBatchWindow   = 10 * time.Second
	batchIdleTimeout = 1 * time.Second
)

// Controller for the resource
type Controller struct {
	Batcher       *Batcher
	Filter        *Filter
	Scheduler     *scheduling.Scheduler
	Launcher      *Launcher
	KubeClient    client.Client
	CloudProvider cloudprovider.CloudProvider
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, coreV1Client corev1.CoreV1Interface, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		Batcher:   NewBatcher(maxBatchWindow, batchIdleTimeout),
		Filter:    &Filter{KubeClient: kubeClient},
		Scheduler: scheduling.NewScheduler(kubeClient, cloudProvider),
		Launcher: &Launcher{
			Packer:        &binpacking.Packer{},
			CloudProvider: cloudProvider,
			KubeClient:    kubeClient,
			CoreV1Client:  coreV1Client,
		},
		KubeClient:    kubeClient,
		CloudProvider: cloudProvider,
	}
}

// Reconcile executes an allocation control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(fmt.Sprintf("allocation.provisioner/%s", req.Name)))
	logging.FromContext(ctx).Infof("Starting provisioning loop")
	// Fetch provisioner
	provisioner, err := c.provisionerFor(ctx, req.NamespacedName)
	if err != nil {
		if errors.IsNotFound(err) {
			c.Batcher.Wait(&v1alpha5.Provisioner{})
			logging.FromContext(ctx).Errorf("Provisioner \"%s\" not found. Create the \"default\" provisioner or specify an alternative using the nodeSelector %s", req.Name, v1alpha5.ProvisionerNameLabelKey)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	// Wait on a pod batch
	logging.FromContext(ctx).Infof("Waiting to batch additional pods")
	c.Batcher.Wait(provisioner)
	// Filter pods
	pods, err := c.Filter.GetProvisionablePods(ctx, provisioner)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("filtering pods, %w", err)
	}
	logging.FromContext(ctx).Infof("Found %d provisionable pods", len(pods))
	if len(pods) == 0 {
		logging.FromContext(ctx).Infof("Watching for pod events")
		return reconcile.Result{}, nil
	}
	// Get Instance Types Options
	instanceTypes, err := c.CloudProvider.GetInstanceTypes(ctx, &provisioner.Spec.Constraints)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting instance types, %w", err)
	}
	// Separate pods by scheduling constraints
	schedules, err := c.Scheduler.Solve(ctx, provisioner, instanceTypes, pods)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("solving scheduling constraints, %w", err)
	}
	// Launch capacity and bind pods
	if err := c.Launcher.Launch(ctx, schedules, instanceTypes); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{Requeue: true}, nil
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	err := controllerruntime.
		NewControllerManagedBy(m).
		Named("Allocation").
		For(&v1alpha5.Provisioner{}).
		Watches(
			&source.Kind{Type: &v1.Pod{}},
			handler.EnqueueRequestsFromMapFunc(c.podToProvisioner(ctx)),
			// Only process pod update events
			builder.WithPredicates(
				predicate.Funcs{
					CreateFunc:  func(_ event.CreateEvent) bool { return false },
					DeleteFunc:  func(_ event.DeleteEvent) bool { return false },
					GenericFunc: func(_ event.GenericEvent) bool { return false },
				},
			),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(c)
	c.Batcher.Start(ctx)
	return err
}

// provisionerFor fetches the provisioner and returns a provisioner w/ default runtime values
func (c *Controller) provisionerFor(ctx context.Context, name types.NamespacedName) (*v1alpha5.Provisioner, error) {
	provisioner := &v1alpha5.Provisioner{}
	if err := c.KubeClient.Get(ctx, name, provisioner); err != nil {
		return nil, err
	}
	return provisioner, nil
}

// podToProvisioner is a function handler to transform pod objs to provisioner reconcile requests
func (c *Controller) podToProvisioner(ctx context.Context) func(o client.Object) []reconcile.Request {
	return func(o client.Object) (requests []reconcile.Request) {
		pod := o.(*v1.Pod)
		if err := c.Filter.isUnschedulable(pod); err != nil {
			return nil
		}
		provisionerKey := v1alpha5.DefaultProvisioner
		if name, ok := pod.Spec.NodeSelector[v1alpha5.ProvisionerNameLabelKey]; ok {
			provisionerKey.Name = name
		}
		provisioner, err := c.provisionerFor(ctx, provisionerKey)
		if err != nil {
			if errors.IsNotFound(err) {
				// Queue and batch a reconcile request for a non-existent, empty provisioner
				// This will reduce the number of repeated error messages about a provisioner not existing
				c.Batcher.Add(&v1alpha5.Provisioner{})
				notFoundProvisioner := v1alpha5.DefaultProvisioner.Name
				if name, ok := pod.Spec.NodeSelector[v1alpha5.ProvisionerNameLabelKey]; ok {
					notFoundProvisioner = name
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: notFoundProvisioner}}}
			}
			return nil
		}
		if err = c.Filter.isProvisionable(pod, provisioner); err != nil {
			return nil
		}
		c.Batcher.Add(provisioner)
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: provisioner.Name}}}
	}
}
