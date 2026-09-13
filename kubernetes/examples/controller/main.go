// Copyright 2025 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	clientset "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/client/clientset/versioned"
	informers "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/client/informers/externalversions"
	listers "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/client/listers/sandbox/v1alpha1"
)

// Controller demonstrates how to use the generated clientset, informer, and lister
type Controller struct {
	// clientset is used to directly manipulate API objects
	clientset clientset.Interface

	// listers are used to read objects from local cache, avoiding frequent API Server access
	batchSandboxLister listers.BatchSandboxLister
	poolLister         listers.PoolLister

	// informer cache is used to check if objects are synced
	batchSandboxSynced cache.InformerSynced
	poolSynced         cache.InformerSynced
}

func NewController(
	clientset clientset.Interface,
	informerFactory informers.SharedInformerFactory,
) *Controller {
	// Get BatchSandbox and Pool informers
	batchSandboxInformer := informerFactory.Sandbox().V1alpha1().BatchSandboxes()
	poolInformer := informerFactory.Sandbox().V1alpha1().Pools()

	controller := &Controller{
		clientset:          clientset,
		batchSandboxLister: batchSandboxInformer.Lister(),
		poolLister:         poolInformer.Lister(),
		batchSandboxSynced: batchSandboxInformer.Informer().HasSynced,
		poolSynced:         poolInformer.Informer().HasSynced,
	}

	// Register event handlers
	batchSandboxInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			bs := obj.(*sandboxv1alpha1.BatchSandbox)
			klog.Infof("BatchSandbox added: %s/%s", bs.Namespace, bs.Name)
		},
		UpdateFunc: func(old, new interface{}) {
			bs := new.(*sandboxv1alpha1.BatchSandbox)
			klog.Infof("BatchSandbox updated: %s/%s", bs.Namespace, bs.Name)
		},
		DeleteFunc: func(obj interface{}) {
			bs := obj.(*sandboxv1alpha1.BatchSandbox)
			klog.Infof("BatchSandbox deleted: %s/%s", bs.Namespace, bs.Name)
		},
	})

	poolInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pool := obj.(*sandboxv1alpha1.Pool)
			klog.Infof("Pool added: %s/%s", pool.Namespace, pool.Name)
		},
		UpdateFunc: func(old, new interface{}) {
			pool := new.(*sandboxv1alpha1.Pool)
			klog.Infof("Pool updated: %s/%s", pool.Namespace, pool.Name)
		},
		DeleteFunc: func(obj interface{}) {
			pool := obj.(*sandboxv1alpha1.Pool)
			klog.Infof("Pool deleted: %s/%s", pool.Namespace, pool.Name)
		},
	})

	return controller
}

// DemonstrateClientsetUsage demonstrates how to use clientset for CRUD operations
func DemonstrateClientsetUsage(ctx context.Context, client clientset.Interface) {
	namespace := "default"

	klog.Info("========================================")
	klog.Info("Demonstrating Clientset Usage")
	klog.Info("========================================")

	// 1. Create Pool
	klog.Info("\n1. Creating Pool resource")
	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "example-pool",
			Namespace: namespace,
		},
		Spec: sandboxv1alpha1.PoolSpec{
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "nginx",
							Image: "nginx:latest",
						},
					},
				},
			},
			CapacitySpec: sandboxv1alpha1.CapacitySpec{
				PoolMin:   2,
				PoolMax:   10,
				BufferMin: 1,
				BufferMax: 5,
			},
		},
	}

	createdPool, err := client.SandboxV1alpha1().Pools(namespace).Create(ctx, pool, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			klog.Infof("Pool already exists: %s", pool.Name)
		} else {
			klog.Errorf("Failed to create Pool: %v", err)
		}
	} else {
		klog.Infof("Successfully created Pool: %s", createdPool.Name)
	}

	// 2. Get Pool
	klog.Info("\n2. Getting Pool resource")
	getPool, err := client.SandboxV1alpha1().Pools(namespace).Get(ctx, "example-pool", metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get Pool: %v", err)
	} else {
		klog.Infof("Successfully retrieved Pool: %s, PoolMin: %d, PoolMax: %d",
			getPool.Name, getPool.Spec.CapacitySpec.PoolMin, getPool.Spec.CapacitySpec.PoolMax)
	}

	// 3. List all Pools
	klog.Info("\n3. Listing all Pool resources")
	poolList, err := client.SandboxV1alpha1().Pools(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("Failed to list Pools: %v", err)
	} else {
		klog.Infof("Found %d Pool(s):", len(poolList.Items))
		for _, p := range poolList.Items {
			klog.Infof("  - %s (PoolMin: %d, PoolMax: %d)",
				p.Name, p.Spec.CapacitySpec.PoolMin, p.Spec.CapacitySpec.PoolMax)
		}
	}

	// 4. Update Pool
	klog.Info("\n4. Updating Pool resource")
	if getPool != nil {
		getPool.Spec.CapacitySpec.PoolMax = 20
		updatedPool, err := client.SandboxV1alpha1().Pools(namespace).Update(ctx, getPool, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("Failed to update Pool: %v", err)
		} else {
			klog.Infof("Successfully updated Pool: %s, new PoolMax: %d", updatedPool.Name, updatedPool.Spec.CapacitySpec.PoolMax)
		}
	}

	// 5. Create BatchSandbox
	klog.Info("\n5. Creating BatchSandbox resource")
	replicas := int32(3)
	batchSandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "example-batchsandbox",
			Namespace: namespace,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: &replicas,
			PoolRef:  "example-pool",
		},
	}

	createdBS, err := client.SandboxV1alpha1().BatchSandboxes(namespace).Create(ctx, batchSandbox, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			klog.Infof("BatchSandbox already exists: %s", batchSandbox.Name)
		} else {
			klog.Errorf("Failed to create BatchSandbox: %v", err)
		}
	} else {
		klog.Infof("Successfully created BatchSandbox: %s, Replicas: %d", createdBS.Name, *createdBS.Spec.Replicas)
	}

	// 6. Get BatchSandbox
	klog.Info("\n6. Getting BatchSandbox resource")
	getBS, err := client.SandboxV1alpha1().BatchSandboxes(namespace).Get(ctx, "example-batchsandbox", metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get BatchSandbox: %v", err)
	} else {
		klog.Infof("Successfully retrieved BatchSandbox: %s, Replicas: %d, PoolRef: %s",
			getBS.Name, *getBS.Spec.Replicas, getBS.Spec.PoolRef)
	}

	// 7. Update BatchSandbox
	klog.Info("\n7. Updating BatchSandbox resource")
	if getBS != nil {
		newReplicas := int32(5)
		getBS.Spec.Replicas = &newReplicas
		updatedBS, err := client.SandboxV1alpha1().BatchSandboxes(namespace).Update(ctx, getBS, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("Failed to update BatchSandbox: %v", err)
		} else {
			klog.Infof("Successfully updated BatchSandbox: %s, new Replicas: %d", updatedBS.Name, *updatedBS.Spec.Replicas)
		}
	}

	// 8. List all BatchSandboxes
	klog.Info("\n8. Listing all BatchSandbox resources")
	bsList, err := client.SandboxV1alpha1().BatchSandboxes(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("Failed to list BatchSandboxes: %v", err)
	} else {
		klog.Infof("Found %d BatchSandbox(es):", len(bsList.Items))
		for _, bs := range bsList.Items {
			klog.Infof("  - %s (Replicas: %d, PoolRef: %s)",
				bs.Name, *bs.Spec.Replicas, bs.Spec.PoolRef)
		}
	}

	// Wait for informer to process events
	klog.Info("\nWaiting 3 seconds for informer to process events...")
	time.Sleep(3 * time.Second)

	// 9. Delete BatchSandbox
	klog.Info("\n9. Deleting BatchSandbox resource")
	err = client.SandboxV1alpha1().BatchSandboxes(namespace).Delete(ctx, "example-batchsandbox", metav1.DeleteOptions{})
	if err != nil {
		klog.Errorf("Failed to delete BatchSandbox: %v", err)
	} else {
		klog.Infof("Successfully deleted BatchSandbox: example-batchsandbox")
	}

	// 10. Delete Pool
	klog.Info("\n10. Deleting Pool resource")
	err = client.SandboxV1alpha1().Pools(namespace).Delete(ctx, "example-pool", metav1.DeleteOptions{})
	if err != nil {
		klog.Errorf("Failed to delete Pool: %v", err)
	} else {
		klog.Infof("Successfully deleted Pool: example-pool")
	}
}

// DemonstrateListerUsage demonstrates how to use lister to read objects from cache
func DemonstrateListerUsage(
	batchSandboxLister listers.BatchSandboxLister,
	poolLister listers.PoolLister,
) {
	klog.Info("\n========================================")
	klog.Info("Demonstrating Lister Usage (reading from local cache)")
	klog.Info("========================================")

	namespace := "default"

	// 1. Use lister to get a specific Pool
	klog.Info("\n1. Using Lister to get Pool")
	pool, err := poolLister.Pools(namespace).Get("example-pool")
	if err != nil {
		if errors.IsNotFound(err) {
			klog.Info("Pool not found (may have been deleted)")
		} else {
			klog.Errorf("Lister failed to get Pool: %v", err)
		}
	} else {
		klog.Infof("Retrieved Pool from cache: %s, PoolMax: %d", pool.Name, pool.Spec.CapacitySpec.PoolMax)
	}

	// 2. Use lister to list all Pools
	klog.Info("\n2. Using Lister to list all Pools")
	pools, err := poolLister.Pools(namespace).List(labels.Everything())
	if err != nil {
		klog.Errorf("Lister failed to list Pools: %v", err)
	} else {
		klog.Infof("Found %d Pool(s) from cache:", len(pools))
		for _, p := range pools {
			klog.Infof("  - %s", p.Name)
		}
	}

	// 3. Use lister to get a specific BatchSandbox
	klog.Info("\n3. Using Lister to get BatchSandbox")
	bs, err := batchSandboxLister.BatchSandboxes(namespace).Get("example-batchsandbox")
	if err != nil {
		if errors.IsNotFound(err) {
			klog.Info("BatchSandbox not found (may have been deleted)")
		} else {
			klog.Errorf("Lister failed to get BatchSandbox: %v", err)
		}
	} else {
		klog.Infof("Retrieved BatchSandbox from cache: %s, Replicas: %d", bs.Name, *bs.Spec.Replicas)
	}

	// 4. Use lister to list all BatchSandboxes
	klog.Info("\n4. Using Lister to list all BatchSandboxes")
	batchSandboxes, err := batchSandboxLister.BatchSandboxes(namespace).List(labels.Everything())
	if err != nil {
		klog.Errorf("Lister failed to list BatchSandboxes: %v", err)
	} else {
		klog.Infof("Found %d BatchSandbox(es) from cache:", len(batchSandboxes))
		for _, bs := range batchSandboxes {
			klog.Infof("  - %s (Replicas: %d)", bs.Name, *bs.Spec.Replicas)
		}
	}
}

func main() {
	var kubeconfig string
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig file")
	flag.Parse()

	// Build configuration
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to build config: %v", err)
	}

	// Create clientset
	client, err := clientset.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create clientset: %v", err)
	}

	// Create informer factory
	informerFactory := informers.NewSharedInformerFactory(client, time.Second*30)

	// Create controller
	controller := NewController(client, informerFactory)

	// Start informers
	ctx := context.Background()
	informerFactory.Start(ctx.Done())

	// Wait for cache sync
	klog.Info("Waiting for informer cache sync...")
	if ok := cache.WaitForCacheSync(ctx.Done(), controller.batchSandboxSynced, controller.poolSynced); !ok {
		klog.Fatal("Failed to sync cache")
	}
	klog.Info("Informer cache synced successfully")

	// Demonstrate clientset usage
	DemonstrateClientsetUsage(ctx, client)

	// Demonstrate lister usage
	DemonstrateListerUsage(controller.batchSandboxLister, controller.poolLister)

	klog.Info("\n========================================")
	klog.Info("Demonstration completed!")
	klog.Info("========================================")
}
