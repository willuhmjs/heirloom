package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	crtypes "sigs.k8s.io/controller-runtime/pkg/recorder"

	"github.com/willuhmjs/backshots/api/v1alpha1"
)

// emit records a Kubernetes Event when a recorder is configured (tests may leave it nil).
func emit(rec crtypes.EventRecorder, obj runtime.Object, eventType, reason, message string) {
	if rec != nil {
		// The message is user- or condition-derived, so pass it as an argument, not a format string.
		rec.Eventf(obj, nil, eventType, reason, "", "%s", message)
	}
}

// healthEvents tells `kubectl describe` readers when a policy's health changes: a Warning when it turns bad or its
// reason changes, a Normal event when it recovers. Steady states are not repeated.
func healthEvents(rec crtypes.EventRecorder, p *v1alpha1.BackupPolicy, before *metav1.Condition) {
	after := meta.FindStatusCondition(p.Status.Conditions, condHealthy)
	if after == nil {
		return
	}
	switch {
	case after.Status == metav1.ConditionFalse && (before == nil || before.Status != metav1.ConditionFalse || before.Reason != after.Reason):
		emit(rec, p, corev1.EventTypeWarning, after.Reason, after.Message)
	case after.Status == metav1.ConditionTrue && before != nil && before.Status == metav1.ConditionFalse:
		emit(rec, p, corev1.EventTypeNormal, "Recovered", "backups are healthy again: "+after.Message)
	}
}

// phaseEvent reports a Restore entering a new phase.
func phaseEvent(rec crtypes.EventRecorder, rs *v1alpha1.Restore) {
	switch rs.Status.Phase {
	case phaseScalingDown:
		emit(rec, rs, corev1.EventTypeNormal, "ScalingDown", "stopping the workloads before restoring")
	case phaseRunning:
		emit(rec, rs, corev1.EventTypeNormal, "Started", "restore started")
	case phaseScalingUp:
		emit(rec, rs, corev1.EventTypeNormal, "ScalingUp", "putting the workloads back")
	case phaseSucceeded:
		emit(rec, rs, corev1.EventTypeNormal, "Succeeded", "restored snapshot "+rs.Status.Snapshot)
	case phaseFailed:
		emit(rec, rs, corev1.EventTypeWarning, "Failed", rs.Status.Message)
	}
}
