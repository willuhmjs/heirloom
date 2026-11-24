// Package controller reconciles BackupPolicy and Restore resources into Jobs.
package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	crtypes "sigs.k8s.io/controller-runtime/pkg/recorder"

	"github.com/willuhmjs/heirloom/api/v1"
	"github.com/willuhmjs/heirloom/internal/jobcfg"
)

// PolicyReconciler schedules the backup and verify runs of a BackupPolicy itself: it creates a Job when a
// schedule slot is due and reports the outcome of the runs in the policy status.
type PolicyReconciler struct {
	client.Client
	// Reader reads RBAC objects uncached (they are not in the label-filtered cache).
	Reader client.Reader
	Scheme *runtime.Scheme
	// Image is the runner image (HEIRLOOM_IMAGE).
	Image string
	// Now is overridable for tests.
	Now func() time.Time
	// Recorder emits Events on health changes; nil disables them.
	Recorder crtypes.EventRecorder
}

func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("backuppolicy").
		For(&v1.BackupPolicy{}).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(policyOfMover)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(policyOfMover)).
		Watches(&v1.BackupStore{}, handler.EnqueueRequestsFromMapFunc(r.policiesOfStore)).
		Watches(&v1.Restore{}, handler.EnqueueRequestsFromMapFunc(policyOfRestore)).
		Complete(r)
}

// policyOfMover maps a backup or verify Job/Pod to its policy.
func policyOfMover(_ context.Context, obj client.Object) []reconcile.Request {
	l := obj.GetLabels()
	if l[jobcfg.LabelPolicy] == "" || l[jobcfg.LabelRole] == string(jobcfg.ModeRestore) {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: l[jobcfg.LabelPolicy]}}}
}

// policyOfRestore maps a Restore to the policy it restores, which pauses scheduling while the Restore runs.
func policyOfRestore(_ context.Context, obj client.Object) []reconcile.Request {
	rs, ok := obj.(*v1.Restore)
	if !ok || rs.Spec.PolicyRef == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.PolicyRef}}}
}

func (r *PolicyReconciler) policiesOfStore(ctx context.Context, obj client.Object) []reconcile.Request {
	var list v1.BackupPolicyList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, p := range list.Items {
		if p.Spec.StoreRef == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&p)})
		}
	}
	return reqs
}

func (r *PolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1.BackupPolicy
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		if apierrors.IsNotFound(err) {
			forgetMetrics(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !p.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	before := p.Status.DeepCopy()
	healthBefore := meta.FindStatusCondition(before.Conditions, condHealthy)
	if err := r.reconcile(ctx, &p); err != nil {
		return ctrl.Result{}, err
	}
	recordMetrics(&p)
	healthEvents(r.Recorder, &p, healthBefore)
	// Come back when something is due on its own: the next schedule slot or the moment the policy turns stale.
	res := ctrl.Result{RequeueAfter: requeueIn(&p, r.now())}
	if equality.Semantic.DeepEqual(before, &p.Status) {
		return res, nil
	}
	if err := r.Status().Update(ctx, &p); err != nil {
		if apierrors.IsConflict(err) {
			// Our cached copy lags our own previous write. Not an error: look again.
			return ctrl.Result{RequeueAfter: time.Millisecond}, nil
		}
		return res, err
	}
	return res, nil
}

// reconcile starts due runs, then brings the in-memory status up to date; the caller persists the status.
func (r *PolicyReconciler) reconcile(ctx context.Context, p *v1.BackupPolicy) error {
	if err := validatePolicy(p); err != nil {
		setReady(p, false, "InvalidSpec", err.Error())
		return nil
	}
	var store v1.BackupStore
	if err := r.Get(ctx, types.NamespacedName{Name: p.Spec.StoreRef}, &store); err != nil {
		if apierrors.IsNotFound(err) {
			setReady(p, false, "StoreNotFound", fmt.Sprintf("BackupStore %q not found", p.Spec.StoreRef))
			return nil
		}
		return err
	}
	jobs, pods, err := r.observe(ctx, p)
	if err != nil {
		return err
	}
	if ready, msg, err := ensureStoreClaim(ctx, r.Client, r.Reader, r.Scheme, p, &store); err != nil {
		return err
	} else if !ready {
		setReady(p, false, "StoreClaimPending", msg)
		return nil
	}
	restoring, err := r.restoreActive(ctx, p)
	if err != nil {
		return err
	}
	now := r.now()
	switch {
	case p.Spec.Suspend:
		setReady(p, true, "Suspended", "scheduling is suspended")
	case restoring != "":
		setReady(p, true, "RestoreInProgress", fmt.Sprintf("paused while restore %q runs", restoring))
	default:
		if p.Spec.Source.Type == v1.SourceCommand {
			if err := r.ensureExecutorRBAC(ctx, p); err != nil {
				return err
			}
		}
		if err := r.triggerManual(ctx, p, &store, jobs); err != nil {
			return err
		}
		if err := r.startDue(ctx, p, &store, jobs, now); err != nil {
			return err
		}
		setReady(p, true, "Scheduled", "runs are scheduled by the controller")
	}
	deriveStatus(p, jobs, pods, now)
	return r.pruneHistory(ctx, jobs)
}

func (r *PolicyReconciler) observe(ctx context.Context, p *v1.BackupPolicy) ([]batchv1.Job, []corev1.Pod, error) {
	scope := []client.ListOption{client.InNamespace(p.Namespace), client.MatchingLabels{jobcfg.LabelPolicy: p.Name}}
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, scope...); err != nil {
		return nil, nil, err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, scope...); err != nil {
		return nil, nil, err
	}
	return jobs.Items, pods.Items, nil
}

func (r *PolicyReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
