package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/willuhmjs/heirloom/api/v1"
	"github.com/willuhmjs/heirloom/internal/jobcfg"
)

const (
	annotationManual    = "heirloom.io/manual"
	annotationManualGen = "heirloom.io/manual-generation"
)

// scheduledJobName names the Job of one schedule slot. It is derived from the slot (not from the time the
// controller got round to it), so creating it twice is harmless: the second Create gets AlreadyExists.
func scheduledJobName(policy string, mode jobcfg.Mode, slot time.Time) string {
	kind := "run"
	if mode == jobcfg.ModeVerify {
		kind = "verify"
	}
	return fmt.Sprintf("bs-%s-%s-%d", policy, kind, slot.Unix())
}

// manualJobName includes the policy generation, so setting a trigger value that was used before (after another
// value) runs again instead of colliding with the old Job, while a failed run is not retried in a loop: within one
// generation the name, and so the Job, is the same.
func manualJobName(policy, value string, generation int64) string {
	return fmt.Sprintf("bs-%s-manual-%s", policy, shortHash(fmt.Sprintf("%s@%d", value, generation)))
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// newJob renders a backup or verify Job of a policy, owned by the policy.
func newJob(scheme *runtime.Scheme, runnerImage string, p *v1.BackupPolicy, store *v1.BackupStore, mode jobcfg.Mode, name string) (*batchv1.Job, error) {
	pod, err := moverPod{
		mode:        mode,
		namespace:   p.Namespace,
		policy:      p.Name,
		spec:        p.Spec,
		store:       store,
		runnerImage: runnerImage,
	}.podSpec()
	if err != nil {
		return nil, err
	}
	labels := jobLabels(p.Name, mode)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.Namespace, Labels: labels},
		Spec:       jobSpec(labels, pod, 1, p.Spec.ActiveDeadlineSeconds),
	}
	if err := controllerutil.SetControllerReference(p, job, scheme); err != nil {
		return nil, err
	}
	return job, nil
}
