package controller

import (
	"context"
	"strconv"
	"time"

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/willuhmjs/backshots/api/v1alpha1"
	"github.com/willuhmjs/backshots/internal/jobcfg"
)

// historyLimit is how many finished Jobs of each outcome (succeeded, failed) the controller keeps per role.
// Status is derived from them, so they must outlive the last run.
const historyLimit = 3

// parseSchedule parses a cron expression in the policy's time zone (UTC when tz is empty).
func parseSchedule(expr, tz string) (cron.Schedule, error) {
	if tz != "" {
		expr = "CRON_TZ=" + tz + " " + expr
	}
	return cron.ParseStandard(expr)
}

// nextSlot is the first scheduled time after the last run of a schedule, or after the policy was created if
// it never ran. A slot that has passed is due. A long outage therefore causes one catch-up run, not a burst.
func nextSlot(expr, tz string, last *metav1.Time, created time.Time) (time.Time, bool) {
	sched, err := parseSchedule(expr, tz)
	if err != nil {
		return time.Time{}, false
	}
	base := created
	if last != nil {
		base = last.Time
	}
	return sched.Next(base), true
}

// hasActive reports whether a Job of the role is still running (not finished, not being deleted).
func hasActive(jobs []batchv1.Job, role jobcfg.Mode) bool {
	for i := range jobs {
		if jobs[i].Labels[jobcfg.LabelRole] != string(role) || !jobs[i].DeletionTimestamp.IsZero() {
			continue
		}
		if done, _ := jobState(&jobs[i]); !done {
			return true
		}
	}
	return false
}

// restoreActive reports whether a Restore for the policy has not finished. While one is, the policy starts no
// backup or verify: a backup would snapshot a half-restored volume and become the newest snapshot.
func (r *PolicyReconciler) restoreActive(ctx context.Context, p *v1alpha1.BackupPolicy) (string, error) {
	var list v1alpha1.RestoreList
	if err := r.List(ctx, &list, client.InNamespace(p.Namespace)); err != nil {
		return "", err
	}
	for i := range list.Items {
		rs := &list.Items[i]
		if rs.Spec.PolicyRef != p.Name {
			continue
		}
		switch rs.Status.Phase {
		case phaseSucceeded, phaseFailed:
		default:
			return rs.Name, nil
		}
	}
	return "", nil
}

// startDue creates the backup and verify Jobs whose schedule slot has passed and records when it did. It
// never starts a second Job of a role while one is active (what CronJob's concurrencyPolicy: Forbid did).
// For the restic engine the two roles exclude each other outright: forget+prune takes an exclusive lock
// and rewrites pack files, which is exactly what a verify reading the same repository must not race.
func (r *PolicyReconciler) startDue(ctx context.Context, p *v1alpha1.BackupPolicy, store *v1alpha1.BackupStore, jobs []batchv1.Job, now time.Time) error {
	created := p.CreationTimestamp.Time
	exclusive := p.Spec.Engine == v1alpha1.EngineRestic
	activeBackup := hasActive(jobs, jobcfg.ModeBackup)
	activeVerify := hasActive(jobs, jobcfg.ModeVerify)
	if slot, ok := nextSlot(p.Spec.Schedule, p.Spec.TimeZone, p.Status.LastScheduleTime, created); ok && !slot.After(now) && !activeBackup && !(exclusive && activeVerify) {
		if err := r.createJob(ctx, p, store, jobcfg.ModeBackup, scheduledJobName(p.Name, jobcfg.ModeBackup, slot)); err != nil {
			return err
		}
		p.Status.LastScheduleTime = metaTime(now)
	}
	if p.Spec.Verify == nil {
		return nil
	}
	if slot, ok := nextSlot(p.Spec.Verify.Schedule, p.Spec.TimeZone, p.Status.LastVerifyScheduleTime, created); ok && !slot.After(now) && !activeVerify && !(exclusive && activeBackup) {
		if err := r.createJob(ctx, p, store, jobcfg.ModeVerify, scheduledJobName(p.Name, jobcfg.ModeVerify, slot)); err != nil {
			return err
		}
		p.Status.LastVerifyScheduleTime = metaTime(now)
	}
	return nil
}

func (r *PolicyReconciler) createJob(ctx context.Context, p *v1alpha1.BackupPolicy, store *v1alpha1.BackupStore, mode jobcfg.Mode, name string) error {
	job, err := newJob(r.Scheme, r.Image, p, store, mode, name)
	if err != nil {
		return err
	}
	return client.IgnoreAlreadyExists(r.Create(ctx, job))
}

// triggerManual starts one backup Job when spec.trigger.manual holds a value that was not run yet and no
// backup is running (and, for restic, no verify that the run's prune could pull the packs out from).
func (r *PolicyReconciler) triggerManual(ctx context.Context, p *v1alpha1.BackupPolicy, store *v1alpha1.BackupStore, jobs []batchv1.Job) error {
	value := p.Spec.Trigger.Manual
	if value == "" || value == p.Status.LastManualSync || hasActive(jobs, jobcfg.ModeBackup) ||
		(p.Spec.Engine == v1alpha1.EngineRestic && hasActive(jobs, jobcfg.ModeVerify)) {
		return nil
	}
	job, err := newJob(r.Scheme, r.Image, p, store, jobcfg.ModeBackup, manualJobName(p.Name, value, p.Generation))
	if err != nil {
		return err
	}
	job.Annotations = map[string]string{annotationManual: value, annotationManualGen: strconv.FormatInt(p.Generation, 10)}
	return client.IgnoreAlreadyExists(r.Create(ctx, job))
}

// pruneHistory deletes finished Jobs beyond historyLimit per role and outcome, oldest first. The pods go with
// them (background propagation).
func (r *PolicyReconciler) pruneHistory(ctx context.Context, jobs []batchv1.Job) error {
	for _, role := range []jobcfg.Mode{jobcfg.ModeBackup, jobcfg.ModeVerify} {
		var succeeded, failed []*batchv1.Job
		for _, run := range finishedRuns(jobs, nil, role) { // oldest first
			if run.ok {
				succeeded = append(succeeded, run.job)
			} else {
				failed = append(failed, run.job)
			}
		}
		for _, set := range [][]*batchv1.Job{succeeded, failed} {
			for len(set) > historyLimit {
				err := r.Delete(ctx, set[0], client.PropagationPolicy(metav1.DeletePropagationBackground))
				if err != nil && !apierrors.IsNotFound(err) {
					return err
				}
				set = set[1:]
			}
		}
	}
	return nil
}

// requeueIn is how long until the controller must look at the policy again with no other event: the nearest
// of the next backup slot, the next verify slot and the moment it turns stale.
func requeueIn(p *v1alpha1.BackupPolicy, now time.Time) time.Duration {
	var soonest time.Time
	consider := func(t time.Time, ok bool) {
		if ok && t.After(now) && (soonest.IsZero() || t.Before(soonest)) {
			soonest = t
		}
	}
	if !p.Spec.Suspend {
		consider(nextSlot(p.Spec.Schedule, p.Spec.TimeZone, p.Status.LastScheduleTime, p.CreationTimestamp.Time))
		if p.Spec.Verify != nil {
			consider(nextSlot(p.Spec.Verify.Schedule, p.Spec.TimeZone, p.Status.LastVerifyScheduleTime, p.CreationTimestamp.Time))
		}
	}
	consider(staleAt(p))
	if soonest.IsZero() {
		return 0
	}
	return soonest.Sub(now) + time.Second
}
