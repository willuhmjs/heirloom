package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	crtypes "sigs.k8s.io/controller-runtime/pkg/recorder"

	"github.com/willuhmjs/backshots/api/v1alpha1"
	"github.com/willuhmjs/backshots/internal/jobcfg"
	"github.com/willuhmjs/backshots/internal/snapshot"
)

const (
	phasePending     v1alpha1.RestorePhase = "Pending"
	phaseScalingDown v1alpha1.RestorePhase = "ScalingDown"
	phaseRunning     v1alpha1.RestorePhase = "Running"
	phaseScalingUp   v1alpha1.RestorePhase = "ScalingUp"
	phaseSucceeded   v1alpha1.RestorePhase = "Succeeded"
	phaseFailed      v1alpha1.RestorePhase = "Failed"

	scaleDownPoll    = 5 * time.Second
	scaleDownTimeout = 5 * time.Minute
	queuePoll        = 10 * time.Second

	// finalizerName makes deleting a Restore mid-flight put the workloads it scaled down back first.
	finalizerName = "backshots.io/restore"
	// maxRestoreNameLen keeps "bs-restore-<name>" (the Job name, and so its pods' job-name label) under 63 characters.
	maxRestoreNameLen = 40
)

// RestoreReconciler runs a Restore as a phase machine. All progress is derived from status.phase,
// status.scaled and the state of the restore Job, so a controller restart resumes where it left off.
type RestoreReconciler struct {
	client.Client
	// Reader reads workloads and their pods uncached (they are not in the label-filtered cache).
	Reader client.Reader
	Scheme *runtime.Scheme
	Image  string
	Now    func() time.Time
	// Recorder emits Events as the restore progresses; nil disables them.
	Recorder crtypes.EventRecorder
}

func (r *RestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("restore").
		For(&v1alpha1.Restore{}).
		Owns(&batchv1.Job{}).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(r.restoresOfMover)).
		Complete(r)
}

// restoresOfMover maps a backup or verify Job to the Restores of its policy, so a Restore waiting for
// the Job to finish starts the moment it does.
func (r *RestoreReconciler) restoresOfMover(ctx context.Context, obj client.Object) []reconcile.Request {
	l := obj.GetLabels()
	policy := l[jobcfg.LabelPolicy]
	if policy == "" || l[jobcfg.LabelRole] == string(jobcfg.ModeRestore) {
		return nil
	}
	var list v1alpha1.RestoreList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.PolicyRef == policy {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

func restoreJobName(restore string) string { return "bs-restore-" + restore }

func (r *RestoreReconciler) now() metav1.Time {
	if r.Now != nil {
		return metav1.NewTime(r.Now())
	}
	return metav1.Now()
}

func (r *RestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rs v1alpha1.Restore
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rs.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &rs)
	}
	if phase := rs.Status.Phase; phase == phaseSucceeded || phase == phaseFailed {
		return ctrl.Result{}, nil
	}
	// Registered before the first workload is touched, so a delete at any point can undo the scale-down.
	if len(rs.Spec.ScaleDown) > 0 && controllerutil.AddFinalizer(&rs, finalizerName) {
		if err := r.Update(ctx, &rs); err != nil {
			return ctrl.Result{}, err
		}
	}
	before := rs.Status.DeepCopy()
	res, err := r.step(ctx, &rs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if rs.Status.Phase != before.Phase {
		phaseEvent(r.Recorder, &rs)
	}
	if !equality.Semantic.DeepEqual(before, &rs.Status) {
		if err := r.Status().Update(ctx, &rs); err != nil {
			return ctrl.Result{}, err
		}
	}
	return res, nil
}

func (r *RestoreReconciler) step(ctx context.Context, rs *v1alpha1.Restore) (ctrl.Result, error) {
	switch rs.Status.Phase {
	case "":
		start := r.now()
		rs.Status.StartTime = &start
		rs.Status.Phase = phasePending
		return ctrl.Result{}, nil
	case phasePending:
		return r.pending(ctx, rs)
	case phaseScalingDown:
		return r.scalingDown(ctx, rs)
	case phaseRunning:
		return ctrl.Result{}, r.running(ctx, rs)
	case phaseScalingUp:
		return ctrl.Result{}, r.scalingUp(ctx, rs)
	}
	return ctrl.Result{}, nil
}

// finalize runs when a Restore is deleted: workloads it scaled down are put back before it goes away.
func (r *RestoreReconciler) finalize(ctx context.Context, rs *v1alpha1.Restore) error {
	if !controllerutil.ContainsFinalizer(rs, finalizerName) {
		return nil
	}
	for _, sc := range rs.Status.Scaled {
		err := r.updateWorkload(ctx, rs.Namespace, sc.WorkloadRef, func(w workload) bool { return w.scaleUp(rs.Name, sc.Replicas) })
		if err != nil {
			return err
		}
	}
	if err := r.restoreHPAs(ctx, rs); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(rs, finalizerName)
	return r.Update(ctx, rs)
}

// failBeforeRestore ends a Restore whose Job never ran. The data is untouched, so workloads and
// autoscalers always come back.
func (r *RestoreReconciler) failBeforeRestore(rs *v1alpha1.Restore, msg string) {
	rs.Status.Message = msg
	if len(rs.Status.Scaled) > 0 || len(rs.Status.ScaledHPAs) > 0 {
		rs.Status.Phase = phaseScalingUp
		return
	}
	r.finish(rs, phaseFailed)
}

// fail records the reason and either brings workloads back first (scaleUpOnFailure) or ends the Restore.
// It is for failures of the restore itself; the workloads stay down by default because the data may be half written.
func (r *RestoreReconciler) fail(rs *v1alpha1.Restore, msg string) {
	rs.Status.Message = msg
	if rs.Spec.ScaleUpOnFailure && len(rs.Status.Scaled) > 0 {
		rs.Status.Phase = phaseScalingUp
		return
	}
	r.finish(rs, phaseFailed)
}

func (r *RestoreReconciler) finish(rs *v1alpha1.Restore, phase v1alpha1.RestorePhase) {
	done := r.now()
	rs.Status.Phase = phase
	rs.Status.CompletionTime = &done
}

func (r *RestoreReconciler) load(ctx context.Context, rs *v1alpha1.Restore) (*v1alpha1.BackupPolicy, *v1alpha1.BackupStore, error) {
	var p v1alpha1.BackupPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.PolicyRef}, &p); err != nil {
		return nil, nil, fmt.Errorf("BackupPolicy %q: %w", rs.Spec.PolicyRef, err)
	}
	var store v1alpha1.BackupStore
	if err := r.Get(ctx, types.NamespacedName{Name: p.Spec.StoreRef}, &store); err != nil {
		return nil, nil, fmt.Errorf("BackupStore %q: %w", p.Spec.StoreRef, err)
	}
	return &p, &store, nil
}

func (r *RestoreReconciler) pending(ctx context.Context, rs *v1alpha1.Restore) (ctrl.Result, error) {
	p, _, err := r.load(ctx, rs)
	if apierrors.IsNotFound(err) {
		r.failBeforeRestore(rs, err.Error())
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	// Everything that can be checked is checked before any workload is stopped: a typo must not cost an outage.
	if err := validateRestore(rs, p); err != nil {
		r.failBeforeRestore(rs, err.Error())
		return ctrl.Result{}, nil
	}
	if missing, err := r.missingClaim(ctx, rs, p); err != nil {
		return ctrl.Result{}, err
	} else if missing != "" {
		r.failBeforeRestore(rs, fmt.Sprintf("PersistentVolumeClaim %q not found", missing))
		return ctrl.Result{}, nil
	}
	if blocker, err := r.blockedBy(ctx, rs); err != nil {
		return ctrl.Result{}, err
	} else if blocker != "" {
		rs.Status.Message = fmt.Sprintf("waiting for restore %q", blocker)
		return ctrl.Result{RequeueAfter: queuePoll}, nil
	}
	// The other half of the repository mutex: the policy pauses new runs once it sees this Restore,
	// but a backup or verify that was already running would otherwise read and rewrite (forget+prune)
	// the same repository under the restore. Wait for it to finish.
	if mover, err := r.activeMover(ctx, rs); err != nil {
		return ctrl.Result{}, err
	} else if mover != "" {
		rs.Status.Message = fmt.Sprintf("waiting for %q to finish before restoring", mover)
		return ctrl.Result{RequeueAfter: queuePoll}, nil
	}
	rs.Status.Message = ""
	if len(rs.Spec.ScaleDown) > 0 {
		since := r.now()
		rs.Status.ScalingDownSince = &since
		rs.Status.Phase = phaseScalingDown
	} else {
		rs.Status.Phase = phaseRunning
	}
	return ctrl.Result{}, nil
}

// missingClaim returns the name of the PVC the restore would write to if it does not exist.
func (r *RestoreReconciler) missingClaim(ctx context.Context, rs *v1alpha1.Restore, p *v1alpha1.BackupPolicy) (string, error) {
	claim := rs.Spec.TargetPVC
	if claim == "" {
		switch p.Spec.Source.Type {
		case v1alpha1.SourceFiles:
			claim = p.Spec.Source.Files.PVC
		case v1alpha1.SourceSQLite:
			claim = p.Spec.Source.SQLite.PVC
		}
	}
	if claim == "" {
		return "", nil
	}
	var pvc corev1.PersistentVolumeClaim
	err := r.Reader.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: claim}, &pvc)
	if apierrors.IsNotFound(err) {
		return claim, nil
	}
	return "", err
}

// blockedBy returns the Restore of the same policy this one has to wait for, or "". Restores of one policy run
// one at a time, oldest first, so two of them never scale each other's workloads up under a running mover.
func (r *RestoreReconciler) blockedBy(ctx context.Context, rs *v1alpha1.Restore) (string, error) {
	var list v1alpha1.RestoreList
	if err := r.List(ctx, &list, client.InNamespace(rs.Namespace)); err != nil {
		return "", err
	}
	for i := range list.Items {
		o := &list.Items[i]
		if o.Name == rs.Name || o.Spec.PolicyRef != rs.Spec.PolicyRef {
			continue
		}
		switch o.Status.Phase {
		case phaseSucceeded, phaseFailed:
			continue
		case phaseScalingDown, phaseRunning, phaseScalingUp:
			return o.Name, nil
		}
		// Pending, or not looked at yet: the older one goes first, and the name breaks a tie.
		if o.CreationTimestamp.Before(&rs.CreationTimestamp) || (o.CreationTimestamp.Equal(&rs.CreationTimestamp) && o.Name < rs.Name) {
			return o.Name, nil
		}
	}
	return "", nil
}

// activeMover returns the name of a running backup or verify Job of the policy, or "". A Restore and
// those runs must never share the restic repository at the same time (see pending).
func (r *RestoreReconciler) activeMover(ctx context.Context, rs *v1alpha1.Restore) (string, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(rs.Namespace), client.MatchingLabels{jobcfg.LabelPolicy: rs.Spec.PolicyRef}); err != nil {
		return "", err
	}
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if j.Labels[jobcfg.LabelRole] == string(jobcfg.ModeRestore) || !j.DeletionTimestamp.IsZero() {
			continue
		}
		if done, _ := jobState(j); !done {
			return j.Name, nil
		}
	}
	return "", nil
}

// validateRestore checks that the restore options make sense for the policy's source type.
func validateRestore(rs *v1alpha1.Restore, p *v1alpha1.BackupPolicy) error {
	if err := validateSource(p.Spec.Source); err != nil {
		return fmt.Errorf("policy %q: %v", p.Name, err)
	}
	if err := validateRestoreInputs(rs); err != nil {
		return err
	}
	// from must name something the mover can resolve: "latest" or a restic snapshot id. Reject here, not
	// after the workloads have been stopped.
	if from := rs.Spec.From; from != "" && from != "latest" && !snapshot.ResticID(from) {
		return fmt.Errorf("from %q is neither \"latest\" nor a restic snapshot id (hex)", from)
	}
	s := rs.Spec
	// The mover passes the backup's exclude patterns to a restore with delete, so that excluded paths are
	// never deleted. restic refuses exclude together with include, so paths + delete cannot keep that
	// guarantee; reject it here rather than surprise the user mid-restore.
	if s.Delete && len(s.Paths) > 0 {
		return fmt.Errorf("restores support delete or paths, not both (excluded paths cannot be protected while restricting includes)")
	}
	switch p.Spec.Source.Type {
	case v1alpha1.SourceFiles:
		if len(s.Databases) > 0 {
			return fmt.Errorf("databases is only valid for postgres sources")
		}
	case v1alpha1.SourceSQLite:
		if len(s.Databases) > 0 || len(s.Paths) > 0 || s.Delete {
			return fmt.Errorf("paths, databases and delete are not valid for sqlite sources")
		}
	case v1alpha1.SourcePostgres:
		if len(s.Paths) > 0 || s.Delete || s.TargetPVC != "" {
			return fmt.Errorf("paths, delete and targetPVC are not valid for postgres sources")
		}
	case v1alpha1.SourceCommand:
		if len(s.Databases) > 0 || s.Delete {
			return fmt.Errorf("databases and delete are not valid for command sources")
		}
		// There is no generic loader for arbitrary dumps: a command restore writes them to a PVC the
		// caller names, and the app's own import tool loads them from there.
		if s.TargetPVC == "" {
			return fmt.Errorf("restore.targetPVC is required for command sources (the PVC the dumps are written to)")
		}
	}
	return nil
}

// validateRestoreInputs rejects values the mover would only reject after the workloads were already stopped.
// (from is checked in validateRestore, next to the other policy-dependent rules.)
func validateRestoreInputs(rs *v1alpha1.Restore) error {
	if len(rs.Name) > maxRestoreNameLen {
		return fmt.Errorf("restore name is longer than %d characters", maxRestoreNameLen)
	}
	for _, path := range rs.Spec.Paths {
		trimmed := strings.TrimLeft(path, "/")
		if trimmed == "" {
			return fmt.Errorf("paths contains an empty path")
		}
		for _, part := range strings.Split(trimmed, "/") {
			if part == ".." {
				return fmt.Errorf("path %q must not contain '..'", path)
			}
		}
	}
	for _, db := range rs.Spec.Databases {
		if db == "" || strings.ContainsAny(db, "/\x00") {
			return fmt.Errorf("invalid database name %q", db)
		}
	}
	return nil
}

func (r *RestoreReconciler) scalingDown(ctx context.Context, rs *v1alpha1.Restore) (ctrl.Result, error) {
	refs := rs.Spec.ScaleDown
	// Persist original replica counts before touching anything so a crash cannot lose them.
	if recorded, err := r.recordReplicas(ctx, rs); err != nil || recorded {
		return ctrl.Result{}, err
	}
	// Then pause every autoscaler aimed at those workloads, recording its bounds first, so HPA (or
	// any autoscaler writing spec.replicas) cannot scale the workload back up under the restore.
	if pending, err := r.quiesceHPAs(ctx, rs); err != nil || pending {
		return ctrl.Result{}, err
	}
	for _, sc := range rs.Status.Scaled {
		if err := r.updateWorkload(ctx, rs.Namespace, sc.WorkloadRef, func(w workload) bool { return w.scaleDown(rs.Name) }); err != nil {
			return ctrl.Result{}, err
		}
	}
	for _, ref := range refs {
		w, err := getWorkload(ctx, r.Reader, rs.Namespace, ref)
		if err != nil {
			return ctrl.Result{}, err
		}
		stopped, err := w.stopped(ctx, r.Reader)
		if err == nil && !stopped && rs.Status.ScalingDownSince != nil && r.now().Sub(rs.Status.ScalingDownSince.Time) > scaleDownTimeout {
			r.failBeforeRestore(rs, fmt.Sprintf("%s %q did not stop within %s; nothing was restored", ref.Kind, ref.Name, scaleDownTimeout))
			return ctrl.Result{}, nil
		}
		if err != nil || !stopped {
			return ctrl.Result{RequeueAfter: scaleDownPoll}, err
		}
	}
	rs.Status.Phase = phaseRunning
	return ctrl.Result{}, nil
}

// recordReplicas appends running workloads to status.scaled; it reports whether it changed anything.
func (r *RestoreReconciler) recordReplicas(ctx context.Context, rs *v1alpha1.Restore) (bool, error) {
	recorded := false
	for _, ref := range rs.Spec.ScaleDown {
		if isScaled(rs, ref) {
			continue
		}
		w, err := getWorkload(ctx, r.Reader, rs.Namespace, ref)
		if err != nil {
			if apierrors.IsNotFound(err) {
				r.failBeforeRestore(rs, fmt.Sprintf("%s %q not found", ref.Kind, ref.Name))
				return true, nil
			}
			return false, err
		}
		if n := w.replicas(); n > 0 {
			rs.Status.Scaled = append(rs.Status.Scaled, v1alpha1.ScaledWorkload{WorkloadRef: ref, Replicas: n})
			recorded = true
		}
	}
	if recorded {
		return true, r.Status().Update(ctx, rs)
	}
	return false, nil
}

// isScaled reports whether ref's replica count is already recorded.
func isScaled(rs *v1alpha1.Restore, ref v1alpha1.WorkloadRef) bool {
	for _, sc := range rs.Status.Scaled {
		if sc.WorkloadRef == ref {
			return true
		}
	}
	return false
}

// isHPAPaused reports whether the autoscaler's bounds were already recorded for this restore.
func isHPAPaused(rs *v1alpha1.Restore, name string) bool {
	for _, h := range rs.Status.ScaledHPAs {
		if h.Name == name {
			return true
		}
	}
	return false
}

// quiesceHPAs pauses every HorizontalPodAutoscaler targeting a scaleDown workload. Bounds are
// recorded into status one at a time (persisted before the next is touched) and then applied, so a
// crash mid-way still knows everything it has to put back. On clusters that reject replica bounds
// of 0 (before 1.30) the autoscaler is left running with a recorded warning instead of failing the
// restore — the pre-quiesce behaviour, and better than an outage. It reports whether the caller
// should requeue so status lands first.
func (r *RestoreReconciler) quiesceHPAs(ctx context.Context, rs *v1alpha1.Restore) (bool, error) {
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := r.Reader.List(ctx, &list, client.InNamespace(rs.Namespace)); err != nil {
		return false, err
	}
	for _, ref := range rs.Spec.ScaleDown {
		for i := range list.Items {
			h := &list.Items[i]
			if !h.DeletionTimestamp.IsZero() || h.Spec.ScaleTargetRef.Name != ref.Name || !strings.EqualFold(h.Spec.ScaleTargetRef.Kind, ref.Kind) {
				continue
			}
			if isHPAPaused(rs, h.Name) {
				continue
			}
			min := int32(1)
			if h.Spec.MinReplicas != nil {
				min = *h.Spec.MinReplicas
			}
			rs.Status.ScaledHPAs = append(rs.Status.ScaledHPAs, v1alpha1.ScaledHPA{Name: h.Name, MinReplicas: min, MaxReplicas: h.Spec.MaxReplicas})
			return true, nil // persist the record before touching the autoscaler
		}
	}
	var warnings []string
	for _, rec := range rs.Status.ScaledHPAs {
		var h autoscalingv2.HorizontalPodAutoscaler
		if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rec.Name}, &h); err != nil {
			if apierrors.IsNotFound(err) {
				continue // deleted since recorded: nothing to pause or restore
			}
			return false, err
		}
		if h.Spec.MaxReplicas == 0 && h.Spec.MinReplicas != nil && *h.Spec.MinReplicas == 0 {
			continue
		}
		original := h.DeepCopy()
		h.Spec.MinReplicas = ptr.To[int32](0)
		h.Spec.MaxReplicas = 0
		if err := r.Patch(ctx, &h, client.MergeFrom(original)); err != nil {
			if apierrors.IsInvalid(err) {
				warnings = append(warnings, fmt.Sprintf("could not pause autoscaler %q (its API rejects 0 replicas): autoscaling may fight the restore", rec.Name))
				continue
			}
			return false, client.IgnoreNotFound(err)
		}
	}
	if len(warnings) > 0 {
		rs.Status.Message = strings.Join(warnings, "; ")
	}
	return false, nil
}

// restoreHPAs puts every recorded autoscaler's bounds back.
func (r *RestoreReconciler) restoreHPAs(ctx context.Context, rs *v1alpha1.Restore) error {
	for _, rec := range rs.Status.ScaledHPAs {
		var h autoscalingv2.HorizontalPodAutoscaler
		if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rec.Name}, &h); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		original := h.DeepCopy()
		if h.Spec.MaxReplicas == rec.MaxReplicas && h.Spec.MinReplicas != nil && *h.Spec.MinReplicas == rec.MinReplicas {
			continue
		}
		h.Spec.MinReplicas = ptr.To(rec.MinReplicas)
		h.Spec.MaxReplicas = rec.MaxReplicas
		if err := r.Patch(ctx, &h, client.MergeFrom(original)); err != nil {
			return client.IgnoreNotFound(err)
		}
	}
	return nil
}

// updateWorkload applies mutate to the workload and patches it when mutate reports a change.
func (r *RestoreReconciler) updateWorkload(ctx context.Context, namespace string, ref v1alpha1.WorkloadRef, mutate func(workload) bool) error {
	w, err := getWorkload(ctx, r.Reader, namespace, ref)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	original := w.DeepCopyObject().(client.Object)
	if !mutate(w) {
		return nil
	}
	// A merge patch of only what changed (replicas, annotations): no resourceVersion conflicts with the workload's
	// own controller, and no risk of writing back fields this client does not know about.
	return r.Patch(ctx, w.Object, client.MergeFrom(original)) //nolint:staticcheck // w.Object is the registered concrete type, not the wrapper w
}

func (r *RestoreReconciler) running(ctx context.Context, rs *v1alpha1.Restore) error {
	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: restoreJobName(rs.Name)}, &job)
	if apierrors.IsNotFound(err) {
		return r.createJob(ctx, rs)
	}
	if err != nil {
		return err
	}
	done, ok := jobState(&job)
	if !done {
		return nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(rs.Namespace), client.MatchingLabels{jobcfg.LabelPolicy: rs.Spec.PolicyRef}); err != nil {
		return err
	}
	res := resultFor(&job, pods.Items)
	if !ok {
		r.fail(rs, res.Message)
		return nil
	}
	rs.Status.Snapshot = res.Snapshot
	rs.Status.Message = ""
	if len(rs.Status.Scaled) > 0 {
		rs.Status.Phase = phaseScalingUp
	} else {
		r.finish(rs, phaseSucceeded)
	}
	return nil
}

func (r *RestoreReconciler) createJob(ctx context.Context, rs *v1alpha1.Restore) error {
	p, store, err := r.load(ctx, rs)
	if err != nil {
		return err
	}
	// A store with mountOptions is mounted through a provisioned claim; make sure it exists before
	// the mover pod would wait on it forever in Pending.
	if _, msg, err := ensureStoreClaim(ctx, r.Client, r.Reader, r.Scheme, p, store); err != nil {
		return err
	} else if msg != "" {
		return fmt.Errorf("store claim: %s", msg)
	}
	job, err := r.restoreJob(rs, p, store)
	if err != nil {
		r.fail(rs, err.Error())
		return nil
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *RestoreReconciler) restoreJob(rs *v1alpha1.Restore, p *v1alpha1.BackupPolicy, store *v1alpha1.BackupStore) (*batchv1.Job, error) {
	from := rs.Spec.From
	if from == "" {
		from = "latest"
	}
	pod, err := moverPod{
		mode:        jobcfg.ModeRestore,
		namespace:   p.Namespace,
		policy:      p.Name,
		spec:        p.Spec,
		store:       store,
		runnerImage: r.Image,
		sourcePVC:   rs.Spec.TargetPVC,
		restore: &jobcfg.Restore{
			From:      from,
			Paths:     rs.Spec.Paths,
			Databases: rs.Spec.Databases,
			Delete:    rs.Spec.Delete,
		},
	}.podSpec()
	if err != nil {
		return nil, err
	}
	labels := jobLabels(p.Name, jobcfg.ModeRestore)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: restoreJobName(rs.Name), Namespace: rs.Namespace, Labels: labels},
		Spec:       jobSpec(labels, pod, 0, rs.Spec.ActiveDeadlineSeconds),
	}
	return job, controllerutil.SetControllerReference(rs, job, r.Scheme)
}

// scalingUp puts workloads and autoscalers back, then ends the Restore: Succeeded only if the
// restore Job succeeded.
func (r *RestoreReconciler) scalingUp(ctx context.Context, rs *v1alpha1.Restore) error {
	if err := r.restoreHPAs(ctx, rs); err != nil {
		return err
	}
	for _, sc := range rs.Status.Scaled {
		err := r.updateWorkload(ctx, rs.Namespace, sc.WorkloadRef, func(w workload) bool { return w.scaleUp(rs.Name, sc.Replicas) })
		if err != nil {
			return err
		}
	}
	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: restoreJobName(rs.Name)}, &job)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	if _, ok := jobState(&job); err == nil && ok {
		r.finish(rs, phaseSucceeded)
		return nil
	}
	r.finish(rs, phaseFailed)
	return nil
}
