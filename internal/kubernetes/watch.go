/*
Copyright 2018 Planet Labs Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing permissions
and limitations under the License.
*/

package kubernetes

import (
	"context"
	"time"

	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// NodeWatch is a cache of node resources that notifies registered
// handlers when its contents change.
type NodeWatch struct {
	cache.SharedInformer
}

// NewNodeWatch creates a watch on node resources. Nodes are cached and the
// provided ResourceEventHandlers are called when the cache changes.
func NewNodeWatch(c kubernetes.Interface, rs ...cache.ResourceEventHandler) (*NodeWatch, error) {
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o meta.ListOptions) (runtime.Object, error) {
			return c.CoreV1().Nodes().List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o meta.ListOptions) (watch.Interface, error) {
			return c.CoreV1().Nodes().Watch(ctx, o)
		},
	}
	i := cache.NewSharedInformer(lw, &core.Node{}, 30*time.Minute)
	for _, r := range rs {
		if _, err := i.AddEventHandler(r); err != nil {
			return nil, errors.Wrap(err, "failed to add event handler")
		}
	}
	return &NodeWatch{i}, nil
}

// Get retrieves a node by name. Returns an error if the node does not exist.
func (w *NodeWatch) Get(name string) (*core.Node, error) {
	o, exists, err := w.GetStore().GetByKey(name)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot get node %s", name)
	}
	if !exists {
		return nil, errors.Errorf("node %s does not exist", name)
	}
	return o.(*core.Node), nil
}
