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

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	clientset "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/client/clientset/versioned"
	informers "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/client/informers/externalversions"
	listers "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/client/listers/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

type BatchSandboxProvider struct {
	informerFactory informers.SharedInformerFactory
	lister          listers.BatchSandboxLister
	informer        cache.SharedIndexInformer
	informerSynced  cache.InformerSynced
}

func NewBatchSandboxProvider(
	config *rest.Config,
	resyncPeriod time.Duration,
) *BatchSandboxProvider {
	clientset, err := clientset.NewForConfig(config)
	if err != nil {
		panic(fmt.Sprintf("failed to create sandbox clientset: %v", err))
	}

	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
		informers.WithNamespace(metav1.NamespaceAll),
	)

	batchSandboxInformer := informerFactory.Sandbox().V1alpha1().BatchSandboxes()
	if err := batchSandboxInformer.Informer().AddIndexers(cache.Indexers{
		sandboxNameIndex: func(obj any) ([]string, error) {
			bs, ok := obj.(*sandboxv1alpha1.BatchSandbox)
			if !ok {
				return []string{}, nil
			}
			return []string{bs.Name}, nil
		},
	}); err != nil {
		panic(fmt.Sprintf("failed to add BatchSandbox indexer: %v", err))
	}

	return &BatchSandboxProvider{
		informerFactory: informerFactory,
		lister:          batchSandboxInformer.Lister(),
		informer:        batchSandboxInformer.Informer(),
		informerSynced:  batchSandboxInformer.Informer().HasSynced,
	}
}

func (p *BatchSandboxProvider) Start(ctx context.Context) error {
	p.informerFactory.Start(ctx.Done())

	// Wait for cache sync
	if !cache.WaitForCacheSync(ctx.Done(), p.informerSynced) {
		return errors.New("failed to sync BatchSandbox informer cache")
	}

	return nil
}

func (p *BatchSandboxProvider) findBatchSandbox(sandboxId string) (*sandboxv1alpha1.BatchSandbox, error) {
	matches := make([]string, 0, 1)
	indexed := []any{}
	needScanFallback := p.informer == nil
	if p.informer != nil {
		var err error
		indexed, err = p.informer.GetIndexer().ByIndex(sandboxNameIndex, sandboxId)
		if err != nil {
			needScanFallback = true
		}
	}

	if needScanFallback {
		all, err := p.lister.List(labels.Everything())
		if err != nil {
			return nil, fmt.Errorf("failed to list BatchSandboxes: %w", err)
		}
		indexed = make([]any, 0, len(all))
		for _, bs := range all {
			if bs.Name == sandboxId {
				indexed = append(indexed, bs)
			}
		}
	}
	var selected *sandboxv1alpha1.BatchSandbox
	for _, item := range indexed {
		bs, ok := item.(*sandboxv1alpha1.BatchSandbox)
		if !ok {
			continue
		}
		matches = append(matches, fmt.Sprintf("%s/%s", bs.Namespace, bs.Name))
		if selected == nil {
			selected = bs
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, sandboxId)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("ambiguous sandbox id %q found in multiple namespaces: %v", sandboxId, matches)
	}
	return selected, nil
}

// GetEndpoint retrieves the endpoint IP for a BatchSandbox
func (p *BatchSandboxProvider) GetEndpoint(sandboxId string) (*EndpointInfo, error) {
	batchSandbox, err := p.findBatchSandbox(sandboxId)
	if err != nil {
		return nil, err
	}
	accessToken := ""
	if batchSandbox.Annotations != nil {
		accessToken = strings.TrimSpace(batchSandbox.Annotations[AnnotationAccessToken])
	}

	// Check if BatchSandbox is ready
	if batchSandbox.Status.Ready < 1 {
		return nil, fmt.Errorf("%w: %s/%s (ready: %d/%d)",
			ErrSandboxNotReady, batchSandbox.Namespace, sandboxId, batchSandbox.Status.Ready, batchSandbox.Status.Replicas)
	}

	// Get endpoints from BatchSandbox using kubernetes utils
	endpoints, err := utils.GetEndpoints(batchSandbox)
	if err != nil {
		return nil, fmt.Errorf("%w: %s/%s: %w", ErrSandboxNotReady, batchSandbox.Namespace, sandboxId, err)
	}

	// Return the first available endpoint
	return &EndpointInfo{
		Endpoint:          endpoints[0],
		SecureAccessToken: accessToken,
	}, nil
}

var _ Provider = (*BatchSandboxProvider)(nil)
