// Package agent keeps the GPUNode resource of the local node up to date.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kybernate/kybernate/api/v1alpha1"
	"github.com/kybernate/kybernate/internal/inventory"
)

// Agent publishes the GPU inventory of one node as a GPUNode resource.
type Agent struct {
	Client   client.Client
	Source   inventory.Source
	NodeName string
	Interval time.Duration
	Log      *slog.Logger
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time

	ready atomic.Bool
}

// Ready reports whether the inventory has been published at least once.
func (a *Agent) Ready() bool { return a.ready.Load() }

// Run publishes the inventory immediately and then at every interval until
// the context ends.
func (a *Agent) Run(ctx context.Context) error {
	ticker := time.NewTicker(a.Interval)
	defer ticker.Stop()
	for {
		if err := a.Sync(ctx); err != nil {
			a.Log.Error("publishing the GPU inventory failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Sync collects the inventory once and writes it to the GPUNode status.
func (a *Agent) Sync(ctx context.Context) error {
	inv, collectErr := a.Source.Collect()

	gpuNode, err := a.ensureGPUNode(ctx)
	if err != nil {
		return err
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := a.Client.Get(ctx, types.NamespacedName{Name: a.NodeName}, gpuNode); err != nil {
			return err
		}
		if collectErr != nil {
			gpuNode.Status = FailedStatus(gpuNode.Status, collectErr)
		} else {
			gpuNode.Status = BuildStatus(inv, gpuNode.Spec, gpuNode.Status, metav1.NewTime(a.now()))
		}
		return a.Client.Status().Update(ctx, gpuNode)
	})
	if err != nil {
		return fmt.Errorf("updating the status of GPUNode %s: %w", a.NodeName, err)
	}
	if collectErr != nil {
		return fmt.Errorf("collecting the GPU inventory: %w", collectErr)
	}

	if !a.ready.Swap(true) {
		a.Log.Info("GPU inventory published", "gpus", len(inv.GPUs), "driver", inv.DriverVersion)
	}
	return nil
}

// ensureGPUNode returns the GPUNode of the local node and creates it if it
// does not exist yet. The Node owns it, so it is deleted together with the Node.
func (a *Agent) ensureGPUNode(ctx context.Context) (*v1alpha1.GPUNode, error) {
	gpuNode := &v1alpha1.GPUNode{}
	err := a.Client.Get(ctx, types.NamespacedName{Name: a.NodeName}, gpuNode)
	if err == nil {
		return gpuNode, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading GPUNode %s: %w", a.NodeName, err)
	}

	node := &corev1.Node{}
	if err := a.Client.Get(ctx, types.NamespacedName{Name: a.NodeName}, node); err != nil {
		return nil, fmt.Errorf("reading Node %s: %w", a.NodeName, err)
	}
	gpuNode = &v1alpha1.GPUNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: a.NodeName,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Node",
				Name:       node.Name,
				UID:        node.UID,
			}},
		},
	}
	if err := a.Client.Create(ctx, gpuNode); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("creating GPUNode %s: %w", a.NodeName, err)
	}
	a.Log.Info("GPUNode created", "name", a.NodeName)
	return gpuNode, nil
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
