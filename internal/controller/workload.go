package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/willuhmjs/heirloom/api/v1"
)

const (
	// annotationFluxReconcile stops Flux from scaling a workload back up while it is being restored.
	annotationFluxReconcile = "kustomize.toolkit.fluxcd.io/reconcile"
	fluxDisabled            = "disabled"
	// annotationRestoredBy marks workloads on which a Restore (by name) added the Flux annotation,
	// so only that annotation is removed again.
	annotationRestoredBy = "heirloom.io/restore"
)

// workload gives uniform access to the scaling fields of a Deployment or StatefulSet.
type workload struct {
	client.Object
	spec     **int32
	observed int32
	selector *metav1.LabelSelector
}

func (w workload) replicas() int32 {
	if *w.spec == nil {
		return 1
	}
	return **w.spec
}

func (w workload) setReplicas(n int32) { *w.spec = &n }

func getWorkload(ctx context.Context, c client.Reader, namespace string, ref v1.WorkloadRef) (workload, error) {
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	switch ref.Kind {
	case "Deployment":
		var d appsv1.Deployment
		err := c.Get(ctx, key, &d)
		return workload{&d, &d.Spec.Replicas, d.Status.Replicas, d.Spec.Selector}, err
	case "StatefulSet":
		var s appsv1.StatefulSet
		err := c.Get(ctx, key, &s)
		return workload{&s, &s.Spec.Replicas, s.Status.Replicas, s.Spec.Selector}, err
	}
	return workload{}, fmt.Errorf("unsupported workload kind %q", ref.Kind)
}

// scaleDown sets replicas to 0 and pauses Flux; it reports whether the object changed.
func (w workload) scaleDown(restore string) bool {
	changed := false
	if w.replicas() != 0 {
		w.setReplicas(0)
		changed = true
	}
	ann := w.GetAnnotations()
	if ann[annotationFluxReconcile] != fluxDisabled {
		if ann == nil {
			ann = map[string]string{}
		}
		ann[annotationFluxReconcile] = fluxDisabled
		ann[annotationRestoredBy] = restore
		w.SetAnnotations(ann)
		changed = true
	}
	return changed
}

// scaleUp restores replicas (only if still 0) and removes the annotations this restore added.
func (w workload) scaleUp(restore string, original int32) bool {
	changed := false
	if w.replicas() == 0 {
		w.setReplicas(original)
		changed = true
	}
	ann := w.GetAnnotations()
	if ann[annotationRestoredBy] == restore {
		delete(ann, annotationFluxReconcile)
		delete(ann, annotationRestoredBy)
		w.SetAnnotations(ann)
		changed = true
	}
	return changed
}

// stopped reports whether no pods of the workload remain.
func (w workload) stopped(ctx context.Context, c client.Reader) (bool, error) {
	if w.observed != 0 {
		return false, nil
	}
	if w.selector == nil {
		return true, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(w.selector)
	if err != nil {
		return false, err
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(w.GetNamespace()), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return false, err
	}
	return len(pods.Items) == 0, nil
}
